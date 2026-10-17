#!/usr/bin/env python3
"""
The OUT endpoint stalls and bulk writes time out until the stick is replugged.
This tries each plausible software recovery in turn and reports which one
actually restores TX, so the shim can self-heal instead of needing a human.

Run it while TX is broken.
"""
import sys
import time

import usb.core
import usb.util
from gs_usb.gs_usb import GsUsb
from gs_usb.gs_usb_frame import GsUsbFrame, GS_USB_NONE_ECHO_ID
from gs_usb.constants import GS_CAN_MODE_HW_TIMESTAMP

BITRATE = 1_000_000
TX_TIMEOUT_MS = 50


def fresh():
    devs = GsUsb.scan()
    if not devs:
        sys.exit("no adapter found")
    return devs[0]


def try_tx(g, n=5, label=""):
    """Fire n frames; return how many the bulk-OUT accepted."""
    hw = bool((g.device_flags or 0) & GS_CAN_MODE_HW_TIMESTAMP)
    ok = 0
    for i in range(n):
        f = GsUsbFrame(can_id=0x4AF, data=[0x01, 0, 0, 0, 0, 0, 0, 0])
        try:
            g.gs_usb.write(0x02, f.pack(hw), TX_TIMEOUT_MS)
            ok += 1
        except Exception:
            pass
    if label:
        print(f"      TX {ok}/{n}")
    return ok


def try_rx(g, seconds=1.0):
    f, n = GsUsbFrame(), 0
    t0 = time.time()
    while time.time() - t0 < seconds:
        try:
            if g.read(f, 100) and f.echo_id == GS_USB_NONE_ECHO_ID:
                n += 1
        except OSError:
            pass
    return n


def attempt(name, fn):
    print(f"\n[{name}]")
    try:
        g = fn()
    except Exception as e:
        print(f"      setup failed: {type(e).__name__}: {e}")
        return False
    tx = try_tx(g, 5, label=name)
    rx = try_rx(g, 1.0)
    print(f"      RX {rx} frames/s")
    good = tx >= 4 and rx > 100
    print("      => RECOVERED" if good else "      => still broken")
    try:
        g.stop()
    except Exception:
        pass
    return good


def s_baseline():
    g = fresh()
    g.set_bitrate(BITRATE)
    g.start()
    return g


def s_clear_halt():
    g = fresh()
    g.set_bitrate(BITRATE)
    g.start()
    for ep in (0x02, 0x81):
        try:
            usb.util.clear_halt(g.gs_usb, ep)
        except Exception as e:
            print(f"      clear_halt({hex(ep)}): {type(e).__name__}")
    return g


def s_stop_start():
    g = fresh()
    g.set_bitrate(BITRATE)
    g.start()
    g.stop()
    time.sleep(0.2)
    g.set_bitrate(BITRATE)
    g.start()
    return g


def s_dispose():
    g = fresh()
    usb.util.dispose_resources(g.gs_usb)
    time.sleep(0.3)
    g = fresh()
    g.set_bitrate(BITRATE)
    g.start()
    return g


def s_set_config():
    g = fresh()
    try:
        g.gs_usb.set_configuration()
    except Exception as e:
        print(f"      set_configuration: {type(e).__name__}: {e}")
    g.set_bitrate(BITRATE)
    g.start()
    return g


def s_usb_reset():
    g = fresh()
    g.gs_usb.reset()          # the nuclear option the handoff warns about
    time.sleep(0.5)
    g = fresh()
    g.set_bitrate(BITRATE)
    g.start()
    return g


def main():
    print("Probing software recoveries for a stalled OUT endpoint.")
    for name, fn in [
        ("baseline (no recovery)", s_baseline),
        ("clear_halt on both endpoints", s_clear_halt),
        ("stop + start cycle", s_stop_start),
        ("dispose_resources + rescan", s_dispose),
        ("set_configuration", s_set_config),
        ("usb reset()", s_usb_reset),
    ]:
        if attempt(name, fn):
            print(f"\n*** '{name}' restores TX. Use it in the shim. ***")
            return
    print("\nNothing recovered it in software — a physical replug is required.")


if __name__ == "__main__":
    main()
