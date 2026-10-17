#!/usr/bin/env python3
"""Does EnableArm actually flip the arm into CAN control mode?

Reads ctrl_mode, sends the enable sequence, reads ctrl_mode again. If it stays
STANDBY (0x00), commands are not reaching the arm (TX stall) or it refuses.
"""
import sys
import time

sys.path.insert(0, str(__import__("pathlib").Path(__file__).resolve().parent))
import piper_mac_can
piper_mac_can.install()

from piper_sdk import C_PiperInterface_V2


def ctrl(p):
    s = p.GetArmStatus().arm_status
    c = s.ctrl_mode
    return getattr(c, "name", "?"), getattr(c, "value", None)


def main():
    p = C_PiperInterface_V2("candle", judge_flag=False)
    p.ConnectPort(piper_init=False)
    for _ in range(50):
        if p.isOk():
            break
        time.sleep(0.1)

    bus = p._C_PiperInterface_V2__arm_can.bus
    print(f"bus: {type(bus).__name__} | io_alive={bus._io.is_alive()} | "
          f"tx_count={bus.tx_count} tx_fail={bus.tx_fail_count} heal={bus.stall_recovery_count}")

    name, val = ctrl(p)
    print(f"before enable: ctrl_mode = {name} (0x{val:02X})")

    print("sending EnableArm(7, 0x02)...")
    p.EnableArm(7, 0x02)
    time.sleep(1.5)
    print("sending MotionCtrl_2(0x01, 0x00, 30, 0x00)...")
    p.MotionCtrl_2(0x01, 0x00, 30, 0x00)
    time.sleep(1.5)

    name, val = ctrl(p)
    print(f"after enable : ctrl_mode = {name} (0x{val:02X})")
    print(f"bus after: tx_count={bus.tx_count} tx_fail={bus.tx_fail_count} "
          f"heal={bus.stall_recovery_count} io_alive={bus._io.is_alive()} "
          f"err={bus.last_error}")

    if val == 0x01:
        print("\nRESULT: arm entered CAN_CTRL — enable works, TX is fine.")
    elif val == 0x00:
        print("\nRESULT: arm STAYED in STANDBY — EnableArm is NOT taking effect.")
        if bus.tx_fail_count > 0 or bus.tx_count == 0:
            print("        TX looks stalled (frames not going out).")
        else:
            print("        TX went out but the arm did not enter CAN mode.")
    else:
        print(f"\nRESULT: ctrl_mode = 0x{val:02X} ({name})")

    p.DisconnectPort()


if __name__ == "__main__":
    main()
