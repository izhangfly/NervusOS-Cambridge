#!/usr/bin/env python3
"""Like piper_bus_debug but NO explicit shutdown — rely on atexit only."""
import sys
import time

sys.path.insert(0, str(__import__("pathlib").Path(__file__).resolve().parent))
import piper_mac_can
piper_mac_can.install()

import can

bus = can.Bus(channel="candle")
time.sleep(1.0)
n = 0
t0 = time.time()
while time.time() - t0 < 2.0:
    m = bus.recv(timeout=0.1)
    if m:
        n += 1
print("frames in 2s:", n)
# no bus.shutdown() here — atexit _shutdown_all handles it
