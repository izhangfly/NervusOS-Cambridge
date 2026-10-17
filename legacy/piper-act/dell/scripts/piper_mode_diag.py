#!/usr/bin/env python3
"""Diagnose the control-mode state and test a move after the full enable sequence."""
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


def state(p, label):
    s = p.GetArmStatus().arm_status
    lo = p.GetArmLowSpdInfoMsgs()
    motors = [bool(getattr(lo, f"motor_{n}").foc_status.driver_enable_status) for n in range(1, 7)]
    print("%-22s ctrl=%s  arm=%s  mode_feed=%s  motors=%d/6" % (
        label, s.ctrl_mode, s.arm_status, s.mode_feed, sum(motors)))


p = C_PiperInterface_V2("candle", judge_flag=False)
p.ConnectPort()
for _ in range(60):
    if p.isOk():
        break
    time.sleep(0.1)
time.sleep(1.0)

state(p, "initial")

# The teleop enable() sequence
p.EmergencyStop(0x02)
time.sleep(0.4)
state(p, "after resume")

p.EnableArm(7, 0x02)
time.sleep(1.2)
state(p, "after enable")

p.MotionCtrl_2(0x01, 0x00, 15, 0x00)
time.sleep(0.8)
state(p, "after CAN mode")

# test move +15 mm Z
p0 = pose(p)
t = list(p0)
t[2] += 15.0
for _ in range(12):
    p.EndPoseCtrl(*[int(round(v * 1000)) for v in t])
    time.sleep(0.05)
time.sleep(2.5)
p1 = pose(p)
s = p.GetArmStatus().arm_status
print("move: Z delta = %+.1f mm | ctrl=%s arm=%s" % (p1[2] - p0[2], s.ctrl_mode, s.arm_status))

p.DisableArm(7, 0x01)
