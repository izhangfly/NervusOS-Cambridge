#!/usr/bin/env python3
"""Live end-effector pose, full-arm FK, and gripper -- read-only.

Two jobs:

 1. Confirm the Cartesian data needed for a 3D trace is actually populated:
    end pose (X/Y/Z/RX/RY/RZ) and GetFK(), which gives the pose of all six
    joints, enough to draw the whole arm rather than just the tool tip.

 2. Answer the one question that decides whether GRASPING can be demonstrated
    by hand: does the gripper reading change when you squeeze it?

    In TEACHING mode the arm ignores every CAN command (README section 4), so
    the gripper cannot be driven from software while drag-teaching. If it also
    cannot be back-driven by hand, a grasp cannot be demonstrated kinesthetically
    at all, and pick-and-place needs a different capture method.

    python piper_pose_probe.py [seconds]

While it runs: move the arm, then SQUEEZE THE GRIPPER JAWS BY HAND and watch
the gripper column.
"""
import sys
import time
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import piper_mac_can
piper_mac_can.install()

from piper_sdk import C_PiperInterface_V2


def main():
    seconds = int(sys.argv[1]) if len(sys.argv) > 1 else 20

    p = C_PiperInterface_V2("candle", judge_flag=False)
    p.ConnectPort(piper_init=False)
    # FK is computed only when explicitly enabled; without this GetFK() keeps
    # returning all-zero link poses.
    p.EnableFkCal()
    time.sleep(1.5)

    try:
        s = p.GetArmStatus().arm_status
        mode = getattr(s.ctrl_mode, "value", None)
        names = {0x00: "STANDBY", 0x01: "CAN", 0x02: "TEACHING"}
        print(f"ctrl_mode = 0x{mode:02X} {names.get(mode, '?')}")
        if mode != 0x02:
            print("  NOTE: not in TEACHING mode. Press the drag-teach button,")
            print("  otherwise the arm may be in STANDBY and not streaming.")
    except Exception as e:
        print(f"could not read status: {e}")

    print("\nMove the arm, then SQUEEZE THE GRIPPER BY HAND.\n")
    print(f"{'t':>5}  {'X':>8} {'Y':>8} {'Z':>8}  {'RX':>7} {'RY':>7} {'RZ':>7}"
          f"  {'gripper':>8} {'effort':>7}")

    grip_min, grip_max = None, None
    t0 = time.time()
    while time.time() - t0 < seconds:
        try:
            e = p.GetArmEndPoseMsgs().end_pose
            g = p.GetArmGripperMsgs().gripper_state
            # SDK reports 0.001 mm and 0.001 degree units.
            x, y, z = e.X_axis / 1000.0, e.Y_axis / 1000.0, e.Z_axis / 1000.0
            rx, ry, rz = e.RX_axis / 1000.0, e.RY_axis / 1000.0, e.RZ_axis / 1000.0
            ga, ge = g.grippers_angle, g.grippers_effort
            grip_min = ga if grip_min is None else min(grip_min, ga)
            grip_max = ga if grip_max is None else max(grip_max, ga)
            print(f"{time.time() - t0:5.1f}  {x:8.1f} {y:8.1f} {z:8.1f}  "
                  f"{rx:7.1f} {ry:7.1f} {rz:7.1f}  {ga:8d} {ge:7d}")
        except Exception as ex:
            print(f"  read failed: {type(ex).__name__}: {ex}")
        time.sleep(0.5)

    try:
        fk = p.GetFK("feedback")
        print(f"\nGetFK('feedback') -> {len(fk)} link poses "
              f"(each X/Y/Z mm + RX/RY/RZ deg)")
        if fk:
            print(f"  last link (tool): {[round(v, 1) for v in fk[-1]]}")
    except Exception as ex:
        print(f"\nGetFK failed: {type(ex).__name__}: {ex}")

    print(f"\ngripper range seen: {grip_min} .. {grip_max}")
    if grip_min is None or grip_max == grip_min:
        print("  GRIPPER DID NOT MOVE. If you squeezed it, it is not")
        print("  back-drivable in this mode -- a grasp cannot be demonstrated")
        print("  by hand, and pick-and-place needs another capture method.")
    else:
        print("  GRIPPER MOVES BY HAND -> kinesthetic grasping is possible.")

    p.DisconnectPort()
    return 0


if __name__ == "__main__":
    sys.exit(main())
