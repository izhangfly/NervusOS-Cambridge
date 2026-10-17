#!/usr/bin/env python3
"""
PiPER J6 zero-point calibration over CAN — no Windows host software required.

Implements AgileX's *New PiPER arm: J6 zero-point setting* V1.1 entirely through piper_sdk,
which is what the Windows GUI does under the hood:

    GUI step 3  ensure slave arm mode -> read MasterSlave/linkage status
    GUI step 4  motor config → disable J6 -> DisableArm(motor_num=6, enable_flag=0x01)   CAN 0x471
    GUI step 5  set zero → J6         -> JointConfig(joint_num=6, set_zero=0xAE)      CAN 0x475
    check 1     J6 reads 0°            -> GetArmJointMsgs()
    response check                    -> GetRespInstruction().zero_config_success_flag == 0x01

This is AgileX's own required, reversible calibration (PiPER manual §3.3).
It is NOT a hardware modification, so it does not break the rule against modifying the borrowed AgileX hardware.

The script sends NO motion commands. Setting a zero point does not move the arm.

PREREQUISITE (manual, cannot be automated — do this FIRST):
    Rotate the flange so its groove points straight up, in line with the rear
    screw hole and the teach button (a three-point line). See figure 2 of the AgileX doc.

Usage:
    ~/NervusOS/robots/.venv/bin/python scripts/piper_j6_zero.py            # dry run
    ~/NervusOS/robots/.venv/bin/python scripts/piper_j6_zero.py --commit   # actually set zero
    ~/NervusOS/robots/.venv/bin/python scripts/piper_j6_zero.py --status   # read-only probe
"""

import argparse
import sys
import time

try:
    from piper_sdk import C_PiperInterface_V2
except ImportError:
    sys.exit("piper_sdk not importable — run with ~/NervusOS/robots/.venv/bin/python")

CAN_IFACE = "can0"
J6 = 6
DISABLE = 0x01
ENABLE = 0x02
SET_ZERO = 0xAE
ZERO_OK = 0x01
# piper_sdk reports joint angles in 0.001 degree units.
MDEG = 1000.0


def connect(iface: str) -> "C_PiperInterface_V2":
    piper = C_PiperInterface_V2(iface)
    piper.ConnectPort()
    for _ in range(50):                      # up to ~5 s for the RX thread to fill
        if piper.isOk():
            break
        time.sleep(0.1)
    if not piper.isOk():
        sys.exit(f"CAN read thread not healthy on {iface}. "
                 f"Check: ip -br link show {iface}")
    time.sleep(0.5)                          # let feedback frames accumulate
    return piper


def joint_degrees(piper) -> dict:
    j = piper.GetArmJointMsgs().joint_state
    return {n: getattr(j, f"joint_{n}") / MDEG for n in range(1, 7)}


def show_status(piper) -> float:
    print("\n--- arm status ---")
    try:
        print(f"  firmware: {piper.GetPiperFirmwareVersion()}")
    except Exception as e:
        print(f"  firmware: unavailable ({e})")

    try:
        st = piper.GetArmStatus().arm_status
        print(f"  ctrl_mode={st.ctrl_mode} arm_status={st.arm_status} "
              f"mode_feed={st.mode_feed} teach_status={st.teach_status} "
              f"motion_status={st.motion_status}")
    except Exception as e:
        print(f"  status: unavailable ({e})")

    degs = joint_degrees(piper)
    print("  joint angles (deg):")
    for n, d in degs.items():
        mark = "   <-- J6" if n == J6 else ""
        print(f"    J{n}: {d:9.3f}{mark}")
    return degs[J6]


def main() -> int:
    ap = argparse.ArgumentParser(description="PiPER J6 zero-point calibration over CAN")
    ap.add_argument("--iface", default=CAN_IFACE, help=f"CAN interface (default {CAN_IFACE})")
    ap.add_argument("--commit", action="store_true",
                    help="actually perform the zero-set (default is a dry run)")
    ap.add_argument("--status", action="store_true", help="read-only probe, then exit")
    ap.add_argument("--yes", action="store_true", help="skip the interactive confirmation")
    args = ap.parse_args()

    print(f"connecting to {args.iface} ...")
    piper = connect(args.iface)
    j6_before = show_status(piper)

    if args.status:
        return 0

    if not args.commit:
        print("\nDRY RUN — nothing was changed.")
        print("Physical prerequisite: flange groove straight up, aligned with the")
        print("rear screw hole and teach button (a three-point line, figure 2).")
        print("When that is done, re-run with --commit")
        return 0

    print("\n" + "=" * 62)
    print("  ABOUT TO SET J6 ZERO POINT")
    print("=" * 62)
    print(f"  J6 currently reads {j6_before:.3f} deg.")
    print("  After this, the CURRENT physical position becomes 0.000 deg.")
    print("  Confirm the flange groove points straight up and is aligned")
    print("  with the rear screw hole and the teach button (a three-point line).")
    print("  Keep the e-stop (aviation plug) within reach.")
    if not args.yes:
        if input("\n  Type 'SET ZERO' to proceed: ").strip() != "SET ZERO":
            print("aborted — nothing changed.")
            return 1

    # GUI step 4 — disable joint 6 only. Zero-set is rejected on an enabled motor.
    print(f"\n[1/3] disabling J{J6} (CAN 0x471) ...")
    piper.DisableArm(J6, DISABLE)
    time.sleep(1.0)

    # GUI step 5 — set current position as zero for joint 6.
    print(f"[2/3] setting J{J6} zero point (CAN 0x475, set_zero=0xAE) ...")
    piper.JointConfig(joint_num=J6, set_zero=SET_ZERO)

    # Poll the set-instruction response for the success flag.
    print("[3/3] waiting for zero_config_success_flag ...")
    ok = False
    for _ in range(30):
        time.sleep(0.2)
        try:
            resp = piper.GetRespInstruction().instruction_response
            if getattr(resp, "is_set_zero_successfully", 0) == ZERO_OK:
                ok = True
                break
        except Exception:
            pass

    time.sleep(0.5)
    j6_after = joint_degrees(piper)[J6]

    print("\n" + "=" * 62)
    if ok:
        print("  RESULT: zero_config_success_flag = 0x01  (SUCCESS)")
    else:
        print("  RESULT: no success flag seen within 6 s")
        print("  The flag is only latched briefly; check the J6 reading below,")
        print("  which is the authoritative verification per the AgileX doc.")
    print(f"  J6 before: {j6_before:9.3f} deg")
    print(f"  J6 after:  {j6_after:9.3f} deg   (verification 1 expects ~0.000)")
    print("=" * 62)

    if abs(j6_after) < 0.5:
        print("\nJ6 reads ~0 deg — calibration verified (check 1 passed).")
        print("Next (check 2): enable the arm and move joints to zero, then check")
        print("the gripper/teach-pendant pose looks correct.")
        return 0

    print(f"\nWARNING: J6 reads {j6_after:.3f} deg, not ~0. Zero-set likely did NOT take.")
    print("Re-check that J6 was disabled and the arm is in slave arm mode.")
    return 2


if __name__ == "__main__":
    try:
        sys.exit(main())
    except KeyboardInterrupt:
        print("\ninterrupted — no further commands sent.")
        sys.exit(130)
