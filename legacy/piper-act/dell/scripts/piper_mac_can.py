"""
macOS CAN shim for piper_sdk.

piper_sdk hardcodes bustype="socketcan" (see C_PiperInterface_V2.__init__), which
does not exist on macOS. The arm here is reached through a candleLight USB-CAN
stick (bytewerk, 1d50:606f). Import this module BEFORE constructing
C_PiperInterface_V2 and it rewrites the bus factory onto that stick.

Three macOS-specific problems are solved here, all found the hard way:

1. python-can's own `gs_usb` backend opens and transmits but NEVER receives on
   macOS. It configures the device with set_timing() + start(HW_TIMESTAMP), and
   frames stop arriving — so the SDK sees a flooded bus as total silence and
   reports every joint as 0.0. Driving the gs_usb library directly with
   set_bitrate() + start(0) receives fine, so this wraps that instead.

2. libusb on Darwin reports a kernel driver it cannot detach, so gs_usb.start()
   raises "[Errno 13] Access denied". The claim succeeds regardless.

3. THE IMPORTANT ONE. piper_sdk reads from a background thread while commands go
   out from the caller's thread. Two threads issuing concurrent libusb transfers
   on one device handle does not work on macOS: reads return nothing, writes fail
   with SEND_MESSAGE_FAILED, and the process can segfault on exit. So exactly one
   thread here ever touches the device, and everything else goes through queues.
   This is why a single-threaded sniff of the same bus succeeds while the SDK
   sees a dead bus.

Never call usb reset() on this adapter. It comes back enumerated but deaf, stops
answering string-descriptor reads ("The device has no langid"), and only a
physical replug clears it.

Usage:
    import piper_mac_can; piper_mac_can.install()
    piper = C_PiperInterface_V2("candle", judge_flag=False)
"""

import atexit
import platform
import queue
import struct
import threading
import time

BITRATE = 1_000_000
GS_USB_VID, GS_USB_PID = 0x1D50, 0x606F

_installed = False
_buses = []


def _patch_darwin_usb():
    """macOS libusb cannot detach kernel drivers; claiming works anyway."""
    if platform.system() != "Darwin":
        return
    import usb.core
    usb.core.Device.is_kernel_driver_active = lambda self, iface: False


def find_adapters():
    """Return [(bus, address, serial)] for every candleLight stick present."""
    import usb.core
    out = []
    for d in usb.core.find(find_all=True, idVendor=GS_USB_VID, idProduct=GS_USB_PID):
        try:
            serial = d.serial_number
        except Exception:
            serial = None  # wedged: enumerates but will not answer
        out.append((d.bus, d.address, serial))
    return out


def _shutdown_all(*_a):
    for bus in list(_buses):
        try:
            bus.shutdown()
        except Exception:
            pass


# A bulk-OUT write that times out is transient — the adapter's TX buffers are
# momentarily full — so the frame is retried instead of thrown away. After this
# many consecutive failures on the same frame the endpoint is treated as stalled:
# the halt is cleared and the frame dropped, so a jammed TX never blocks RX.
TX_RETRY_LIMIT = 5
TX_TIMEOUT_MS = 30   # pyusb's 1000 ms default would stall the control loop
RX_TIMEOUT_MS = 100  # 2 ms thrashed libusb0; reads return at once when busy
TX_OPEN_ATTEMPTS = 4  # re-opens allowed to clear a stalled endpoint at startup
RX_BURST = 64         # frames drained per lap before TX gets a turn


