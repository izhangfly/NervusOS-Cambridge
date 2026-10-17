#!/usr/bin/env python3
"""Open+close the shim bus 3x IN ONE PROCESS, counting frames each time."""
import sys
import time

sys.path.insert(0, str(__import__("pathlib").Path(__file__).resolve().parent))
import piper_mac_can
piper_mac_can.install()

import can

for i in range(3):
    bus = can.Bus(channel="candle")
    time.sleep(0.5)
    n = 0
    t0 = time.time()
    while time.time() - t0 < 2.0:
        m = bus.recv(timeout=0.1)
        if m:
            n += 1
    print(f"open #{i}: frames in 2s = {n}, I/O dead = {bus._dead}")
    bus.shutdown()
    time.sleep(0.3)
print("done")
