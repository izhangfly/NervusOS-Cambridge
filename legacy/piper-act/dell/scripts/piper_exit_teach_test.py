#!/usr/bin/env python3
"""Try to exit drag-teach recording via CAN: end-recording -> standby -> CAN mode."""
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


def show(p, label):
    s = p.GetArmStatus().arm_status
    print("%-26s ctrl=%s  teach=%s  arm=%s" % (label, s.ctrl_mode, s.teach_status, s.arm_status))


p = C_PiperInterface_V2("candle", judge_flag=False)
p.ConnectPort()
for _ in range(60):
    if p.isOk():
        break
    time.sleep(0.1)
time.sleep(1.0)

show(p, "initial")

# 1. end recording / exit drag-teach (grag_teach_ctrl=0x02)
p.MotionCtrl_1(0x02, 0x00, 0x02)
time.sleep(0.8)
show(p, "after end-teach")

# 2. standby mode
p.MotionCtrl_2(0x00, 0x00, 15, 0x00)
time.sleep(0.8)
show(p, "after standby")

# 3. CAN control mode
p.MotionCtrl_2(0x01, 0x00, 15, 0x00)
time.sleep(0.8)
show(p, "after CAN mode")

# 4. enable + move
p.EnableArm(7, 0x02)
time.sleep(1.2)
p0 = pose(p)
t = list(p0)
t[2] += 15.0
for _ in range(10):
    p.EndPoseCtrl(*[int(round(v * 1000)) for v in t])
    time.sleep(0.05)
time.sleep(2.5)
p1 = pose(p)
s = p.GetArmStatus().arm_status
print("move: Z delta = %+.1f mm | ctrl=%s teach=%s" % (p1[2] - p0[2], s.ctrl_mode, s.teach_status))

p.DisableArm(7, 0x01)
