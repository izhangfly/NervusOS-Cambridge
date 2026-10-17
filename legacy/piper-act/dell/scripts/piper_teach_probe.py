#!/usr/bin/env python3
"""Read the raw control/teach state to tell WHY the arm is ignoring commands."""
import sys
import time

sys.path.insert(0, str(__import__("pathlib").Path(__file__).resolve().parent))
import piper_mac_can
piper_mac_can.install()

from piper_sdk import C_PiperInterface_V2


def main():
    p = C_PiperInterface_V2("candle", judge_flag=False)
    p.ConnectPort(piper_init=False)
    for _ in range(50):
        if p.isOk():
            break
        time.sleep(0.1)

    s = p.GetArmStatus().arm_status   # ArmMsgFeedbackStatus
    ctrl = s.ctrl_mode                # CtrlMode enum
    arm = s.arm_status                # ArmStatus enum
    teach = s.teach_status            # TeachingState enum

    print("=== arm control/teach state ===")
    for label, e in (("ctrl_mode", ctrl), ("arm_status", arm), ("teach_status", teach)):
        name = getattr(e, "name", "?")
        val = getattr(e, "value", None)
        print(f"  {label:12} value=0x{val:02X}  {name}" if val is not None
              else f"  {label:12} = {e!r}")

    print("\n=== interpretation ===")
    cv = getattr(ctrl, "value", None)
    tv = getattr(teach, "value", None)
    if cv == 0x02:
        print("  ctrl_mode = TEACHING_MODE -> arm ignores ALL CAN motion commands.")
        if tv == 0x01:
            print("  teach_status = START_RECORDING -> the drag-teach button is")
            print("  latched ON (recording). Press the button between J5/J6 once")
            print("  to turn its light OFF and exit teaching mode.")
        elif tv == 0x02:
            print("  teach_status = STOP_RECORDING -> recording stopped but still in")
            print("  teaching mode; press the button to fully exit (light off).")
        else:
            print(f"  teach_status = 0x{tv:02X} (see TeachingState enum)")
    elif cv == 0x01:
        print("  ctrl_mode = CAN -> arm should accept commands.")
    else:
        print(f"  ctrl_mode = 0x{cv:02X} (unknown)")

    p.DisconnectPort()


if __name__ == "__main__":
    main()
