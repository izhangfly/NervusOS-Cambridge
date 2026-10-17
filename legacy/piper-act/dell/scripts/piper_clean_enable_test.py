#!/usr/bin/env python3
"""Verify EnableArm (no EmergencyStop) + MotionCtrl_2 + EndPoseCtrl moves the arm."""
import sys
import time

sys.path.insert(0, str(__import__("pathlib").Path(__file__).resolve().parent))
import piper_mac_can
piper_mac_can.install()

from piper_sdk import C_PiperInterface_V2


def pose(p):
    e = p.GetArmEndPoseMsgs().end_pose
    return [e.X_axis / 1000.0, e.Y_axis / 1000.0, e.Z_axis / 1000.0,
            e.RX_axis / 1000.0, e.RY_axis / 1000.0, e.RZ_axis / 1000.0]


def motors(p):
    lo = p.GetArmLowSpdInfoMsgs()
    return [bool(getattr(lo, f"motor_{n}").foc_status.driver_enable_status) for n in range(1, 7)]


p = C_PiperInterface_V2("candle", judge_flag=False)
p.ConnectPort()
for _ in range(60):
    if p.isOk():
        break
    time.sleep(0.1)
time.sleep(1.0)

p.EnableArm(7, 0x02)
time.sleep(1.2)
p.MotionCtrl_2(0x01, 0x00, 15, 0x00)
time.sleep(0.6)

s = p.GetArmStatus().arm_status
print("after enable+CAN: ctrl=%s arm=%s motors=%s" % (s.ctrl_mode, s.arm_status, motors(p)))

p0 = pose(p)
t = list(p0)
t[2] += 15.0
for _ in range(12):
    p.EndPoseCtrl(*[int(round(v * 1000)) for v in t])
    time.sleep(0.05)
time.sleep(2.5)
p1 = pose(p)
s = p.GetArmStatus().arm_status
print("move: Z delta = %+.1f mm | ctrl=%s arm=%s motors=%s" % (p1[2] - p0[2], s.ctrl_mode, s.arm_status, motors(p)))

p.DisableArm(7, 0x01)
