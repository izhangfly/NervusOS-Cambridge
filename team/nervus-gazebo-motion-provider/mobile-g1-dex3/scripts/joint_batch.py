#!/usr/bin/env python3
import argparse
import json
import math
import re
import sys
import time
import xml.etree.ElementTree as ET
from pathlib import Path
from typing import Any


PRIMARY_GROUPS = ("waist", "left_arm", "right_arm", "left_hand", "right_hand")
SAFE_NAME = re.compile(r"^[A-Za-z][A-Za-z0-9_]*$")


def default_paths() -> tuple[Path, Path]:
    variant_root = Path(__file__).resolve().parent.parent
    return (
        variant_root / "config" / "native-joint-control.json",
        variant_root
        / "simulation"
        / "models"
        / "nervus_mobile_g1_dex3"
        / "source"
        / "nervus_mobile_g1_dex3.urdf",
    )


def load_control_definition(config_path: Path, urdf_path: Path) -> dict[str, Any]:
    config = json.loads(config_path.read_text(encoding="utf-8"))
    if config.get("schema_version") != 1:
        raise ValueError("unsupported native joint-control schema")
    model = config.get("model")
    if not isinstance(model, str) or not SAFE_NAME.fullmatch(model):
        raise ValueError("invalid model name")
    if config.get("message_type") != "gz.msgs.Double":
        raise ValueError("native joint commands must use gz.msgs.Double")
    if config.get("topic_template") != "/model/{model}/joint/{joint}/0/cmd_pos":
        raise ValueError("unexpected joint command topic template")

    groups = config.get("groups")
    if not isinstance(groups, dict) or any(name not in groups for name in PRIMARY_GROUPS):
        raise ValueError("missing primary joint group")
    primary: list[str] = []
    for group_name in PRIMARY_GROUPS:
        joints = groups[group_name]
        if not isinstance(joints, list) or not joints:
            raise ValueError(f"invalid joint group {group_name}")
        if any(not isinstance(joint, str) or not SAFE_NAME.fullmatch(joint) for joint in joints):
            raise ValueError(f"invalid joint name in group {group_name}")
        primary.extend(joints)
    if len(primary) != 31 or len(set(primary)) != 31:
        raise ValueError("primary groups must contain exactly 31 unique joints")
    if groups.get("all_upper_body") != primary:
        raise ValueError("all_upper_body must exactly preserve primary group order")

    root = ET.parse(urdf_path).getroot()
    limits: dict[str, tuple[float, float]] = {}
    for joint in root.findall("joint"):
        name = joint.get("name", "")
        if name not in set(primary):
            continue
        limit = joint.find("limit")
        if limit is None or limit.get("lower") is None or limit.get("upper") is None:
            raise ValueError(f"missing URDF position limit for {name}")
        limits[name] = (float(limit.get("lower", "nan")), float(limit.get("upper", "nan")))
    if set(limits) != set(primary):
        missing = sorted(set(primary) - set(limits))
        raise ValueError(f"control definition references unknown URDF joints: {missing}")

    states = config.get("states")
    if not isinstance(states, dict) or set(states) != {"home", "probe"}:
        raise ValueError("home and probe states are required")
    for state_name, targets in states.items():
        if not isinstance(targets, dict) or set(targets) != set(primary):
            raise ValueError(f"state {state_name} must define all 31 joints")
        for joint, raw_target in targets.items():
            target = float(raw_target)
            lower, upper = limits[joint]
            if not math.isfinite(target) or target < lower or target > upper:
                raise ValueError(
                    f"state {state_name} target for {joint} is outside [{lower}, {upper}]"
                )
    tolerances = config.get("tolerance_rad")
    if not isinstance(tolerances, dict) or set(tolerances) != set(PRIMARY_GROUPS):
        raise ValueError("one tolerance is required for every primary group")
    if any(not 0 < float(value) <= 0.1 for value in tolerances.values()):
        raise ValueError("joint tolerances must be in (0, 0.1] rad")

    config["limits"] = limits
    config["joint_to_group"] = {
        joint: group for group in PRIMARY_GROUPS for joint in groups[group]
    }
    return config


def command_topics(config: dict[str, Any], joints: list[str]) -> list[str]:
    return [
        config["topic_template"].format(model=config["model"], joint=joint)
        for joint in joints
    ]


def publish_batch(config: dict[str, Any], state_name: str, group_name: str) -> None:
    try:
        from gz.msgs.double_pb2 import Double
        from gz.transport import Node
    except ImportError as error:
        raise RuntimeError("Gazebo Transport Python bindings are unavailable") from error

    groups = config["groups"]
    states = config["states"]
    if state_name not in states:
        raise ValueError(f"unknown state {state_name}")
    if group_name not in groups:
        raise ValueError(f"unknown group {group_name}")
    joints = groups[group_name]
    topics = command_topics(config, joints)

    # Keep all publishers alive in one native Gazebo Transport process. Wait
    # for every controller connection before sending the first command, so a
    # missing subscriber cannot produce a partially validated batch.
    node = Node()
    publishers = [node.advertise(topic, Double()) for topic in topics]
    if any(not publisher.valid() for publisher in publishers):
        raise RuntimeError("Gazebo Transport rejected a joint command publisher")
    deadline = time.monotonic() + 5.0
    while not all(publisher.has_connections() for publisher in publishers):
        if time.monotonic() >= deadline:
            missing = [
                topic
                for topic, publisher in zip(topics, publishers, strict=True)
                if not publisher.has_connections()
            ]
            raise RuntimeError(f"joint command topics have no subscribers: {missing}")
        time.sleep(0.02)

    for joint, publisher in zip(joints, publishers, strict=True):
        message = Double()
        message.data = float(states[state_name][joint])
        if not publisher.publish(message):
            raise RuntimeError(f"Gazebo Transport rejected command for {joint}")
    time.sleep(0.10)
    print(f"published state={state_name} group={group_name} joints={len(joints)}")


def main() -> int:
    config_default, urdf_default = default_paths()
    parser = argparse.ArgumentParser(description="Test-only native Gazebo joint batch publisher")
    parser.add_argument("--config", type=Path, default=config_default)
    parser.add_argument("--urdf", type=Path, default=urdf_default)
    parser.add_argument("--state", choices=("home", "probe"), required=True)
    parser.add_argument("--group", default="all_upper_body")
    parser.add_argument("--validate-only", action="store_true")
    args = parser.parse_args()

    try:
        config = load_control_definition(args.config, args.urdf)
        if args.group not in config["groups"]:
            raise ValueError(f"unknown group {args.group}")
        if not args.validate_only:
            publish_batch(config, args.state, args.group)
        else:
            print(
                f"validated state={args.state} group={args.group} "
                f"joints={len(config['groups'][args.group])}"
            )
        return 0
    except (OSError, ValueError, RuntimeError) as error:
        print(f"joint batch failed: {error}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
