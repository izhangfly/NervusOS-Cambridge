#!/usr/bin/env python3
"""Watch the CAN link over time -- catches an INTERMITTENT connection.

One-shot checks lie when the link comes and goes: run piper_can_check.py twice
and you get "9135 frames" then "SILENT BUS", which looks like a software bug and
is not. This opens the adapter once and prints frames/s every second, so a
cable that drops in and out is visible as it happens.

    python piper_link_monitor.py [seconds]

Reading it:
    steady ~2000+/s        healthy link
    flips between N and 0  INTERMITTENT -- connector, cable, or power
    0 for the whole run    nothing transmitting (power, wiring, e-stop)

Wiggle the CAN connector at the arm and at the stick while this runs. If the
count reacts, you have found the fault.
"""
import sys
import time

import usb.core
import usb.util
from gs_usb.gs_usb import GsUsb

BITRATE = 1000000
PIPER_IDS = {0x2A1, 0x2A2, 0x2A3, 0x2A4, 0x2A5, 0x2A6, 0x2A7, 0x2A8,
             0x251, 0x252, 0x253, 0x254, 0x255, 0x256}


def main():
    seconds = int(sys.argv[1]) if len(sys.argv) > 1 else 30

    devs = GsUsb.scan()
    if not devs:
        print("no candleLight adapter found.")
        return 1
    dev = devs[0]
    try:
        print(f"adapter serial={dev.serial_number}")
    except Exception:
        print("adapter wedged (will not answer) -- unplug and replug it.")
        return 1

    if not dev.set_bitrate(BITRATE):
        print("set_bitrate failed")
        return 1
    dev.start(0)          # 20-byte frames; matches this firmware

    print(f"watching {seconds}s at 1 Mbit/s — wiggle the CAN connectors now\n")
    print("  time   frames/s   piper/s   state")

    from gs_usb.gs_usb_frame import GsUsbFrame
    f = GsUsbFrame()
    drops, ups, prev_live = 0, 0, None
    t_start = time.time()

    try:
        while time.time() - t_start < seconds:
            n = piper = 0
            t_sec = time.time()
            while time.time() - t_sec < 1.0:
                try:
                    if dev.read(f, 20):
                        n += 1
                        if f.can_id in PIPER_IDS:
                            piper += 1
                except Exception:
                    pass
            live = n > 0
            if prev_live is not None and live != prev_live:
                drops += (not live)
                ups += live
                mark = "  <-- LINK LOST" if not live else "  <-- LINK BACK"
            else:
                mark = ""
            prev_live = live
            print(f"  {time.time() - t_start:5.0f}s  {n:8}  {piper:8}   "
                  f"{'LIVE' if live else 'dead'}{mark}")
    except KeyboardInterrupt:
        pass
    finally:
        try:
            dev.stop()
        except Exception:
            pass
        usb.util.dispose_resources(dev.gs_usb)

    print(f"\n=== {drops} dropout(s), {ups} recovery(ies) ===")
    if drops or ups:
        print("  INTERMITTENT LINK CONFIRMED. This is physical, not software:")
        print("    * CAN connector not fully latched at the arm or the stick")
        print("    * cable strained (did mounting the camera pull on it?)")
        print("    * marginal 24 V supply browning the arm out")
        print("  Reseat both ends and re-run; a steady LIVE column is the goal.")
    elif prev_live:
        print("  Link steady. Good to record.")
    else:
        print("  Never saw a frame. Arm power, wiring, or e-stop.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
