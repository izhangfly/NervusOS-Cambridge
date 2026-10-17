#!/usr/bin/env python3
"""
PiPER range-of-motion exercise — sweep each joint through its range, one at a
time, slowly, then return everything to zero.

*** THIS MOVES THE ROBOT THROUGH LARGE ARCS. ***

Default behaviour is deliberately conservative:
  - only 60% of each joint's rated range (use --full for the real limits)
  - 15% speed
  - ONE joint moves at a time; all others are held at their current target
  - every joint returns to 0 before the next one starts
  - Ctrl-C fires EmergencyStop immediately

Joint limits are from the PiPER Standard user manual. Verify against your unit
before trusting --full.

Usage:
    piper_range_test.py --dry-run              # print the plan, move nothing
    piper_range_test.py --commit               # 60% envelope, all joints + gripper
    piper_range_test.py --commit --joint 6     # just J6
    piper_range_test.py --commit --full        # rated limits (read the warning)
    piper_range_test.py --commit --no-gripper  # skip the gripper
    piper_range_test.py --estop                # panic
"""

import argparse
import sys
import time

try:
    from piper_sdk import C_PiperInterface_V2
except ImportError:
    sys.exit("piper_sdk not importable — use ~/NervusOS/robots/.venv/bin/python")

CAN_IFACE = "can0"
MDEG = 1000.0                    # joints: 0.001 deg units
GRIP_UNIT = 1000.0               # gripper: 0.001 mm units
ENABLE_ALL, ENABLE_FLAG = 7, 0x02
CTRL_MODE_CAN, MOVE_MODE_J = 0x01, 0x01

# (min_deg, max_deg) per the PiPER standard-edition manual.
JOINT_LIMITS = {
    1: (-154.0, 154.0),
    2: (0.0, 195.0),
    3: (-175.0, 0.0),
    4: (-106.0, 106.0),
    5: (-75.0, 75.0),
    6: (-100.0, 100.0),
}
GRIPPER_MM = (0.0, 70.0)         # max_range_config reports one of (0, 70, 100)

import json
import os
LIMITS_FILE = os.path.join(os.path.dirname(os.path.abspath(__file__)),
                           "workspace_limits.json")


def load_workspace_limits(ignore=False):
    """Prefer calibrated per-installation limits over the datasheet's."""
    if ignore or not os.path.exists(LIMITS_FILE):
        return dict(JOINT_LIMITS), None
    try:
        data = json.load(open(LIMITS_FILE))
        eff = dict(JOINT_LIMITS)
        for k, v in data.get("joints", {}).items():
            j = int(k)
            if "safe_min_deg" not in v or "safe_max_deg" not in v:
                continue
            lo, hi = float(v["safe_min_deg"]), float(v["safe_max_deg"])
            rlo, rhi = JOINT_LIMITS[j]
            if lo > hi:
                print(f"  (J{j}: inverted calibration {lo}>{hi} — ignoring, using rated)")
                continue
            # Calibration may only ever be MORE restrictive than the datasheet.
            eff[j] = (max(lo, rlo), min(hi, rhi))
        return eff, data
    except Exception as e:
        print(f"  (could not read {LIMITS_FILE}: {e}; using rated limits)")
        return dict(JOINT_LIMITS), None


EFFECTIVE = dict(JOINT_LIMITS)

DEFAULT_SPEED = 15
DEFAULT_FRACTION = 0.60
STREAM_HZ = 20                   # JointCtrl is fire-and-forget; must be streamed
SETTLE_TOL = 1.5
MOVE_TIMEOUT = 25


def connect(iface):
    piper = C_PiperInterface_V2(iface)
    piper.ConnectPort()
    for _ in range(50):
        if piper.isOk():
            break
        time.sleep(0.1)
    if not piper.isOk():
        sys.exit(f"CAN read thread unhealthy on {iface}")
    time.sleep(0.5)
    return piper


def joints(piper):
    j = piper.GetArmJointMsgs().joint_state
    return [getattr(j, f"joint_{n}") / MDEG for n in range(1, 7)]


def scaled(j, fraction):
    lo, hi = EFFECTIVE[j]
    mid = (lo + hi) / 2.0
    return (mid + (lo - mid) * fraction, mid + (hi - mid) * fraction)


def stream_to(piper, target_deg, label, timeout=MOVE_TIMEOUT):
    """Stream JointCtrl toward target until every joint settles or we time out."""
    cmd = [int(round(v * MDEG)) for v in target_deg]
    print(f"      -> {label}: " + " ".join(f"J{i+1}={v:7.1f}" for i, v in enumerate(target_deg)))
    deadline = time.time() + timeout
    while time.time() < deadline:
        piper.JointCtrl(*cmd)
        time.sleep(1.0 / STREAM_HZ)
        if all(abs(c - t) < SETTLE_TOL for c, t in zip(joints(piper), target_deg)):
            return True
    print("      (timed out before settling)")
    return False


