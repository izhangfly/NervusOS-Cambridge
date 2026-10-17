#!/usr/bin/env python3
"""SDK connect but SKIP PiperInit (piper_init=False) — cross-process test."""
import sys
import time

sys.path.insert(0, str(__import__("pathlib").Path(__file__).resolve().parent))
import piper_mac_can
piper_mac_can.install()

from piper_sdk import C_PiperInterface_V2

p = C_PiperInterface_V2("candle", judge_flag=False)
p.ConnectPort(piper_init=False)  # skip the 3 query sends
time.sleep(2.0)
t1 = p.GetArmJointMsgs().time_stamp
time.sleep(0.5)
t2 = p.GetArmJointMsgs().time_stamp
print("isOk:", p.isOk(), "| timestamp advancing:", t1 != t2, "(%s -> %s)" % (t1, t2))
