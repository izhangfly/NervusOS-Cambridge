#!/usr/bin/env python3
"""
PiPER verification 2 — enable the arm and move all joints to zero, then eyeball the pose.

This is step 2 of the AgileX's *New PiPER arm: J6 zero-point setting* verification:

    Verification 2: enable the arm, move every joint back to zero, and check that the arm, gripper and teach pendant poses look normal.

*** THIS MOVES THE ROBOT. *** Unlike piper_j6_zero.py (which sends no motion),
this commands every joint to 0 deg. Read the safety block below before running.

Sequence (piper_sdk / CAN):
    EnableArm(7, 0x02)                              CAN 0x471  enable all motors
    MotionCtrl_2(ctrl_mode=0x01, move_mode=0x01,    CAN 0x151  CAN-control + MOVE J
                 move_spd_rate_ctrl=<slow>)
    JointCtrl(0,0,0,0,0,0)                          CAN 0x155+ all joints -> 0
    EmergencyStop()                                 CAN 0x150  on Ctrl-C

Usage:
    ~/NervusOS/robots/.venv/bin/python scripts/piper_verify_zero.py --dry-run
    ~/NervusOS/robots/.venv/bin/python scripts/piper_verify_zero.py --commit
    ~/NervusOS/robots/.venv/bin/python scripts/piper_verify_zero.py --commit --speed 10
    ~/NervusOS/robots/.venv/bin/python scripts/piper_verify_zero.py --estop   # panic
"""

import argparse
import sys
import time

try:
    from piper_sdk import C_PiperInterface_V2
except ImportError:
    sys.exit("piper_sdk not importable — use ~/NervusOS/robots/.venv/bin/python")

CAN_IFACE = "can0"
MDEG = 1000.0                # piper reports/accepts 0.001 deg units
ENABLE_ALL, ENABLE_FLAG = 7, 0x02
CTRL_MODE_CAN, MOVE_MODE_J = 0x01, 0x01
DEFAULT_SPEED = 15           # percent; deliberately slow for a first move
SETTLE_TOL_DEG = 1.0
TIMEOUT_S = 30


def connect(iface):
    piper = C_PiperInterface_V2(iface)
    piper.ConnectPort()
    for _ in range(50):
        if piper.isOk():
            break
        time.sleep(0.1)
    if not piper.isOk():
        sys.exit(f"CAN read thread unhealthy on {iface}. Check: ip -br link show {iface}")
    time.sleep(0.5)
    return piper


def joints(piper):
    j = piper.GetArmJointMsgs().joint_state
    return [getattr(j, f"joint_{n}") / MDEG for n in range(1, 7)]


def show(piper, label):
    d = joints(piper)
    print(f"  {label}: " + "  ".join(f"J{i+1}={v:8.3f}" for i, v in enumerate(d)))
    return d


def main():
    ap = argparse.ArgumentParser(description="PiPER verification 2 — move joints to zero")
    ap.add_argument("--iface", default=CAN_IFACE)
    ap.add_argument("--commit", action="store_true", help="actually move the arm")
    ap.add_argument("--dry-run", action="store_true", help="show plan, move nothing")
    ap.add_argument("--speed", type=int, default=DEFAULT_SPEED,
                    help=f"speed rate percent 1-100 (default {DEFAULT_SPEED})")
    ap.add_argument("--estop", action="store_true", help="send emergency stop and exit")
    ap.add_argument("--yes", action="store_true", help="skip confirmation")
    args = ap.parse_args()

    if not 1 <= args.speed <= 100:
        sys.exit("--speed must be 1..100")

    piper = connect(args.iface)

    if args.estop:
        piper.EmergencyStop(0x01)
        print("EMERGENCY STOP sent (CAN 0x150).")
        return 0

    print("\n--- current state ---")
    try:
        st = piper.GetArmStatus().arm_status
        print(f"  ctrl_mode={st.ctrl_mode} arm_status={st.arm_status} "
              f"teach_status={st.teach_status}")
    except Exception as e:
        print(f"  status unavailable ({e})")
    before = show(piper, "before")

    biggest = max(abs(v) for v in before)
    print(f"\n  Largest joint excursion from zero: {biggest:.1f} deg")
    print(f"  Speed rate: {args.speed}%   (all 6 joints will drive to 0.000)")

    if args.dry_run or not args.commit:
        print("\nDRY RUN — nothing moved. Re-run with --commit when the area is clear.")
        return 0

    print("\n" + "=" * 62)
    print("  THE ARM IS ABOUT TO MOVE")
    print("=" * 62)
    print("  - Clear everything within the arm's full reach (including above it).")
    print("  - Hold the e-stop: pull the aviation plug (manual §7.3).")
    print("  - Keep hands clear of the gripper and all joints.")
    print("  - Ctrl-C sends an EmergencyStop immediately.")
    if not args.yes:
        if input("\n  Type 'MOVE' to proceed: ").strip() != "MOVE":
            print("aborted — arm not enabled, nothing sent.")
            return 1

    try:
        print(f"\n[1/3] enabling all motors (CAN 0x471) ...")
        piper.EnableArm(ENABLE_ALL, ENABLE_FLAG)
        time.sleep(1.5)

        print(f"[2/3] CAN-control + MOVE J @ {args.speed}% (CAN 0x151) ...")
        piper.MotionCtrl_2(CTRL_MODE_CAN, MOVE_MODE_J, args.speed, 0x00)
        time.sleep(0.3)

        print("[3/3] commanding all joints -> 0.000 deg ...")
        deadline = time.time() + TIMEOUT_S
        while time.time() < deadline:
            # JointCtrl is fire-and-forget: it must be streamed, not sent once.
            piper.JointCtrl(0, 0, 0, 0, 0, 0)
            time.sleep(0.1)
            cur = joints(piper)
            if all(abs(v) < SETTLE_TOL_DEG for v in cur):
                print("\n  all joints within tolerance of zero")
                break
        else:
            print("\n  timed out before every joint settled (see readings below)")

    except KeyboardInterrupt:
        piper.EmergencyStop(0x01)
        print("\n\nInterrupted — EMERGENCY STOP sent.")
        return 130

    time.sleep(0.5)
    print("\n--- result ---")
    after = show(piper, "after ")

    print("\n" + "=" * 62)
    ok = all(abs(v) < SETTLE_TOL_DEG for v in after)
    print("  RESULT:", "all joints at zero" if ok else "NOT all joints reached zero")
    print("=" * 62)
    print("""
  NOW CHECK PHYSICALLY (this is the actual verification 2 — the numbers alone
  do not prove it; the doc asks for "no abnormality in the pose"):

    1. The flange groove should still point STRAIGHT UP, in line with the
       rear screw hole and the teach button (a three-point line). If J6 zero took
       correctly, zero pose reproduces that alignment.
    2. The gripper / teach pendant should sit square — not rolled or
       twisted a few degrees off.
    3. No joint should be straining, buzzing, or holding against a limit.

  If the flange is visibly rotated off vertical at zero, the J6 zero was
  captured at the wrong physical position: re-align and re-run
  piper_j6_zero.py --commit.
""")
    return 0 if ok else 2


if __name__ == "__main__":
    try:
        sys.exit(main())
    except KeyboardInterrupt:
        print("\ninterrupted before any motion command.")
        sys.exit(130)
