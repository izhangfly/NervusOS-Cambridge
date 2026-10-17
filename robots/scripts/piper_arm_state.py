#!/usr/bin/env python3
"""
Read the arm's real state through piper_sdk: control mode, motor enable flags,
errors, joints, end pose.

Verifies RX is actually flowing before printing anything. Without that check the
SDK hands back default-initialised objects — every field reading a plausible
STANDBY / NORMAL / False — which looks exactly like a healthy idle arm rather
than a dead bus.

    /Users/ianzhang/agilex/.venv/bin/python piper_arm_state.py
"""

import sys
import time

sys.path.insert(0, str(__import__("pathlib").Path(__file__).resolve().parent))
import piper_mac_can
piper_mac_can.install()

from piper_sdk import C_PiperInterface_V2


def main():
    p = C_PiperInterface_V2("candle", judge_flag=False)
    p.ConnectPort()
    for _ in range(60):
        if p.isOk():
            break
        time.sleep(0.1)
    time.sleep(1.0)

    # Is data really arriving? A moving timestamp is the proof; isOk() alone is not.
    t1 = p.GetArmJointMsgs().time_stamp
    time.sleep(0.6)
    t2 = p.GetArmJointMsgs().time_stamp
    if not p.isOk() or t1 == t2:
        sys.exit(
            "NO RX — the SDK is not receiving.\n"
            "  Everything below would be default values, not the arm's real state.\n"
            "  Run piper_can_check.py; if it says WEDGED, replug the CAN stick."
        )
    print(f"RX live (timestamp {t1} -> {t2})\n")

    j = p.GetArmJointMsgs().joint_state
    print("joints (deg):", [round(getattr(j, f"joint_{n}") / 1000.0, 2) for n in range(1, 7)])
    e = p.GetArmEndPoseMsgs().end_pose
    print("end pose    : X=%.1fmm Y=%.1fmm Z=%.1fmm RX=%.1f RY=%.1f RZ=%.1f" % (
        e.X_axis / 1000, e.Y_axis / 1000, e.Z_axis / 1000,
        e.RX_axis / 1000, e.RY_axis / 1000, e.RZ_axis / 1000))
    g = p.GetArmGripperMsgs().gripper_state
    print("gripper     : %.1f mm" % (g.grippers_angle / 1000.0))

    s = p.GetArmStatus().arm_status
    print("\ncontrol mode:", s.ctrl_mode)
    print("arm status  :", s.arm_status)
    print("mode feed   :", s.mode_feed)
    print("teach status:", s.teach_status)
    print("error code  :", s.err_code)

    lo = p.GetArmLowSpdInfoMsgs()
    flags = [bool(getattr(lo, f"motor_{n}").foc_status.driver_enable_status) for n in range(1, 7)]
    print("motors enabled:", flags)

    if not any(flags):
        print("\n  Motors are OFF — the arm cannot move. Press ENABLE in the web panel,")
        print("  or call EnableArm(7, 0x02). Check the E-stop button is released.")
    elif not all(flags):
        print("\n  Only some motors enabled — check the ones reading False.")


if __name__ == "__main__":
    main()
