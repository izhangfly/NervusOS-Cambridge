#!/usr/bin/env python3
"""Verify a joint-space 'home' move (all joints -> 0) works and clears the fault."""
import sys
import time

sys.path.insert(0, str(__import__("pathlib").Path(__file__).resolve().parent))
import piper_mac_can
piper_mac_can.install()

from piper_sdk import C_PiperInterface_V2


def joints(p):
    j = p.GetArmJointMsgs().joint_state
    return [round(getattr(j, f"joint_{n}") / 1000.0, 2) for n in range(1, 7)]


p = C_PiperInterface_V2("candle", judge_flag=False)
p.ConnectPort()
for _ in range(60):
    if p.isOk():
        break
    time.sleep(0.1)
time.sleep(1.0)

print("joints before:", joints(p))

p.EnableArm(7, 0x02)
time.sleep(1.0)
p.ModeCtrl(0x01, 0x01, 30, 0x00)   # CAN + MOVE J, 30%
time.sleep(0.5)
p.JointCtrl(0, 0, 0, 0, 0, 0)       # all joints -> 0
time.sleep(4.0)

s = p.GetArmStatus().arm_status
print("joints after :", joints(p))
print("arm_status   :", s.arm_status, "| ctrl:", s.ctrl_mode, "| mode_feed:", s.mode_feed)

p.DisableArm(7, 0x01)
