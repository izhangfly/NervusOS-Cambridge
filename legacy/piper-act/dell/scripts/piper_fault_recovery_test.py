#!/usr/bin/env python3
"""Determine the exact recovery sequence so jogging resumes after an edge fault."""
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


def st(p):
    s = p.GetArmStatus().arm_status
    return "%s/%s" % (s.arm_status, s.mode_feed)


p = C_PiperInterface_V2("candle", judge_flag=False)
p.ConnectPort()
for _ in range(60):
    if p.isOk():
        break
    time.sleep(0.1)
time.sleep(1.0)

# home first
p.EnableArm(7, 0x02)
time.sleep(1.0)
p.ModeCtrl(0x01, 0x01, 30, 0x00)   # MOVE J
time.sleep(0.3)
p.JointCtrl(0, 0, 0, 0, 0, 0)
time.sleep(4.0)
print("home:  Z=%.1f  %s" % (pose(p)[2], st(p)))

p.ModeCtrl(0x01, 0x00, 15, 0x00)   # MOVE P
time.sleep(0.5)
print("modeP: Z=%.1f  %s" % (pose(p)[2], st(p)))

# fault
p0 = pose(p)
inv = list(p0)
inv[0] = 1500.0
p.EndPoseCtrl(*[int(round(v * 1000)) for v in inv])
time.sleep(1.5)
print("fault: Z=%.1f  %s" % (pose(p)[2], st(p)))

# recovery
p.EmergencyStop(0x02)
time.sleep(0.4)
p.EnableArm(7, 0x02)
time.sleep(1.0)
p.MotionCtrl_2(0x01, 0x00, 15, 0x00)
time.sleep(1.0)
print("recov: Z=%.1f  %s" % (pose(p)[2], st(p)))

# jog +Z
p1 = pose(p)
t = list(p1)
t[2] += 12.0
for _ in range(10):
    p.EndPoseCtrl(*[int(round(v * 1000)) for v in t])
    time.sleep(0.05)
time.sleep(2.5)
print("jog+Z: Z delta=%+.1f mm  %s" % (pose(p)[2] - p1[2], st(p)))

p.DisableArm(7, 0x01)