def main():
    ap = argparse.ArgumentParser(description="PiPER range-of-motion exercise")
    ap.add_argument("--iface", default=CAN_IFACE)
    ap.add_argument("--commit", action="store_true", help="actually move the arm")
    ap.add_argument("--dry-run", action="store_true")
    ap.add_argument("--joint", type=int, choices=[1, 2, 3, 4, 5, 6], help="only this joint")
    ap.add_argument("--full", action="store_true", help="use rated limits, not 60%%")
    ap.add_argument("--fraction", type=float, default=DEFAULT_FRACTION)
    ap.add_argument("--speed", type=int, default=DEFAULT_SPEED)
    ap.add_argument("--no-gripper", action="store_true")
    ap.add_argument("--estop", action="store_true")
    ap.add_argument("--yes", action="store_true")
    ap.add_argument("--ignore-workspace", action="store_true",
                    help="use rated limits instead of calibrated ones (DANGEROUS: "
                         "this is what drove the arm into the table)")
    args = ap.parse_args()

    global EFFECTIVE
    EFFECTIVE, calib = load_workspace_limits(args.ignore_workspace)
    if calib:
        print(f"\nUsing CALIBRATED workspace limits "
              f"(location: {calib.get('location','?')}, {calib.get('calibrated_at','?')})")
    elif args.ignore_workspace:
        print("\n!! --ignore-workspace: using RATED limits. Arm may hit obstacles. !!")
    else:
        print(f"\n!! No {os.path.basename(LIMITS_FILE)} found — falling back to RATED limits.")
        print("!! Run piper_workspace_calib.py --commit first, or clear the workspace.")

    frac = 1.0 if args.full else max(0.05, min(1.0, args.fraction))
    todo = [args.joint] if args.joint else [1, 2, 3, 4, 5, 6]

    piper = connect(args.iface)

    if args.estop:
        piper.EmergencyStop(0x01)
        print("EMERGENCY STOP sent.")
        return 0

    print(f"\n--- plan (envelope {frac*100:.0f}% of rated range, speed {args.speed}%) ---")
    for j in todo:
        lo, hi = scaled(j, frac)
        elo, ehi = EFFECTIVE[j]
        rlo, rhi = JOINT_LIMITS[j]
        src = "calibrated" if (elo, ehi) != (rlo, rhi) else "rated"
        print(f"  J{j}: 0 -> {lo:7.1f} -> {hi:7.1f} -> 0"
              f"     ({src} {elo:.0f}..{ehi:.0f}; rated {rlo:.0f}..{rhi:.0f})")
    if not args.no_gripper and not args.joint:
        print(f"  gripper: close -> open({GRIPPER_MM[1]:.0f}mm) -> close")
    print("  finally: all joints -> 0")

    print("\n--- current ---")
    cur = joints(piper)
    print("  " + " ".join(f"J{i+1}={v:8.3f}" for i, v in enumerate(cur)))

    if args.dry_run or not args.commit:
        print("\nDRY RUN — nothing moved. Re-run with --commit when the area is clear.")
        return 0

    print("\n" + "=" * 64)
    print("  THE ARM WILL SWEEP THROUGH LARGE ARCS")
    print("=" * 64)
    print("  - Clear the ENTIRE reach envelope: front, back, above, below.")
    print("  - Hold the e-stop (pull the aviation plug, manual §7.3).")
    print("  - One joint moves at a time; each returns to 0 before the next.")
    print("  - Ctrl-C = EmergencyStop.")
    if args.full:
        print("\n  !! --full drives to RATED LIMITS. Collision risk is real. !!")
    if not args.yes:
        if input("\n  Type 'RANGE' to proceed: ").strip() != "RANGE":
            print("aborted — nothing sent.")
            return 1

    try:
        print("\n[init] enabling motors + CAN control, MOVE J ...")
        piper.EnableArm(ENABLE_ALL, ENABLE_FLAG)
        time.sleep(1.5)
        piper.MotionCtrl_2(CTRL_MODE_CAN, MOVE_MODE_J, args.speed, 0x00)
        time.sleep(0.3)

        print("[init] homing all joints to zero first ...")
        stream_to(piper, [0.0] * 6, "zero")

        for j in todo:
            lo, hi = scaled(j, frac)
            print(f"\n--- J{j} sweep ---")
            for label, val in (("min", lo), ("max", hi), ("back to zero", 0.0)):
                tgt = [0.0] * 6
                tgt[j - 1] = val
                stream_to(piper, tgt, f"J{j} {label}")
                time.sleep(0.4)

        if not args.no_gripper and not args.joint:
            print("\n--- gripper ---")
            # GripperCtrl(angle_0.001mm, effort, code, set_zero); code 0x01 = enable
            for label, mm in (("open", GRIPPER_MM[1]), ("close", GRIPPER_MM[0])):
                print(f"      -> gripper {label} ({mm:.0f} mm)")
                piper.GripperCtrl(int(mm * GRIP_UNIT), 1000, 0x01, 0)
                time.sleep(2.5)

        print("\n[final] returning all joints to zero ...")
        stream_to(piper, [0.0] * 6, "zero")

    except KeyboardInterrupt:
        piper.EmergencyStop(0x01)
        print("\n\nInterrupted — EMERGENCY STOP sent.")
        return 130

    time.sleep(0.5)
    final = joints(piper)
    print("\n--- final ---")
    print("  " + " ".join(f"J{i+1}={v:8.3f}" for i, v in enumerate(final)))
    ok = all(abs(v) < SETTLE_TOL for v in final)
    print("\n" + ("RANGE TEST COMPLETE — arm returned to zero."
                  if ok else "Finished, but not all joints returned to zero (see above)."))
    return 0 if ok else 2


if __name__ == "__main__":
    try:
        sys.exit(main())
    except KeyboardInterrupt:
        print("\ninterrupted before motion.")
        sys.exit(130)
