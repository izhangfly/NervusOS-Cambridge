#!/usr/bin/env python3
"""Test whether repeated EnableArm eventually enables the motors."""
import sys
import time

sys.path.insert(0, str(__import__("pathlib").Path(__file__).resolve().parent))
import piper_mac_can
piper_mac_can.install()

from piper_sdk import C_PiperInterface_V2


def motors(p):
    lo = p.GetArmLowSpdInfoMsgs()
    return [bool(getattr(lo, f"motor_{n}").foc_status.driver_enable_status) for n in range(1, 7)]


def state(p, label):
    s = p.GetArmStatus().arm_status
    print("%-22s ctrl=%s arm=%s motors=%s" % (label, s.ctrl_mode, s.arm_status, motors(p)))


p = C_PiperInterface_V2("candle", judge_flag=False)
p.ConnectPort()
for _ in range(60):
    if p.isOk():
        break
    time.sleep(0.1)
time.sleep(1.0)

state(p, "initial")

for i in range(8):
    p.EnableArm(7, 0x02)
    time.sleep(0.4)
    m = motors(p)
    state(p, "enable try %d" % (i + 1))
    if all(m):
        print(">>> motors enabled after %d attempts" % (i + 1))
        break

p.DisableArm(7, 0x01)
