#!/usr/bin/env python3
"""
Is the PiPER actually on the CAN bus? Run this before piper_cartesian_ctl.py.

Checks, in order: the USB stick is present, the gs_usb bus opens at 1 Mbit/s,
and the arm is transmitting. A healthy powered arm floods the bus (~2300
frames/s) with 0x2A1-0x2A8 pose feedback and 0x251-0x256 joint feedback, with
no input needed. Silence means wiring or power, not software.

    /Users/ianzhang/agilex/.venv/bin/python piper_can_check.py
"""

import platform
import sys
import time

if platform.system() == "Darwin":
    import usb.core
    # macOS libusb reports a kernel driver it cannot detach; the claim works anyway.
    usb.core.Device.is_kernel_driver_active = lambda self, iface: False

import usb.core
from gs_usb.gs_usb import GsUsb
from gs_usb.gs_usb_frame import GsUsbFrame

PIPER_IDS = {
    0x2A1: "end pose X/Y", 0x2A2: "end pose Z/RX", 0x2A3: "end pose RY/RZ",
    0x251: "joint 1 driver", 0x252: "joint 2 driver", 0x253: "joint 3 driver",
    0x254: "joint 4 driver", 0x255: "joint 5 driver", 0x256: "joint 6 driver",
}


def main():
    print("1. USB stick")
    devs = list(usb.core.find(find_all=True, idVendor=0x1D50, idProduct=0x606F))
    if not devs:
        sys.exit("   NOT FOUND — no candleLight (1d50:606f) adapter. Replug it.")
    for d in devs:
        try:
            serial = d.serial_number
        except Exception:
            # A wedged candleLight still enumerates but stops answering string
            # descriptor reads. Nothing in software recovers this.
            sys.exit(f"   WEDGED — adapter at bus={d.bus} addr={d.address} enumerates "
                     "but will not answer.\n"
                     "   Unplug the CAN stick and plug it back in, then re-run.")
        print(f"   ok  bus={d.bus} addr={d.address} serial={serial}")

    print("2. Open bus @ 1 Mbit/s")
    g = GsUsb.scan()[0]
    if not g.set_bitrate(1000000):
        sys.exit("   FAILED to set bitrate")
    g.start(0)
    print("   ok")

    print("3. Listen 4 s for the arm")
    f, ids, n = GsUsbFrame(), {}, 0
    t0 = time.time()
    while time.time() - t0 < 4.0:
        if g.read(f, 100):
            n += 1
            ids[f.can_id] = ids.get(f.can_id, 0) + 1
    g.stop()

    print(f"   {n} frames, {len(ids)} distinct IDs")
    for i in sorted(ids):
        print(f"     0x{i:03X} x{ids[i]:<5} {PIPER_IDS.get(i, '')}")

    if n == 0:
        print("\n   SILENT BUS. The adapter is fine; nothing is talking. Check:")
        print("     * arm powered on (24 V brick in, base LED lit)")
        print("     * CAN cable seated in BOTH the stick and the arm")
        print("     * CAN-H/CAN-L not swapped; 120 ohm termination present")
    elif not any(i in PIPER_IDS for i in ids):
        print("\n   Traffic present but no PiPER IDs — wrong device or bitrate.")
    else:
        print("\n   ARM IS ALIVE. piper_cartesian_ctl.py should connect.")


if __name__ == "__main__":
    main()
