#!/usr/bin/env python3
"""
PiPER workspace calibration — discover the SAFE range of each joint in the
arm's CURRENT physical installation, by feeling for resistance instead of
trusting the datasheet.

WHY THIS EXISTS
---------------
Rated joint limits (manual) describe the arm in free space. They know nothing
about your table, your mount height, or anything else nearby. On 2026-07-24 a
J2 sweep to 156 deg drove the forearm into the table: every joint was inside
its rated limit, and it still collided.

This tool walks each joint outward in small steps, watching motor current. When
current rises past a threshold (something is pushing back), it stops, backs off,
and records the last known-good angle as that joint's safe limit HERE.

The result is written to workspace_limits.json next to this script. Re-run it
whenever the arm is moved, re-mounted, or the surroundings change.

IMPORTANT — WHAT THIS DOES *NOT* GIVE YOU
-----------------------------------------
Per-joint safe limits are necessary but NOT sufficient. Two joints that are each
individually safe can still collide when combined (e.g. J2 mid-range + J3
extended puts the wrist under the table). Single-axis calibration cannot see
that. Use piper_fk_guard.py for whole-pose checking.

USAGE
    piper_workspace_calib.py --dry-run            # show plan
    piper_workspace_calib.py --commit             # calibrate all joints
    piper_workspace_calib.py --commit --joint 2   # just J2
    piper_workspace_calib.py --show               # print saved limits
    piper_workspace_calib.py --estop
"""

import argparse
import json
import os
import sys
import time
from datetime import datetime

try:
    from piper_sdk import C_PiperInterface_V2
except ImportError:
    sys.exit("piper_sdk not importable — use ~/NervusOS/robots/.venv/bin/python")

HERE = os.path.dirname(os.path.abspath(__file__))
LIMITS_FILE = os.path.join(HERE, "workspace_limits.json")

MDEG = 1000.0
ENABLE_ALL, ENABLE_FLAG = 7, 0x02
CTRL_MODE_CAN, MOVE_MODE_J = 0x01, 0x01

RATED = {1: (-154.0, 154.0), 2: (0.0, 195.0), 3: (-175.0, 0.0),
         4: (-106.0, 106.0), 5: (-75.0, 75.0), 6: (-100.0, 100.0)}

STEP_DEG = 3.0            # how far to probe per increment
STREAM_HZ = 20
STEP_TIMEOUT = 6.0        # max time to let one step complete
STILL_EPS_DEG = 0.08      # movement below this counts as "not moving"
STILL_TIME_S = 0.5        # must be still this long to count as settled
ARRIVE_TOL = 1.2          # deg from target that counts as "arrived"
HOME_TOL_DEG = 1.5
HOME_TIMEOUT = 40.0       # homing from a far pose at 8% speed is slow
BACKOFF_DEG = 6.0         # retreat this far from the blocking angle
CURRENT_MARGIN = 2.5      # multiple of baseline current that counts as "pushing"
CURRENT_FLOOR = 300       # absolute guard if baseline is ~0 (raw sdk units)
CALIB_SPEED = 8           # deliberately slow

# can0 ships with txqueuelen 10; streaming 3 frames @20Hz overflows it and the
# SDK spews SEND_MESSAGE_FAILED (100017). Fix before running:
#     sudo ip link set can0 txqueuelen 1000
MIN_TXQUEUELEN = 1000


def connect(iface):
    p = C_PiperInterface_V2(iface)
    p.ConnectPort()
    for _ in range(50):
        if p.isOk():
            break
        time.sleep(0.1)
    if not p.isOk():
        sys.exit(f"CAN read thread unhealthy on {iface}")
    time.sleep(0.5)
    return p


def joints(p):
    j = p.GetArmJointMsgs().joint_state
    return [getattr(j, f"joint_{n}") / MDEG for n in range(1, 7)]


