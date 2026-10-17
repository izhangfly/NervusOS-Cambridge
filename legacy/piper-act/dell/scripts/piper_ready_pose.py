#!/usr/bin/env python3
"""
Find a joint pose from which Cartesian control actually works.

The joint-zero "home" is a kinematic singularity: the arm stands straight up
with its joints aligned and the wrist Euler angles sit at RY~85 deg, one degree
off gimbal lock. From there EndPoseCtrl can only solve Z — X, Y, RX and RZ all
return TARGET_POS_EXCEEDS_LIMIT. Recovering to that pose therefore drops the
operator somewhere they cannot steer.

This tries candidate elbow-bent poses and reports, for each, how many of the
six Cartesian axes can be jogged. The winner becomes the teleop's READY pose.
"""
import sys
import time

sys.path.insert(0, str(__import__("pathlib").Path(__file__).resolve().parent))
import piper_mac_can
piper_mac_can.install()

from piper_sdk import C_PiperInterface_V2

MMM = MDEG = 1000.0
AXES = ["X", "Y", "Z", "RX", "RY", "RZ"]
SPEED = 30
STREAM_HZ = 20
LEAD_MM = 200.0 * SPEED / 100.0 / STREAM_HZ
LEAD_DEG = 150.0 * SPEED / 100.0 / STREAM_HZ

# (J1..J6) in degrees. Elbow bent, wrist pitched clear of +-90 Euler lock.
CANDIDATES = {
    "ready-A  (J2 45 / J3 -60 / J5 45)": (0, 45, -60, 0, 45, 0),
    "ready-B  (J2 60 / J3 -80 / J5 60)": (0, 60, -80, 0, 60, 0),
    "ready-C  (J2 30 / J3 -45 / J5 30)": (0, 30, -45, 0, 30, 0),
}


def pose(p):
    e = p.GetArmEndPoseMsgs().end_pose
    return [e.X_axis / MMM, e.Y_axis / MMM, e.Z_axis / MMM,
            e.RX_axis / MDEG, e.RY_axis / MDEG, e.RZ_axis / MDEG]


def status(p):
    return getattr(p.GetArmStatus().arm_status.arm_status, "name", "?")


def goto_joints(p, degs, settle=5.0):
    p.ModeCtrl(0x01, 0x01, 30, 0x00)          # CAN + MOVE J (no IK, never faults)
    time.sleep(0.4)
    p.JointCtrl(*[int(round(d * 1000)) for d in degs])
    time.sleep(settle)
    p.MotionCtrl_2(0x01, 0x00, SPEED, 0x00)   # back to MOVE P for Cartesian
    time.sleep(0.6)


def jog(p, idx, direction, secs=1.2):
    t0 = time.time()
    while time.time() - t0 < secs:
        tgt = pose(p)
        tgt[idx] += direction * (LEAD_MM if idx < 3 else LEAD_DEG)
        p.EndPoseCtrl(*[int(round(v * 1000)) for v in tgt])
        time.sleep(1.0 / STREAM_HZ)
    time.sleep(0.7)


def survey(p, degs, label):
    print(f"\n{label}")
    goto_joints(p, degs)
    print(f"   pose {['%.1f' % v for v in pose(p)]}  status {status(p)}")
    good = []
    for i, ax in enumerate(AXES):
        before = pose(p)
        jog(p, i, +1)
        after, st = pose(p), status(p)
        d = after[i] - before[i]
        ok = abs(d) > (2.0 if i < 3 else 1.0)
        if ok:
            good.append(ax)
        else:
            # Clear the latched fault so the next axis starts clean.
            goto_joints(p, degs, settle=4.0)
        print(f"     {ax:<3} d={d:+7.1f} {'mm ' if i < 3 else 'deg'}  {st:<24} "
              f"{'OK' if ok else 'blocked'}")
    print(f"   => {len(good)}/6 axes usable: {', '.join(good) if good else 'none'}")
    return len(good), good


def main():
    p = C_PiperInterface_V2("candle", judge_flag=False)
    p.ConnectPort()
    for _ in range(60):
        if p.isOk():
            break
        time.sleep(0.1)
    time.sleep(1.0)
    if not p.isOk():
        print("NO RX")
        return 1

    p.EnableArm(7, 0x02)
    time.sleep(1.2)
    p.MotionCtrl_2(0x01, 0x00, SPEED, 0x00)
    time.sleep(0.5)

    results = {}
    for label, degs in CANDIDATES.items():
        try:
            results[label] = survey(p, degs, label)
        except Exception as e:
            print(f"   {label}: FAILED {type(e).__name__}: {e}")

    print("\n=== summary ===")
    for label, (n, good) in sorted(results.items(), key=lambda kv: -kv[1][0]):
        print(f"  {n}/6  {label}  [{', '.join(good)}]")
    return 0


if __name__ == "__main__":
    sys.exit(main())
