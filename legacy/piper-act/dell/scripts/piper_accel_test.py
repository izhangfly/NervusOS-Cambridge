#!/usr/bin/env python3
"""Isolate why the teleop jog no longer moves the arm: acceleration cap vs not."""
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


def move_to(p, target, n=10):
    for _ in range(n):
        p.EndPoseCtrl(*[int(round(v * 1000)) for v in target])
        time.sleep(0.05)


def enable(p):
    p.EmergencyStop(0x02)
    time.sleep(0.3)
    p.EnableArm(7, 0x02)
    time.sleep(1.2)
    p.MotionCtrl_2(0x01, 0x00, 15, 0x00)
    time.sleep(0.5)


def main():
    p = C_PiperInterface_V2("candle", judge_flag=False)
    p.ConnectPort()
    for _ in range(60):
        if p.isOk():
            break
        time.sleep(0.1)
    time.sleep(1.0)

    enable(p)
    p0 = pose(p)
    print("pose0: Z=%.1f" % p0[2])

    # Case 1: NO acceleration cap, +15 mm Z
    t = list(p0); t[2] += 15.0
    move_to(p, t)
    time.sleep(2.0)
    p1 = pose(p)
    print("case1 (no cap)   Z delta = %+.1f mm" % (p1[2] - p0[2]))

    # Case 2: WITH the teleop's acceleration cap, +15 mm more
    p.EndSpdAndAccParamSet(400, 600, 250, 400)
    time.sleep(0.5)
    t = list(p1); t[2] += 15.0
    move_to(p, t)
    time.sleep(2.0)
    p2 = pose(p)
    print("case2 (with cap) Z delta = %+.1f mm" % (p2[2] - p1[2]))

    # Case 3: high (near-unlimited) acceleration, +15 mm more
    p.EndSpdAndAccParamSet(1000, 1000, 2000, 2000)
    time.sleep(0.5)
    t = list(p2); t[2] += 15.0
    move_to(p, t)
    time.sleep(2.0)
    p3 = pose(p)
    print("case3 (high acc) Z delta = %+.1f mm" % (p3[2] - p2[2]))

    p.DisableArm(7, 0x01)


if __name__ == "__main__":
    main()
