#!/usr/bin/env python3
"""Read directly from the SDK's bus (bypassing its ReadCan thread)."""
import sys
import time

sys.path.insert(0, str(__import__("pathlib").Path(__file__).resolve().parent))
import piper_mac_can
piper_mac_can.install()

from piper_sdk import C_PiperInterface_V2

p = C_PiperInterface_V2("candle", judge_flag=False)
p.ConnectPort()
time.sleep(1.0)

bus = p._C_PiperInterface_V2__arm_can.bus
print("bus:", type(bus).__name__, "| I/O alive:", bus._io.is_alive(), "| dead:", bus._dead)

n = 0
t0 = time.time()
while time.time() - t0 < 2.0:
    m = bus.recv(timeout=0.1)
    if m:
        n += 1
print("manual read frames in 2s:", n)
print("rx queue size after:", bus._rx.qsize())