def _make_bus_class():
    import can
    import usb.core
    import usb.util
    from gs_usb.constants import GS_CAN_MODE_HW_TIMESTAMP
    from gs_usb.gs_usb import GsUsb
    from gs_usb.gs_usb_frame import GsUsbFrame, GS_USB_NONE_ECHO_ID

    class GsUsbRawBus(can.BusABC):
        """python-can Bus whose device is touched by one thread only."""

        def __init__(self, channel=None, bitrate=BITRATE, index=0, **kwargs):
            self._index = index
            self._bitrate = bitrate
            self.dev = self._open(index, bitrate)
            self.channel_info = f"gs_usb[{index}] @ {bitrate // 1000} kbit/s"
            self._can_protocol = can.CanProtocol.CAN_20
            self._is_shutdown = False

            self._rx = queue.Queue(maxsize=20000)
            self._tx = queue.Queue(maxsize=2000)
            self._stopping = threading.Event()
            self._dead = None          # exception that killed the I/O thread
            # TX frame size depends on whether the device negotiated HW stamps.
            self._hw_ts = bool(
                (self.dev.device_flags or 0) & GS_CAN_MODE_HW_TIMESTAMP)
            # Counters, so a caller can tell "the stick delivered nothing" from
            # "the stick delivered frames but the SDK made nothing of them".
            self.rx_count = 0
            self.tx_count = 0
            self.echo_count = 0
            self.tx_fail_count = 0   # bulk-OUT timeouts (retried)
            self.tx_drop_count = 0   # frames given up on after TX_RETRY_LIMIT
            self.stall_recovery_count = 0
            self.error_count = 0
            self.last_error = None
            self._io = threading.Thread(target=self._io_loop, name="gs_usb-io", daemon=True)
            self._io.start()

            _buses.append(self)
            super().__init__(channel=channel or "candle", **kwargs)

        @staticmethod
        def _pick(index):
            devs = GsUsb.scan()
            if not devs:
                raise can.CanInitializationError(
                    "no candleLight adapter found — is the CAN stick plugged in?")
            if index >= len(devs):
                raise can.CanInitializationError(
                    f"adapter index {index} out of range ({len(devs)} present)")
            return devs[index]

        @staticmethod
        def _drain(dev, seconds):
            """Read out whatever the adapter still has queued.

            The candleLight shares one small buffer pool between frames it has
            received and the TX echoes it owes the host. Leave that pool full —
            as a process does when it exits mid-flood — and the firmware stops
            accepting writes: RX keeps streaming at full rate while every
            bulk-OUT times out, so the arm reports pose but ignores commands.
            Reading the backlog hands those buffers back and TX recovers.
            """
            f = GsUsbFrame()
            n = 0
            t0 = time.time()
            while time.time() - t0 < seconds:
                try:
                    if not dev.read(f, 20):
                        break
                # gs_usb.read() unpacks OUTSIDE its own USBError handler, so a
                # stalled or short bulk-IN raises struct.error -- which is not an
                # OSError and would otherwise escape the reopen loop below, the
                # very code written to recover this. Treat it as "no frame".
                except (OSError, usb.core.USBError, struct.error):
                    break
                n += 1
            return n

        @staticmethod
        def _tx_probe(dev):
            """Does the OUT endpoint accept a write, or is it stalled?

            Sends the arm's firmware-version query (0x4AF) — harmless, and the
            SDK itself sends it on every connect.
            """
            hw = bool((dev.device_flags or 0) & GS_CAN_MODE_HW_TIMESTAMP)
            probe = GsUsbFrame(can_id=0x4AF, data=[0x01, 0, 0, 0, 0, 0, 0, 0])
            for _ in range(2):
                try:
                    dev.gs_usb.write(0x02, probe.pack(hw), TX_TIMEOUT_MS)
                    return True
                except (OSError, usb.core.USBError):
                    time.sleep(0.05)
            return False

        @staticmethod
        def _release(dev):
            """Hand the device back so the next scan() gets a clean handle."""
            try:
                dev.stop()
            except Exception:
                pass
            try:
                usb.util.dispose_resources(dev.gs_usb)
            except Exception:
                pass

        def _open(self, index, bitrate):
            """Open the stick, and hand back a handle whose TX actually works.

            A previous process can leave the bulk-OUT endpoint stalled: RX keeps
            streaming perfectly while every write times out, so the arm reports
            its pose but silently ignores every command. Reusing the handle does
            not clear it — only a fresh one does — so probe TX here and re-open
            until the adapter answers.
            """
            last = None
            for attempt in range(TX_OPEN_ATTEMPTS):
                try:
                    dev = self._pick(index)
                    if not dev.set_bitrate(bitrate):
                        raise can.CanInitializationError(
                            f"could not set bitrate {bitrate}")
                    # flags=0, NOT gs_usb's default. gs_usb sizes every bulk-IN
                    # read from the negotiated HW-timestamp flag, but this
                    # candleLight delivers plain 20-byte frames even though it
                    # advertises timestamps (device_flags=0x10). Asking for 24
                    # makes every read short -> struct.error -> RX dies after a
                    # few frames and the arm appears to "stop sending".
                    # Measured: start(0) sustains 2130 frames/s for 25 s with
                    # zero errors; start() yields 11 frames then silence.
                    dev.start(0)
                except can.CanInitializationError:
                    raise
                except Exception as e:
                    last = f"{type(e).__name__}: {e}"
                    time.sleep(0.2)
                    continue

                # Free the firmware's buffers before asking it to transmit; a
                # backlog left by the previous process is what blocks TX.
                self._drain(dev, 0.4)

                if self._tx_probe(dev):
                    if attempt:
                        print(f"[piper_mac_can] cleared a stalled TX endpoint "
                              f"after {attempt} reopen(s)")
                    return dev

                last = "bulk-OUT endpoint stalled (writes time out)"
                self._release(dev)
                time.sleep(0.25)

            raise can.CanInitializationError(
                f"could not get a working CAN stick after {TX_OPEN_ATTEMPTS} "
                f"attempts — last: {last}.\n"
                "  If this persists, unplug the CAN stick and plug it back in."
            )

        def _write(self, frame):
            """One bulk-OUT with a short timeout, bypassing GsUsb.send().

            GsUsb.send() uses pyusb's 1000 ms default, so a single stalled write
            would freeze RX for a full second — fatal for a 20 Hz jog loop.
            """
            self.dev.gs_usb.write(0x02, frame.pack(self._hw_ts), TX_TIMEOUT_MS)

        def _clear_tx_stall(self):
            """Recover a stalled OUT endpoint.

            The candleLight's bulk-OUT endpoint stalls under this workload:
            writes time out forever while RX keeps streaming perfectly, so the
            arm stops taking commands but still reports pose. libusb0 has no
            clear_halt (pyusb raises AttributeError), and a stop()+start()
            cycle is what measurably brings TX back — see piper_tx_recover.py.
            """
            self.stall_recovery_count += 1
            try:
                self._release(self.dev)
                time.sleep(0.15)
                self.dev = self._open(self._index, self._bitrate)
                self._hw_ts = bool(
                    (self.dev.device_flags or 0) & GS_CAN_MODE_HW_TIMESTAMP)
            except Exception as e:
                self.last_error = e

        def _io_loop(self):
            """The only thread that touches libusb. Drains TX, then polls RX.

            Nothing short of shutdown may end this loop. A USB timeout on either
            direction is transient (libusb0 reports it as a bare OSError, WinError
            10060, not usb.core.USBError), and letting one escape used to kill the
            thread and leave the SDK staring at a permanently dead bus.
            """
            frame = GsUsbFrame()
            pending = None      # frame whose write failed, retried next lap
            tx_fails = 0
            while not self._stopping.is_set():
                try:
                    # ---- TX: at most a few per lap, so RX keeps draining ----
                    sent = 0
                    while sent < 8:
                        if pending is None:
                            try:
                                pending = self._tx.get_nowait()
                            except queue.Empty:
                                break
                        try:
                            self._write(pending)
                        except (OSError, usb.core.USBError):
                            tx_fails += 1
                            self.tx_fail_count += 1
                            if tx_fails >= TX_RETRY_LIMIT:
                                # Endpoint looks genuinely stalled. Clear the
                                # halt and drop this frame: a control stream is
                                # better off skipping a stale setpoint than
                                # wedging behind it.
                                self._clear_tx_stall()
                                pending = None
                                tx_fails = 0
                                self.tx_drop_count += 1
                            break  # let RX run before trying again
                        pending = None
                        tx_fails = 0
                        self.tx_count += 1
                        sent += 1

                    # ---- RX: drain hard, not one frame per lap ----
                    # The arm floods ~2300 frames/s. Reading a single frame per
                    # lap falls behind, the adapter's buffer pool fills, and a
                    # full pool is exactly what stalls its TX endpoint. Keep
                    # reading until it has nothing left, then give TX a turn.
                    # On an idle bus the first read simply times out and breaks.
                    for _ in range(RX_BURST):
                        try:
                            got = self.dev.read(frame, RX_TIMEOUT_MS)
                        except (OSError, usb.core.USBError, struct.error):
                            got = False
                        if not got:
                            break
                        # echo_id marks the adapter echoing our own transmission;
                        # only genuine bus traffic belongs upstream.
                        if frame.echo_id != GS_USB_NONE_ECHO_ID:
                            self.echo_count += 1
                            continue
                        self.rx_count += 1
                        msg = can.Message(
                            timestamp=time.time(),
                            arbitration_id=frame.arbitration_id,
                            is_extended_id=frame.is_extended_id,
                            is_remote_frame=frame.is_remote_frame,
                            is_error_frame=frame.is_error_frame,
                            dlc=frame.can_dlc,
                            data=bytearray(frame.data[:frame.can_dlc]),
                            channel=self.channel_info,
                        )
                        try:
                            self._rx.put_nowait(msg)
                        except queue.Full:
                            pass  # a stalled reader must not stall the bus
                except Exception as e:
                    # Truly unexpected (not a USB timeout). Record it, but keep
                    # the loop alive so the bus degrades instead of dying.
                    self.last_error = e
                    self.error_count += 1
                    if self.error_count > 1000:
                        self._dead = e
                        return
                    time.sleep(0.01)

        def _recv_internal(self, timeout):
            if self._dead is not None:
                raise can.CanOperationError(f"CAN I/O thread died: {self._dead}")
            try:
                return self._rx.get(timeout=0.05 if timeout is None else timeout), False
            except queue.Empty:
                return None, False

        def send(self, msg, timeout=None):
            if self._dead is not None:
                raise can.CanOperationError(f"CAN I/O thread died: {self._dead}")
            if self._is_shutdown:
                raise can.CanOperationError("bus is shut down")
            from gs_usb.constants import CAN_EFF_FLAG, CAN_ERR_FLAG, CAN_RTR_FLAG
            frame = GsUsbFrame(can_id=msg.arbitration_id, data=list(bytearray(msg.data))[:8])
            if msg.is_extended_id:
                frame.can_id |= CAN_EFF_FLAG
            if msg.is_remote_frame:
                frame.can_id |= CAN_RTR_FLAG
            if msg.is_error_frame:
                frame.can_id |= CAN_ERR_FLAG
            try:
                self._tx.put_nowait(frame)
            except queue.Full:
                raise can.CanOperationError("CAN transmit queue full")

        def shutdown(self):
            if self._is_shutdown:
                return
            self._is_shutdown = True
            self._stopping.set()
            # Let the I/O thread finish its in-flight transfer before stopping the
            # device underneath it — tearing it down mid-transfer segfaults.
            io = getattr(self, "_io", None)
            if io is not None and io.is_alive():
                io.join(timeout=2.0)
            # Drain whatever the adapter still has buffered before stopping it.
            # Leaving frames (and un-collected TX echoes) queued in the firmware
            # is what left the next process facing a stalled OUT endpoint.
            frame = GsUsbFrame()
            t0 = time.time()
            while time.time() - t0 < 0.3:
                try:
                    if not self.dev.read(frame, 20):
                        break
                except (OSError, usb.core.USBError, struct.error):
                    break
            try:
                self.dev.stop()
            except Exception:
                pass
            # No re-init cycle here on purpose. python-can's native backend does
            # one, but its start() issues a usb.reset() that leaves this adapter
            # with a stalled bulk-OUT endpoint for the NEXT process. The I/O loop
            # recovers a stall on its own, so the workaround costs more than it buys.
            try:
                _buses.remove(self)
            except ValueError:
                pass

    return GsUsbRawBus


