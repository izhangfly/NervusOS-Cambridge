#!/usr/bin/env python3
"""
Back-to-back motion check. Moves +Z then back -Z so the arm ends where it
started, and reports the shim's TX health. Run it several times in a row:
this is the exact pattern that used to leave the adapter's OUT endpoint
stalled, with the arm reporting pose while ignoring every command.
"""
import sys
import time

sys.path.insert(0, str(__import__("pathlib").Path(__file__).resolve().parent))
import piper_mac_can
piper_mac_can.install()

from piper_sdk import C_PiperInterface_V2

MMM = 1000.0
STEP_MM = 20.0
SPEED = 20


def pose(p):
    e = p.GetArmEndPoseMsgs().end_pose
    return [e.X_axis / MMM, e.Y_axis / MMM, e.Z_axis / MMM,
            e.RX_axis / 1000.0, e.RY_axis / 1000.0, e.RZ_axis / 1000.0]


def move_to(p, target, settle=2.5):
    for _ in range(10):
        p.EndPoseCtrl(*[int(round(v * 1000)) for v in target])
        time.sleep(0.05)
    time.sleep(settle)


def main():
    p = C_PiperInterface_V2("candle", judge_flag=False)
    p.ConnectPort()
    for _ in range(60):
        if p.isOk():
            break
        time.sleep(0.1)
    time.sleep(1.0)

    bus = p._C_PiperInterface_V2__arm_can.bus
    if not p.isOk():
        print("NO RX — refusing to command motion.")
        return 1

    p.EnableArm(7, 0x02)
    time.sleep(1.2)
    p.MotionCtrl_2(0x01, 0x00, SPEED, 0x00)
    time.sleep(0.5)

    start = pose(p)
    up = list(start)
    up[2] += STEP_MM
    move_to(p, up)
    mid = pose(p)
    move_to(p, start)
    end = pose(p)

    d_up = mid[2] - start[2]
    d_back = end[2] - mid[2]
    ok = d_up > STEP_MM * 0.7 and d_back < -STEP_MM * 0.7
    print(f"  Z {start[2]:.1f} -> {mid[2]:.1f} -> {end[2]:.1f} "
          f"(up {d_up:+.1f}, back {d_back:+.1f})")
    print(f"  shim: rx={bus.rx_count} tx={bus.tx_count} echo={bus.echo_count} "
          f"txfail={bus.tx_fail_count} drop={bus.tx_drop_count} "
          f"heal={bus.stall_recovery_count} io_alive={bus._io.is_alive()}")
    print("  RESULT:", "MOVED BOTH WAYS" if ok else "*** MOTION FAILED ***")
    return 0 if ok else 1


if __name__ == "__main__":
    sys.exit(main())
