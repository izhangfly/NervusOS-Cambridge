#!/usr/bin/env python3
"""
Connect to an AgileX arm (NERO 7-DOF or PiPER 6-DOF) from any platform.

One SDK (pyAgxArm) drives both arms. The same code runs on:
  macOS  -> interface="slcan",     channel="/dev/tty.usbmodem*"  (auto-detected)
  Linux  -> interface="socketcan", channel="can0"                (RK3588 / RDK S100)
  Windows-> interface="agx_cando", channel="0"

Default is READ-ONLY: connects, prints state, disconnects. It never commands
motion, so first contact with real hardware cannot move the arm.

Usage:
    python scripts/arm_connect.py nero
    python scripts/arm_connect.py piper
    python scripts/arm_connect.py nero --channel /dev/tty.usbmodem1101
    python scripts/arm_connect.py nero --list      # list candidate serial ports

Both arms run CAN2.0B at 1 Mbps, so they CANNOT share one bus.
Use one USB-CAN adapter per arm.
"""

from __future__ import annotations

import argparse
import glob
import platform
import sys

from pyAgxArm import AgxArmFactory, ArmModel, create_agx_arm_config

# A CAN adapter in SLCAN mode enumerates under these names on macOS.
# (AgileX docs say /dev/ttyACM0, which is Linux naming — macOS differs.)
MAC_PORT_GLOBS = (
    "/dev/tty.usbmodem*",
    "/dev/tty.usbserial*",
    "/dev/tty.SLAB_USBtoUART*",
)

ARMS = {"nero": ArmModel.NERO, "piper": ArmModel.PIPER}


def find_mac_ports() -> list[str]:
    ports: list[str] = []
    for pattern in MAC_PORT_GLOBS:
        ports.extend(sorted(glob.glob(pattern)))
    return ports


def resolve_link(channel: str | None) -> tuple[str, str]:
    """Return (interface, channel) appropriate for the current OS."""
    system = platform.system()

    if system == "Linux":
        return "socketcan", channel or "can0"

    if system == "Windows":
        return "agx_cando", channel or "0"

    if system != "Darwin":
        sys.exit(f"Unsupported platform: {system}")

    if channel:
        return "slcan", channel

    ports = find_mac_ports()
    if not ports:
        sys.exit(
            "No CAN adapter found.\n"
            "  Plug in a USB-CAN adapter running SLCAN firmware, then retry.\n"
            "  Note: gs_usb / candleLight firmware does NOT work on macOS —\n"
            "  that needs Linux SocketCAN. Use SLCAN firmware here.\n"
            f"  Looked for: {', '.join(MAC_PORT_GLOBS)}"
        )
    if len(ports) > 1:
        print(f"Multiple ports found, using the first: {ports}")
    return "slcan", ports[0]


def probe(robot) -> None:
    """Read-only interrogation. Not every model exposes every getter."""
    checks = (
        ("joint count", "joint_nums"),
        ("firmware", "get_firmware"),
        ("arm status", "get_arm_status"),
        ("joint angles (rad)", "get_joint_angles"),
        ("flange pose", "get_flange_pose"),
        ("enabled", "get_joints_enable_status_list"),
    )
    for label, attr in checks:
        probe_fn = getattr(robot, attr, None)
        if probe_fn is None:
            continue
        try:
            value = probe_fn() if callable(probe_fn) else probe_fn
        except Exception as exc:  # noqa: BLE001 - report, don't abort the sweep
            value = f"<error: {exc}>"
        print(f"{label:>20}: {value}")


def main() -> None:
    parser = argparse.ArgumentParser(description="Connect to an AgileX NERO or PiPER arm.")
    parser.add_argument("arm", choices=sorted(ARMS), help="which arm to connect to")
    parser.add_argument("--channel", help="override CAN channel / serial path")
    parser.add_argument(
        "--firmware",
        default="default",
        help='firmware profile: "default", "v111", "v112", "v120"',
    )
    parser.add_argument("--list", action="store_true", help="list candidate serial ports and exit")
    args = parser.parse_args()

    if args.list:
        ports = find_mac_ports() if platform.system() == "Darwin" else []
        print("\n".join(ports) if ports else "(no candidate serial ports found)")
        return

    interface, channel = resolve_link(args.channel)
    print(f"platform : {platform.system()}")
    print(f"interface: {interface}")
    print(f"channel  : {channel}")
    print(f"arm      : {args.arm}\n")

    config = create_agx_arm_config(
        robot=ARMS[args.arm],
        firmeware_version=args.firmware,  # SDK spells it this way
        channel=channel,
        interface=interface,
    )
    robot = AgxArmFactory.create_arm(config)

    robot.connect()
    print("connected.\n")
    try:
        probe(robot)
        if robot.has_comm_error():
            print(f"\ncomm error: {robot.get_comm_error()}")
        else:
            print("\nno communication errors.")
    finally:
        robot.disconnect()
        print("disconnected.")


if __name__ == "__main__":
    main()
