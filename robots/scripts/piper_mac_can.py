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


def _make_bus_class():
    import can
    from gs_usb.gs_usb import GsUsb
    from gs_usb.gs_usb_frame import GsUsbFrame, GS_USB_NONE_ECHO_ID

    class GsUsbRawBus(can.BusABC):
        """python-can Bus whose device is touched by one thread only."""

        def __init__(self, channel=None, bitrate=BITRATE, index=0, **kwargs):
            self.dev = self._open(index, bitrate)
            self.channel_info = f"gs_usb[{index}] @ {bitrate // 1000} kbit/s"
            self._can_protocol = can.CanProtocol.CAN_20
            self._is_shutdown = False

            self._rx = queue.Queue(maxsize=20000)
            self._tx = queue.Queue(maxsize=2000)
            self._stopping = threading.Event()
            self._dead = None          # exception that killed the I/O thread
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

        def _open(self, index, bitrate):
            dev = self._pick(index)
            try:
                if not dev.set_bitrate(bitrate):
                    raise can.CanInitializationError(f"could not set bitrate {bitrate}")
                dev.start(0)  # flags=0: HW timestamps break RX on macOS
            except can.CanError:
                raise
            except Exception as e:
                raise can.CanInitializationError(
                    f"could not open the CAN stick ({type(e).__name__}: {e}).\n"
                    "  If this says 'Pipe error' or 'no langid', the adapter is wedged "
                    "from an earlier run — unplug it and plug it back in."
                ) from e
            return dev

        def _io_loop(self):
            """The only thread that touches libusb. Drains TX, then polls RX."""
            frame = GsUsbFrame()
            while not self._stopping.is_set():
                try:
                    sent = 0
                    while sent < 16:
                        try:
                            self.dev.send(self._tx.get_nowait())
                            sent += 1
                        except queue.Empty:
                            break

                    # Short timeout so queued commands are never held up for long.
                    if self.dev.read(frame, 2):
                        # echo_id marks the adapter echoing our own transmission;
                        # only genuine bus traffic belongs upstream.
                        if frame.echo_id == GS_USB_NONE_ECHO_ID:
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
                    if not self._stopping.is_set():
                        self._dead = e
                    return

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
            try:
                self.dev.stop()
            except Exception:
                pass
            try:
                _buses.remove(self)
            except ValueError:
                pass

    return GsUsbRawBus


def install(bitrate=BITRATE, index=0):
    """Rewrite can.interface.Bus so piper_sdk's socketcan request lands on gs_usb."""
    global _installed
    if _installed:
        return
    _patch_darwin_usb()
    atexit.register(_shutdown_all)

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
