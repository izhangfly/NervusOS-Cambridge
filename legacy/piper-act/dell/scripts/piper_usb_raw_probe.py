#!/usr/bin/env python3
"""Raw bulk-IN probe: distinguish 'adapter wedged' from 'arm silent'.

gs_usb.read() unpacks blindly, so a short/empty packet surfaces as a confusing
struct.error. This reads endpoint 0x81 directly and reports the ACTUAL byte
counts, which is what tells the two failure modes apart:

    all reads time out          -> adapter fine, nothing on the bus (arm off/wiring)
    reads return 0 bytes (ZLP)  -> adapter wedged, needs a physical replug
    reads return 20/24 bytes    -> traffic is flowing, the bug is above this layer
"""
import sys
import time

import usb.core
import usb.util
from gs_usb.gs_usb import GsUsb
from gs_usb.constants import GS_CAN_MODE_HW_TIMESTAMP

BITRATE = 1000000


def main():
    devs = GsUsb.scan()
    print(f"adapters found: {len(devs)}")
    if not devs:
        return 1
    dev = devs[0]
    print(f"  bus={dev.bus} addr={dev.address} serial={dev.serial_number}")

    if not dev.set_bitrate(BITRATE):
        print("  set_bitrate FAILED")
        return 1
    print(f"  bitrate set to {BITRATE}")

    for label, flags in (("flags=0 (no hw timestamps)", 0), ("default flags", None)):
        print(f"\n--- start({label}) ---")
        try:
            dev.start(0) if flags == 0 else dev.start()
        except Exception as e:
            print(f"  start raised {type(e).__name__}: {e}")
            continue

        hw = bool((dev.device_flags or 0) & GS_CAN_MODE_HW_TIMESTAMP)
        expect = 24 if hw else 20
        print(f"  device_flags=0x{dev.device_flags or 0:X} hw_timestamps={hw} "
              f"expected frame size={expect}")

        counts, timeouts, errors = {}, 0, 0
        t0 = time.time()
        while time.time() - t0 < 3.0:
            try:
                data = dev.gs_usb.read(0x81, expect, 100)
                counts[len(data)] = counts.get(len(data), 0) + 1
            except usb.core.USBError as e:
                if "timeout" in str(e).lower() or getattr(e, "errno", None) == 110:
                    timeouts += 1
                else:
                    errors += 1
                    if errors <= 2:
                        print(f"  USBError: {e}")

        print(f"  3s: timeouts={timeouts} other_errors={errors}")
        print(f"  byte-count histogram: {dict(sorted(counts.items()))}")

        total = sum(counts.values())
        if total == 0:
            print("  => NO DATA. Adapter is fine; nothing is transmitting.")
            print("     Check arm power (24V brick, base LED) and CAN wiring.")
        elif set(counts) == {0}:
            print("  => ZERO-LENGTH packets only. Adapter is WEDGED.")
            print("     Unplug the CAN stick and plug it back in.")
        elif expect in counts:
            print("  => REAL FRAMES flowing. Bus is healthy.")
        else:
            print("  => Short packets of unexpected size — firmware/protocol mismatch.")

        try:
            dev.stop()
        except Exception:
            pass

    usb.util.dispose_resources(dev.gs_usb)
    print("\nreleased device cleanly.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
