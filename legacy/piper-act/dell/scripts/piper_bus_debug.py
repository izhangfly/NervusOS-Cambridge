#!/usr/bin/env python3
"""Debug the hand-rolled shim bus directly: is the I/O thread alive? receiving?"""
import sys
import time

sys.path.insert(0, str(__import__("pathlib").Path(__file__).resolve().parent))
import piper_mac_can
piper_mac_can.install()

import can

bus = can.Bus(channel="candle")
print("channel_info:", bus.channel_info)
time.sleep(1.0)

io = getattr(bus, "_io", None)
print("I/O thread alive:", io.is_alive() if io else None)
print("I/O thread dead (exception):", bus._dead)

n = 0
t0 = time.time()
while time.time() - t0 < 3.0:
    m = bus.recv(timeout=0.1)
    if m:
        n += 1
print("frames received in 3s:", n)
print("rx queue size:", bus._rx.qsize())

bus.shutdown()
print("shutdown done")
