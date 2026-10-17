#!/usr/bin/env python3
"""
Does a TX burst kill RX on the candleLight? Isolates the PiperInit failure.

Raw GsUsb, single thread, no shim — so nothing but the burst is a variable.
Reads a baseline, fires N frames back-to-back the way PiperInit does, then
reads again. If RX collapses right after the burst we have the mechanism in
one process instead of guessing across process restarts.
"""
import sys
import time

import usb.core
from gs_usb.gs_usb import GsUsb
from gs_usb.gs_usb_frame import GsUsbFrame, GS_USB_NONE_ECHO_ID

BITRATE = 1_000_000


def drain(g, seconds, label):
    """Count real (non-echo) frames arriving over `seconds`."""
    f, n, echo = GsUsbFrame(), 0, 0
    t0 = time.time()
    while time.time() - t0 < seconds:
        try:
            if g.read(f, 100):
                if f.echo_id == GS_USB_NONE_ECHO_ID:
                    n += 1
                else:
                    echo += 1
        except OSError:
            pass
    rate = n / seconds
    print(f"   {label}: {n} frames ({rate:.0f}/s), {echo} echoes")
    return n


def burst(g, count, gap=0.0):
    """Send `count` frames as PiperInit does (0x472 motor queries + 0x4AF)."""
    sent, failed = 0, 0
    for i in range(count):
        # 0x472 = motor limit query, 0x4AF = firmware version query
        if i < count - 1:
            can_id, data = 0x472, [(i % 6) + 1, 0x01, 0, 0, 0, 0, 0, 0]
        else:
            can_id, data = 0x4AF, [0x01, 0, 0, 0, 0, 0, 0, 0]
        try:
            g.send(GsUsbFrame(can_id=can_id, data=data))
            sent += 1
        except Exception as e:
            failed += 1
            print(f"   send #{i} FAILED: {type(e).__name__}: {e}")
        if gap:
            time.sleep(gap)
    print(f"   burst: {sent} sent, {failed} failed")


def open_bus():
    devs = GsUsb.scan()
    if not devs:
        sys.exit("no adapter")
    g = devs[0]
    if not g.set_bitrate(BITRATE):
        sys.exit("bitrate failed")
    g.start()
    return g


def main():
    n = int(sys.argv[1]) if len(sys.argv) > 1 else 13
    gap = float(sys.argv[2]) if len(sys.argv) > 2 else 0.0
    print(f"TX burst test: {n} frames, {gap*1000:.0f} ms gap between sends\n")

    g = open_bus()
    base = drain(g, 2.0, "baseline  ")

    print(f"\n-- firing {n} frames --")
    burst(g, n, gap)

    after = drain(g, 2.0, "after burst")
    later = drain(g, 2.0, "2 s later ")

    if base > 100 and after == 0:
        print("\n>>> RX DIED on the burst (deaf in-process).")
        print("    Trying stop()+start() to see if it recovers in-process...")
        g.stop()
        time.sleep(0.2)
        g.set_bitrate(BITRATE)
        g.start()
        rec = drain(g, 2.0, "after re-init")
        print("    RECOVERED" if rec > 100 else "    STILL DEAF — needs replug")
    elif after < base * 0.5:
        print(f"\n>>> RX DEGRADED ({base} -> {after}).")
    else:
        print(f"\n>>> RX SURVIVED the burst ({base} -> {after} -> {later}).")

    g.stop()


if __name__ == "__main__":
    main()
