#!/usr/bin/env python3
"""Diagnose: why does EndPoseCtrl no longer move the arm? Check joints/status
and test a move WITHOUT the EmergencyStop resume (like the original nudge)."""
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


p = C_PiperInterface_V2("candle", judge_flag=False)
p.ConnectPort()
for _ in range(60):
    if p.isOk():
        break
    time.sleep(0.1)
time.sleep(1.0)

j = p.GetArmJointMsgs().joint_state
print("joints (deg):", [round(getattr(j, f"joint_{n}") / 1000.0, 2) for n in range(1, 7)])
print("pose:", [round(v, 2) for v in pose(p)])
s = p.GetArmStatus().arm_status
print("arm_status:", s.arm_status, "| ctrl_mode:", s.ctrl_mode, "| err_code:", s.err_code)

# Enable WITHOUT EmergencyStop (mimic the original working nudge)
p.EnableArm(7, 0x02)
time.sleep(1.2)
p.MotionCtrl_2(0x01, 0x00, 15, 0x00)
time.sleep(0.5)

p0 = pose(p)
t = list(p0)
t[2] += 15.0  # +15 mm Z
for _ in range(10):
    p.EndPoseCtrl(*[int(round(v * 1000)) for v in t])
    time.sleep(0.05)
time.sleep(2.5)
p1 = pose(p)
s = p.GetArmStatus().arm_status
print("after +15mm Z (no resume): Z delta = %+.1f mm | arm_status = %s" % (p1[2] - p0[2], s.arm_status))

p.DisableArm(7, 0x01)
