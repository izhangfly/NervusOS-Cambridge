#!/usr/bin/env python3
"""Open bus, SEND a frame (like PiperInit's queries), read, exit — cross-proc."""
import sys
import time

sys.path.insert(0, str(__import__("pathlib").Path(__file__).resolve().parent))
import piper_mac_can
piper_mac_can.install()

import can

bus = can.Bus(channel="candle")
time.sleep(0.5)

# Mimic PiperInit: send a query-like frame on the arm's command channel
msg = can.Message(arbitration_id=0x101, data=[0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00], is_extended_id=False)
bus.send(msg)

n = 0
t0 = time.time()
while time.time() - t0 < 2.0:
    m = bus.recv(timeout=0.1)
    if m:
        n += 1
print("frames in 2s:", n)
# no shutdown; atexit
