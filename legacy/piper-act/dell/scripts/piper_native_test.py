#!/usr/bin/env python3
"""Test python-can's NATIVE gs_usb backend (no shim) on Windows.

README "route 2": CreateCanBus with bustype="gs_usb" and a channel index.
Used to compare against the hand-rolled shim (piper_mac_can) for the
"second SDK connection goes deaf" problem.
"""

import sys
import time

from piper_sdk import C_PiperInterface_V2


def main():
    p = C_PiperInterface_V2(can_auto_init=False)
    p.CreateCanBus(can_name=0, bustype="gs_usb",
                   expected_bitrate=1000000, judge_flag=False)
    p.ConnectPort(piper_init=False)
    for _ in range(60):
        if p.isOk():
            break
        time.sleep(0.1)
    time.sleep(1.0)

    t1 = p.GetArmJointMsgs().time_stamp
    time.sleep(0.6)
    t2 = p.GetArmJointMsgs().time_stamp
    print("NATIVE gs_usb: isOk=%s ts_adv=%s" % (p.isOk(), t1 != t2))
    if p.isOk() and t1 != t2:
        j = p.GetArmJointMsgs().joint_state
        print("joints (deg):", [round(getattr(j, f"joint_{n}") / 1000.0, 2)
                                for n in range(1, 7)])


if __name__ == "__main__":
    main()
