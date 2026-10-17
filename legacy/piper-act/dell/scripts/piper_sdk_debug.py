#!/usr/bin/env python3
"""Debug the SDK layer: what bus is it actually using, and is it receiving?"""
import sys
import time

sys.path.insert(0, str(__import__("pathlib").Path(__file__).resolve().parent))
import piper_mac_can
piper_mac_can.install()

from piper_sdk import C_PiperInterface_V2

p = C_PiperInterface_V2("candle", judge_flag=False)
print("created interface")
p.ConnectPort()
print("connected")

arm_can = p._C_PiperInterface_V2__arm_can
bus = arm_can.bus
print("SDK bus:", bus)
print("SDK bus type:", type(bus).__name__)
print("SDK bus channel_info:", getattr(bus, "channel_info", "N/A"))

if hasattr(bus, "_io"):
    print("I/O thread alive:", bus._io.is_alive())
    print("I/O thread dead:", bus._dead)

time.sleep(2.0)
print("isOk:", p.isOk())
print("rx queue size:", getattr(bus, "_rx", None).qsize() if hasattr(bus, "_rx") else "N/A")

t1 = p.GetArmJointMsgs().time_stamp
time.sleep(0.5)
t2 = p.GetArmJointMsgs().time_stamp
print("timestamp advancing:", t1 != t2, "(%s -> %s)" % (t1, t2))