# Verified against a live PiPER (firmware S-V1.9-0) on 2026-07-24:
#   motor_N.bus_current      int   (0 at idle)
#   motor_N.foc_status       FOC_Status object with the named booleans below
#   motor_N.foc_status_code  int   (64 == healthy/enabled on this unit)
# NOTE: driver_enable_status is True in NORMAL operation — it is NOT a fault.
FAULT_FLAGS = ("collision_status", "stall_status", "driver_error_status",
               "driver_overcurrent", "driver_overheating", "motor_overheating",
               "voltage_too_low")


def motor_current(p, n):
    """Per-motor bus current, or None if unavailable."""
    try:
        m = getattr(p.GetArmLowSpdInfoMsgs(), f"motor_{n}", None)
        v = getattr(m, "bus_current", None)
        return abs(v) if v is not None else None
    except Exception:
        return None


def motor_error(p, n):
    """Return a short description of any ACTIVE fault flag, else None.

    Earlier this returned the whole foc_status object, which is always truthy —
    so every probe read as 'blocked' and the arm never moved. Only the specific
    boolean fault flags count now.
    """
    try:
        m = getattr(p.GetArmLowSpdInfoMsgs(), f"motor_{n}", None)
        st = getattr(m, "foc_status", None)
        if st is None:
            return None
        tripped = [f for f in FAULT_FLAGS if getattr(st, f, False) is True]
        return ", ".join(tripped) if tripped else None
    except Exception:
        return None


def hold(p, target, secs):
    """Stream a target for a while (JointCtrl is fire-and-forget)."""
    cmd = [int(round(v * MDEG)) for v in target]
    end = time.time() + secs
    while time.time() < end:
        p.JointCtrl(*cmd)
        time.sleep(1.0 / STREAM_HZ)


def stream_until_still(p, target, j, timeout=STEP_TIMEOUT):
    """Stream `target` until joint j stops moving, or timeout.

    Returns (final_deg, moved_at_all, timed_out).

    A fixed settle time cannot tell "still travelling" from "blocked" — at 8%
    speed a 3 deg step can take longer than any timer you'd want to wait, while
    a real obstruction stalls immediately. So we watch for motion to CEASE.
    """
    cmd = [int(round(v * MDEG)) for v in target]
    last = joints(p)[j - 1]
    start = last
    still_for = 0.0
    deadline = time.time() + timeout
    while time.time() < deadline:
        p.JointCtrl(*cmd)
        time.sleep(1.0 / STREAM_HZ)
        now = joints(p)[j - 1]
        if abs(now - last) < STILL_EPS_DEG:
            still_for += 1.0 / STREAM_HZ
        else:
            still_for = 0.0
        last = now
        if still_for >= STILL_TIME_S:
            return now, abs(now - start) > STILL_EPS_DEG, False
    return last, abs(last - start) > STILL_EPS_DEG, True


def home_verified(p, log, timeout=HOME_TIMEOUT):
    """Drive every joint to 0 and CONFIRM it got there.

    The first version streamed zero for a flat 3 s. J1 was still at +58 deg when
    probing started, so the very first probe step read a 61 deg 'tracking error'
    and declared the joint blocked. Homing must be verified, not assumed.
    """
    cmd = [0] * 6
    deadline = time.time() + timeout
    while time.time() < deadline:
        p.JointCtrl(*cmd)
        time.sleep(1.0 / STREAM_HZ)
        cur = joints(p)
        if all(abs(v) < HOME_TOL_DEG for v in cur):
            log("    homed: " + " ".join(f"J{i+1}={v:+.1f}" for i, v in enumerate(cur)))
            return True
    cur = joints(p)
    log("    HOME FAILED: " + " ".join(f"J{i+1}={v:+.1f}" for i, v in enumerate(cur)))
    return False


