#!/usr/bin/env python3
"""
Which Cartesian axes can actually be jogged, and from where?

Homes to a known pose before each axis so one axis's fault cannot poison the
next, jogs that axis alone, and reports the delta plus the arm's status. Also
retries the rotation axes from a deliberately non-singular wrist orientation,
because the joint-zero home sits at RY~85 deg — right at Euler gimbal lock,
where RX/RZ are degenerate and every EndPoseCtrl target is unreachable.
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


def pose(p):
    e = p.GetArmEndPoseMsgs().end_pose
    return [e.X_axis / MMM, e.Y_axis / MMM, e.Z_axis / MMM,
            e.RX_axis / MDEG, e.RY_axis / MDEG, e.RZ_axis / MDEG]


def status(p):
    st = p.GetArmStatus().arm_status.arm_status
    return getattr(st, "name", "?")


def home(p):
    p.ModeCtrl(0x01, 0x01, 30, 0x00)      # CAN + MOVE J
    time.sleep(0.4)
    p.JointCtrl(0, 0, 0, 0, 0, 0)
    time.sleep(4.5)
    p.MotionCtrl_2(0x01, 0x00, SPEED, 0x00)   # back to MOVE P
    time.sleep(0.5)


def jog(p, idx, direction, secs=1.5):
    """Stream actual-pose + lead on one axis, exactly like the teleop leash."""
    t0 = time.time()
    while time.time() - t0 < secs:
        tgt = pose(p)
        tgt[idx] += direction * (LEAD_MM if idx < 3 else LEAD_DEG)
        p.EndPoseCtrl(*[int(round(v * 1000)) for v in tgt])
        time.sleep(1.0 / STREAM_HZ)
    time.sleep(0.8)


def trial(p, idx, label, prep=None):
    home(p)
    if prep:
        prep(p)
    before, s0 = pose(p), status(p)
    jog(p, idx, +1)
    after, s1 = pose(p), status(p)
    d = after[idx] - before[idx]
    unit = "mm" if idx < 3 else "deg"
    ok = abs(d) > (2.0 if idx < 3 else 1.0)
    print(f"  {label:<22} d={d:+7.1f} {unit:<3}  {s0} -> {s1}  "
          f"{'OK' if ok else 'NO MOTION'}")
    return ok


def tilt_wrist(p):
    """Pitch the wrist away from RY~85 so the Euler angles are well conditioned."""
    tgt = pose(p)
    tgt[4] = 40.0          # RY well clear of +-90
    for _ in range(25):
        p.EndPoseCtrl(*[int(round(v * 1000)) for v in tgt])
        time.sleep(0.05)
    time.sleep(2.0)
    print(f"    (wrist tilted to RY={pose(p)[4]:.1f}, status {status(p)})")


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

    print("\nFrom the joint-zero home pose:")
    for i, ax in enumerate(AXES):
        trial(p, i, ax)

    print("\nRotation axes from a non-singular wrist (RY=40):")
    for i in (3, 4, 5):
        trial(p, i, AXES[i] + " (tilted)", prep=tilt_wrist)

    print("\nreturning home")
    home(p)
    return 0


if __name__ == "__main__":
    sys.exit(main())