def install(bitrate=BITRATE, index=0):
    """Rewrite can.interface.Bus so piper_sdk's socketcan request lands on gs_usb.

    Always uses the hand-rolled single-I/O-thread bus. It touches the adapter from
    exactly one thread (send and recv both go through the I/O thread), which is
    what avoids the libusb0 concurrent-transfer deadlock that python-can's native
    gs_usb backend hits on Windows during jogging.
    """
    global _installed
    if _installed:
        return
    _patch_darwin_usb()
    atexit.register(_shutdown_all)

    # The SDK's C_STD_CAN runs JudgeCanInfo() when built with the default
    # judge_flag=True, and that method confirms the CAN socket by reading
    # /sys/class/net — which cannot work off Linux. Code that constructs
    # C_PiperInterface_V2(port) with the default (the LeRobot Piper plugin does
    # exactly this) would otherwise raise "CAN socket <port> does not exist".
    # Neutralise the check the same way we rewrite can.interface.Bus below: the
    # candleLight shim owns the port on this machine.
    if platform.system() != "Linux":
        try:
            # piper_interface_v2 does `from ..hardware_port import *`, which
            # re-exports C_STD_CAN from can_encapsulation_v0_4_0 (not the
            # older can_encapsulation module). Patch through the same path the
            # SDK itself uses so the class object matches.
            from piper_sdk.hardware_port import C_STD_CAN
            C_STD_CAN.JudgeCanInfo = lambda self: None
        except Exception:
            pass

    import can

    bus_cls = _make_bus_class()

    def bus_factory(channel=None, bustype=None, interface=None, **kwargs):
        # Anything the SDK asks for on this machine is the USB stick.
        kwargs.pop("bitrate", None)
        kwargs.pop("can_filters", None)
        return bus_cls(channel=channel, bitrate=bitrate, index=index, **kwargs)

    can.interface.Bus = bus_factory
    can.Bus = bus_factory
    _installed = True