def probe_direction(p, j, direction, baseline, rated_stop, log):
    """Walk J{j} outward one STEP_DEG at a time until something resists."""
    target = [0.0] * 6
    last_good = 0.0
    angle = 0.0
    while True:
        nxt = angle + direction * STEP_DEG
        if (direction > 0 and nxt > rated_stop) or (direction < 0 and nxt < rated_stop):
            log(f"      reached rated limit {rated_stop:+.1f} without resistance")
            return rated_stop, "rated"

        target[j - 1] = nxt
        actual, moved, timed_out = stream_until_still(p, target, j)
        cur = motor_current(p, j)
        err = motor_error(p, j)
        short_by = abs(actual - nxt)

        blocked = False
        why = ""
        if err:
            blocked, why = True, f"fault flags: {err}"
        elif cur is not None and baseline is not None and \
                cur > max(baseline * CURRENT_MARGIN, CURRENT_FLOOR):
            blocked, why = True, \
                f"current {cur:.0f} > {max(baseline*CURRENT_MARGIN, CURRENT_FLOOR):.0f}"
        elif timed_out and moved:
            # Still creeping when the timer expired — slow, not obstructed.
            log(f"      {nxt:+7.1f} slow (at {actual:+.1f}, still moving) — continuing")
            last_good = actual
            angle = actual
            continue
        elif short_by > ARRIVE_TOL:
            # Motion CEASED short of the commanded angle => something stops it.
            blocked, why = True, \
                f"stopped {short_by:.1f} deg short (commanded {nxt:+.1f}, rested at {actual:+.1f})"

        if blocked:
            # Back off toward zero, but never cross it — otherwise a joint that
            # blocks immediately yields a limit on the WRONG side of zero, and
            # min/max come out inverted.
            safe = last_good - direction * BACKOFF_DEG
            safe = min(0.0, safe) if direction < 0 else max(0.0, safe)
            log(f"      BLOCKED at {nxt:+.1f}: {why}")
            log(f"      backing off -> safe limit {safe:+.1f}")
            target[j - 1] = safe
            hold(p, target, 1.2)
            return safe, why

        last_good = nxt
        angle = nxt
        log(f"      {nxt:+7.1f} ok" + (f"  (I={cur:.0f})" if cur is not None else ""))


