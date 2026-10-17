#!/usr/bin/env python3
"""One-shot safe nudge: switch to CAN control, command a small +Z move, verify.

Read-only until it has proven RX is live. Moves the end-effector ~20 mm up at
10% speed, confirms the pose actually changed, then leaves motors ENABLED so the
arm holds its position (matches the state it was found in). Never calls
ResetPiper.

    python piper_nudge.py
"""

import sys
import time

sys.path.insert(0, str(__import__("pathlib").Path(__file__).resolve().parent))
import piper_mac_can
piper_mac_can.install()

from piper_sdk import C_PiperInterface_V2

MMM = 1000.0
MDEG = 1000.0
Z_MOVE_MM = 20.0
SPEED = 10


def pose(p):
    e = p.GetArmEndPoseMsgs().end_pose
    return [e.X_axis / MMM, e.Y_axis / MMM, e.Z_axis / MMM,
            e.RX_axis / MDEG, e.RY_axis / MDEG, e.RZ_axis / MDEG]


def fmt(p):
    return "X=%.1fmm Y=%.1fmm Z=%.1fmm RX=%.1f RY=%.1f RZ=%.1f" % tuple(p)


def main():
    p = C_PiperInterface_V2("candle", judge_flag=False)
    p.ConnectPort()
    for _ in range(60):
        if p.isOk():
            break
        time.sleep(0.1)
    time.sleep(1.0)

    # Prove RX is live before ANY motion command. Retry a few times: the shim's
    # first connection after a fresh process occasionally needs a beat to start
    # the I/O thread, and a false "NO RX" here must never become a moved arm.
    rx_ok = False
    for i in range(8):
        ok = p.isOk()
        t1 = p.GetArmJointMsgs().time_stamp
        time.sleep(0.6)
        t2 = p.GetArmJointMsgs().time_stamp
        print("  rx check %d: isOk=%s ts_adv=%s" % (i + 1, ok, t1 != t2))
        if ok and t1 != t2:
            rx_ok = True
            break
    if not rx_ok:
        print("NO RX after retries — refusing to command motion.")
        return

    p0 = pose(p)
    print("initial pose :", fmt(p0))

    # Enable motors (idempotent), then switch to CAN + MOVE P at low speed.
    p.EnableArm(7, 0x02)
    time.sleep(1.2)
    p.MotionCtrl_2(0x01, 0x00, SPEED, 0x00)
    time.sleep(0.6)

    # Re-read pose now that we hold CAN control (in case the mode switch moved it).
    p1 = pose(p)
    print("locked pose  :", fmt(p1))

    target = list(p1)
    target[2] += Z_MOVE_MM
    print("target pose  :", fmt(target), "(+%.0f mm Z)" % Z_MOVE_MM)

    # Stream the target briefly so at least one frame lands.
    for _ in range(10):
        p.EndPoseCtrl(*[int(round(v * 1000)) for v in target])
        time.sleep(0.05)

    time.sleep(3.0)
    p2 = pose(p)
    dz = p2[2] - p1[2]
    print("after move   :", fmt(p2))
    print("Z delta      : %+.1f mm" % dz)

    if dz > 2.0:
        print("RESULT: arm moved up %.1f mm — CAN control on Windows is fully working." % dz)
    else:
        print("RESULT: no significant motion detected (dz=%.1f mm)." % dz)

    # Leave motors ENABLED so the arm holds position.
    print("done — motors left ENABLED, arm holding position.")


if __name__ == "__main__":
    main()
