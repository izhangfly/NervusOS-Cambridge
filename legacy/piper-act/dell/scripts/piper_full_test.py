#!/usr/bin/env python3
"""Enable -> home (joints to 0, motors stay ON) -> Cartesian jog +Z. Verify move."""
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

p.EnableArm(7, 0x02)
time.sleep(1.0)
p.ModeCtrl(0x01, 0x01, 30, 0x00)   # CAN + MOVE J
time.sleep(0.3)
p.JointCtrl(0, 0, 0, 0, 0, 0)       # home
time.sleep(4.0)
print("after home: joints=%s" % joints(p))
print("after home: pose=%s" % [round(v, 2) for v in pose(p)])

p.ModeCtrl(0x01, 0x00, 15, 0x00)    # back to MOVE P
time.sleep(0.5)
p0 = pose(p)
t = list(p0)
t[2] += 15.0
for _ in range(10):
    p.EndPoseCtrl(*[int(round(v * 1000)) for v in t])
    time.sleep(0.05)
time.sleep(2.5)
p1 = pose(p)
s = p.GetArmStatus().arm_status
print("after jog +Z: Z delta = %+.1f mm | arm_status = %s" % (p1[2] - p0[2], s.arm_status))

p.DisableArm(7, 0x01)