def main():
    ap = argparse.ArgumentParser(description="PiPER workspace (installation) calibration")
    ap.add_argument("--iface", default="can0")
    ap.add_argument("--commit", action="store_true")
    ap.add_argument("--dry-run", action="store_true")
    ap.add_argument("--joint", type=int, choices=[1, 2, 3, 4, 5, 6])
    ap.add_argument("--show", action="store_true", help="print saved limits and exit")
    ap.add_argument("--estop", action="store_true")
    ap.add_argument("--location", default="", help="label for where the arm is mounted")
    ap.add_argument("--yes", action="store_true")
    args = ap.parse_args()

    if args.show:
        if not os.path.exists(LIMITS_FILE):
            print(f"no calibration yet at {LIMITS_FILE}")
            return 1
        print(json.dumps(json.load(open(LIMITS_FILE)), indent=2))
        return 0

    # Preflight: a 10-deep TX queue cannot carry 3 frames @20Hz and the bus
    # floods with SEND_MESSAGE_FAILED (100017), corrupting every measurement.
    try:
        qlen = int(open(f"/sys/class/net/{args.iface}/tx_queue_len").read().strip())
        if qlen < MIN_TXQUEUELEN:
            print(f"\nERROR: {args.iface} tx_queue_len is {qlen}, need >= {MIN_TXQUEUELEN}.")
            print(f"  Fix:  sudo ip link set {args.iface} txqueuelen {MIN_TXQUEUELEN}")
            print("  (Persist it in /etc/systemd/system/can0.service.)")
            return 1
    except FileNotFoundError:
        print(f"  (could not read tx_queue_len for {args.iface}; continuing)")

    p = connect(args.iface)

    if args.estop:
        p.EmergencyStop(0x01)
        print("EMERGENCY STOP sent.")
        return 0

    todo = [args.joint] if args.joint else [1, 2, 3, 4, 5, 6]

    print(f"\nWorkspace calibration — probes each joint until it meets resistance.")
    print(f"  step {STEP_DEG} deg · speed {CALIB_SPEED}% · backoff {BACKOFF_DEG} deg")
    print(f"  joints: {todo}")
    print(f"  output: {LIMITS_FILE}")

    if args.dry_run or not args.commit:
        print("\nDRY RUN — nothing moved.")
        return 0

    print("\n" + "=" * 64)
    print("  THE ARM WILL PROBE UNTIL IT TOUCHES SOMETHING")
    print("=" * 64)
    print("  This is intentional: it finds limits by making light contact.")
    print("  - Clear FRAGILE items; leave the table/mount as it normally is.")
    print("  - Hold the e-stop (aviation plug, manual §7.3).")
    print("  - It moves at 8% speed in 3 deg steps and backs off on resistance.")
    print("  - Ctrl-C = EmergencyStop.")
    if not args.yes:
        if input("\n  Type 'CALIBRATE' to proceed: ").strip() != "CALIBRATE":
            print("aborted.")
            return 1

    results = {}
    lines = []

    def log(s):
        print(s)
        lines.append(s)

    try:
        p.EnableArm(ENABLE_ALL, ENABLE_FLAG)
        time.sleep(1.5)
        p.MotionCtrl_2(CTRL_MODE_CAN, MOVE_MODE_J, CALIB_SPEED, 0x00)
        time.sleep(0.3)

        log("\n[home] driving all joints to 0 (verified) ...")
        if not home_verified(p, log):
            p.EmergencyStop(0x01)
            print("\nABORTED: could not home before probing. Nothing was calibrated.")
            print("Move the arm clear by hand, clear faults, and retry.")
            return 1

        for j in todo:
            lo_r, hi_r = RATED[j]
            log(f"\n--- J{j} (rated {lo_r:+.0f} .. {hi_r:+.0f}) ---")
            base = motor_current(p, j)
            log(f"    baseline current: {base if base is not None else 'unavailable'}")

            log("    probing negative ...")
            lo, lo_why = probe_direction(p, j, -1, base, lo_r, log)
            home_verified(p, log)

            log("    probing positive ...")
            hi, hi_why = probe_direction(p, j, +1, base, hi_r, log)
            home_verified(p, log)

            if lo > hi:                      # never emit an inverted range
                log(f"    !! inverted result ({lo:+.1f} > {hi:+.1f}) — recording as 0..0")
                lo = hi = 0.0
            results[str(j)] = {
                "safe_min_deg": round(lo, 1), "safe_max_deg": round(hi, 1),
                "rated_min_deg": lo_r, "rated_max_deg": hi_r,
                "min_stop_reason": lo_why, "max_stop_reason": hi_why,
            }
            log(f"    J{j} SAFE RANGE: {lo:+.1f} .. {hi:+.1f}")

        log("\n[final] returning to zero")
        hold(p, [0.0] * 6, 3.0)

    except KeyboardInterrupt:
        p.EmergencyStop(0x01)
        print("\n\nInterrupted — EMERGENCY STOP sent. Partial results below.")

    payload = {
        "schema": "piper-workspace-limits/v1",
        "calibrated_at": datetime.now().isoformat(timespec="seconds"),
        "location": args.location or "unspecified",
        "iface": args.iface,
        "method": "incremental probe with current/tracking resistance detection",
        "step_deg": STEP_DEG, "backoff_deg": BACKOFF_DEG,
        "warning": ("Per-joint only. Multi-joint poses can still collide; "
                    "use piper_fk_guard.py for whole-pose checking."),
        "joints": results,
    }
    # Merge so calibrating a single joint doesn't discard the others.
    if os.path.exists(LIMITS_FILE):
        try:
            old = json.load(open(LIMITS_FILE))
            merged = old.get("joints", {})
            merged.update(results)
            payload["joints"] = merged
        except Exception:
            pass

    with open(LIMITS_FILE, "w") as f:
        json.dump(payload, f, indent=2)
    print(f"\nwrote {LIMITS_FILE}")
    print(json.dumps(payload["joints"], indent=2))
    return 0


if __name__ == "__main__":
    try:
        sys.exit(main())
    except KeyboardInterrupt:
        print("\ninterrupted.")
        sys.exit(130)
