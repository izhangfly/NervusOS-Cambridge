#!/usr/bin/env python3
"""Diagnose the recovery path for a latched arm fault (e.g. TARGET_POS_EXCEEDS_LIMIT)."""
import sys
import time

sys.path.insert(0, str(__import__("pathlib").Path(__file__).resolve().parent))
import piper_mac_can
piper_mac_can.install()

from piper_sdk import C_PiperInterface_V2


def status(p):
    s = p.GetArmStatus().arm_status
    lo = p.GetArmLowSpdInfoMsgs()
    motors = [bool(getattr(lo, f"motor_{n}").foc_status.driver_enable_status) for n in range(1, 7)]
    return s, motors


def main():
    p = C_PiperInterface_V2("candle", judge_flag=False)
    p.ConnectPort()
    for _ in range(60):
        if p.isOk():
            break
        time.sleep(0.1)
    time.sleep(1.0)

    s, m = status(p)
    print("BEFORE            : arm_status=%s  motors=%s" % (s, m))

    p.EmergencyStop(0x02)  # 0x02 = resume / clear estop latch
    time.sleep(1.0)
    s, m = status(p)
    print("after estop resume: arm_status=%s  motors=%s" % (s, m))

    p.EnableArm(7, 0x02)
    time.sleep(1.2)
    p.MotionCtrl_2(0x01, 0x00, 15, 0x00)
    time.sleep(0.5)
    s, m = status(p)
    print("after enable+mode : arm_status=%s  motors=%s" % (s, m))


if __name__ == "__main__":
    main()
