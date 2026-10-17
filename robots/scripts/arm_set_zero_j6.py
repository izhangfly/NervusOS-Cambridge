#!/usr/bin/env python3
"""
Set the AgileX PiPER J6 joint zero point — from macOS (or Linux / Windows).

AgileX documents this procedure *only* through the Windows-only ArmRobotTool
GUI (slave arm mode → motor configuration → disable joint 6 → set zero → confirm). That GUI does not run
on macOS. This script does the identical thing over CAN via pyAgxArm:

    disable(6)  →  [you align the flange by hand]  →  calibrate_joint(6)  →  enable(6)

`calibrate_joint(6)` sends JointConfig(joint=6, set_zero=0xAE) and returns True
ONLY when the arm reports is_set_zero_successfully — so success here is the arm's
own confirmation, not just "the CAN frame went out".

Why this is allowed on borrowed hardware: PiPER manual §3.3 states a new arm
MUST have J6 zero set after the flange is installed. It is AgileX's own required
calibration, it writes a stored offset, and it is fully *reversible* — re-run
this any time. It cuts no wires and adds no parts; it is not a modification.

The GUI's "slave arm mode" step is a teach-pendant leader/follower concept.
Driving over CAN, we ARE the master issuing the command, so it does not apply.

────────────────────────────────────────────────────────────────────────────
BEFORE you run this
  1. Mount the arm (4×M5), install the J6 limit screw (M3×4) and the flange
     (manual §3.1.1 / §3.1.3).
  2. Wire CAN (the aviation plug) → USB-CAN adapter → this computer, and 24 V power, in the
     A→G order of §3.2.2. The base panel LED should be BLINKING GREEN.
     (Solid green or blinking red = fault — fix before continuing.)
  3. Confirm the arm answers first:  ./.venv/bin/python scripts/arm_connect.py piper

DURING (this script walks you through it)
  • It disables J6 → the wrist goes limp. By hand, rotate the flange so its
    groove points straight UP, in a three-point line with the rear
    screw hole and the teach button. Hold it there.
  • Press Enter → it writes the zero, re-enables J6, and reads J6 back (≈ 0°).

SAFETY
  • A disabled J6 is free-swinging. It is light, but support it with your hand.
  • Keep clear of the whole arm and keep the power plug within reach — pulling
    it is the hard e-stop (§7.3). This script never commands arm motion.
"""

from __future__ import annotations

import argparse
import math
import sys

from pyAgxArm import AgxArmFactory, ArmModel, create_agx_arm_config

# Reuse the platform/port resolution already proven in arm_connect.py.
# (Same directory, so this import is on sys.path when run as a script.)
from arm_connect import resolve_link

ARMS = {"piper": ArmModel.PIPER, "nero": ArmModel.NERO}


def read_joint_deg(robot, index: int) -> float | None:
    """Return joint `index` (1-based) angle in degrees, or None if unavailable."""
    try:
        angles = robot.get_joint_angles().msg
        return math.degrees(float(angles[index - 1]))
    except Exception as exc:  # noqa: BLE001 - informational read
        print(f"  (could not read joint angle: {exc})")
        return None


def confirm(prompt: str) -> bool:
    try:
        return input(prompt).strip().lower() in ("", "y", "yes")
    except EOFError:
        return False


def main() -> None:
    parser = argparse.ArgumentParser(
        description="Set the PiPER J6 (or any joint) zero point over CAN, from any OS.",
    )
    parser.add_argument("arm", nargs="?", default="piper", choices=sorted(ARMS),
                        help="which arm (default: piper)")
    parser.add_argument("--joint", type=int, default=6, choices=range(1, 7), metavar="1-6",
                        help="joint to zero (default: 6 — the one disturbed by flange install)")
    parser.add_argument("--channel", help="override CAN channel / serial path")
    parser.add_argument("--firmware", default="default", help='"default", "v111", "v112", "v120"')
    parser.add_argument("--yes", action="store_true",
                        help="skip the interactive alignment pause (NOT recommended)")
    args = parser.parse_args()

    interface, channel = resolve_link(args.channel)
    j = args.joint
    print(f"platform : {sys.platform}")
    print(f"interface: {interface}")
    print(f"channel  : {channel}")
    print(f"arm      : {args.arm}")
    print(f"joint    : J{j}\n")

    print("⚠  This DISABLES the joint (it goes limp) and then writes a new zero.")
    print("   Make sure the flange/limit screw is installed and the base LED blinks green.")
    if not args.yes and not confirm("   Continue? [Y/n] "):
        sys.exit("aborted.")

    config = create_agx_arm_config(
        robot=ARMS[args.arm],
        firmeware_version=args.firmware,  # SDK spells it this way
        channel=channel,
        interface=interface,
    )
    robot = AgxArmFactory.create_arm(config)
    robot.connect()
    print("\nconnected.")

    try:
        before = read_joint_deg(robot, j)
        if before is not None:
            print(f"J{j} now reads: {before:+.3f}°")

        # 1. Disable just this joint so it can be positioned by hand.
        print(f"\ndisabling J{j} ...")
        robot.disable(j)
        print(f"J{j} disabled — it is now FREE. Support it by hand.")

        # 2. Human aligns the flange to the mechanical zero.
        print("\nAlign the flange groove straight UP — three-point line")
        print("with the rear screw hole and the teach button. Hold it there.")
        if not args.yes and not confirm("Press Enter to write the zero (or 'n' to abort): "):
            print("aborting — re-enabling joint, no zero written.")
            robot.enable(j)
            sys.exit("aborted, nothing changed.")

        # 3. Write current position as this joint's zero. True == arm confirmed.
        print(f"\nwriting zero for J{j} ...")
        ok = robot.calibrate_joint(j)
        if not ok:
            print(f"✗ arm did NOT confirm the zero-set for J{j}.")
            print("  Re-enabling the joint. Nothing was stored.")
            print("  If this persists, the firmware may need CAN command mode set first;")
            print("  fall back to piper_sdk demo/V2/piper_set_joint_zero.py.")
            robot.enable(j)
            sys.exit(1)
        print(f"✓ J{j} zero stored (arm confirmed is_set_zero_successfully).")

        # 4. Re-enable and verify.
        robot.enable(j)
        after = read_joint_deg(robot, j)
        if after is not None:
            print(f"J{j} now reads: {after:+.3f}°  (expect ≈ 0)")
            if abs(after) > 1.0:
                print("  ⚠ not close to 0 — the flange may have moved during the write.")
                print("    Re-run and hold the alignment steady while pressing Enter.")

        print("\nDone. To sanity-check the whole arm, enable all joints and home it")
        print("(move_j([0]*6)); the pose should match manual §4.4.1. Keep the area clear.")
    finally:
        robot.disconnect()
        print("disconnected.")


if __name__ == "__main__":
    main()
