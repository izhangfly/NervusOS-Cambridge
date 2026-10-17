#!/usr/bin/env python3
"""
Split the open bug: is it the shim's bus class, or piper_sdk's threading?

Exercises piper_mac_can's Bus on its own — single-threaded, no SDK, no read
thread. Compare against piper_can_check.py (raw gs_usb, known good).

    RX works, clean exit, adapter still healthy afterwards
        -> the shim's bus is fine; the bug is piper_sdk driving it from two
           threads. Verify the single-I/O-thread rewrite end to end.

    no RX, or the adapter wedges
        -> the bug is in the shim's own bus class. Try a longer read timeout in
           _io_loop (currently 2 ms, try 50 ms), then look for stray open handles.

Run piper_can_check.py immediately after this to see whether the adapter survived.

    python piper_bus_probe.py
"""

import sys
import time

sys.path.insert(0, str(__import__("pathlib").Path(__file__).resolve().parent))
import piper_mac_can
piper_mac_can.install()

import can

PIPER_IDS = {0x2A1, 0x2A2, 0x2A3, 0x251, 0x252, 0x253, 0x254, 0x255, 0x256}


def main():
    print("opening bus through the shim...")
    bus = can.Bus(channel="candle")
    print(f"  ok: {bus.channel_info}")

    print("listening 3 s...")
    n, ids = 0, {}
    t0 = time.time()
    while time.time() - t0 < 3.0:
        m = bus.recv(timeout=0.2)
        if m:
            n += 1
            ids[m.arbitration_id] = ids.get(m.arbitration_id, 0) + 1

    print(f"  {n} frames, {len(ids)} distinct IDs")
    for i in sorted(ids)[:10]:
        print(f"    0x{i:03X} x{ids[i]}")

    io = getattr(bus, "_io", None)
    print(f"  I/O thread alive: {io.is_alive() if io else '?'}   error: {bus._dead}")

    print("shutting down...")
    bus.shutdown()
    print("  clean")

    print()
    if n == 0:
        print("VERDICT: no RX through the shim's own bus.")
        print("  The bug is in piper_mac_can's bus class, not piper_sdk threading.")
        print("  Next: raise the _io_loop read timeout from 2 ms to 50 ms.")
    elif any(i in PIPER_IDS for i in ids):
        print("VERDICT: the shim's bus receives correctly.")
        print("  The bug is piper_sdk driving it from two threads.")
        print("  Next: run piper_arm_state.py and see if the single-I/O-thread fix holds.")
    else:
        print("VERDICT: traffic, but no PiPER IDs — wrong bitrate or wrong bus.")
    print("\nNow run piper_can_check.py to confirm the adapter is still healthy.")


if __name__ == "__main__":
    main()
