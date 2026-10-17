#!/usr/bin/env python3
import argparse
import json
import math
import sys
import time
from pathlib import Path
from typing import Any

from single_hand_grasp import (
    ContactMonitor,
    JointMonitor,
    NativeJointCommander,
    PoseMonitor,
    add,
    distance,
    rotate,
    subtract,
)


def default_config() -> Path:
    return Path(__file__).resolve().parent.parent / "config" / "bimanual-mobile-manipulation.json"


def yaw(quaternion: tuple[float, float, float, float]) -> float:
    x, y, z, w = quaternion
    return math.atan2(2.0 * (w * z + x * y), 1.0 - 2.0 * (y * y + z * z))


def load_config(path: Path) -> dict[str, Any]:
    config = json.loads(path.read_text(encoding="utf-8"))
    if config.get("schema_version") != 1:
        raise ValueError("unsupported bimanual configuration schema")
    for state in ("open", "closed"):
        if not isinstance(config.get(state), dict) or len(config[state]) != 14:
            raise ValueError(f"{state} must define fourteen Dex3 joints")
    if set(config["open"]) != set(config["closed"]):
        raise ValueError("open and closed states must define identical joints")
    for state in ("lift_home", "lifted"):
        if set(config.get(state, {})) != {
            "left_shoulder_pitch_joint",
            "right_shoulder_pitch_joint",
        }:
            raise ValueError(f"{state} must define both shoulder pitch joints")
    if any(not -0.40 <= float(value) <= 0.0 for state in ("lift_home", "lifted") for value in config[state].values()):
        raise ValueError("bimanual shoulder targets exceed the accepted workspace")
    expected_roll_joints = {
        "left_shoulder_roll_joint",
        "right_shoulder_roll_joint",
    }
    for state in ("separation_home", "separated"):
        if set(config.get(state, {})) != expected_roll_joints:
            raise ValueError(f"{state} must define both shoulder roll joints")
    separated = config["separated"]
    if not 0.0 < float(separated["left_shoulder_roll_joint"]) <= 0.30:
        raise ValueError("left shoulder separation target must move outward")
    if not -0.30 <= float(separated["right_shoulder_roll_joint"]) < 0.0:
        raise ValueError("right shoulder separation target must move outward")
    for field in (
        "close_seconds",
        "close_settle_seconds",
        "lift_seconds",
        "precarry_hold_seconds",
        "lower_seconds",
        "open_seconds",
        "open_feedback_timeout_seconds",
        "maximum_open_joint_error_rad",
        "separation_seconds",
        "base_velocity_m_s",
        "base_slow_velocity_m_s",
        "base_travel_m",
        "base_slow_distance_m",
        "base_timeout_seconds",
        "minimum_lift_m",
        "maximum_relative_drift_m",
        "minimum_wrist_separation_m",
        "minimum_midline_clearance_m",
    ):
        value = float(config.get(field, 0))
        if not math.isfinite(value) or value <= 0:
            raise ValueError(f"{field} must be positive and finite")
    return config


class BaseCommander:
    def __init__(self, node: Any, topic: str) -> None:
        try:
            from gz.msgs.twist_pb2 import Twist
        except ImportError as error:
            raise RuntimeError("Gazebo Twist bindings are unavailable") from error
        self._message_type = Twist
        self._publisher = node.advertise(topic, Twist())
        if not self._publisher.valid():
            raise RuntimeError("Gazebo Transport rejected base publisher")

    def wait_for_connection(self, timeout: float) -> None:
        deadline = time.monotonic() + timeout
        while not self._publisher.has_connections():
            if time.monotonic() >= deadline:
                raise RuntimeError("base command topic has no subscriber")
            time.sleep(0.05)

    def publish(self, linear_x: float) -> None:
        message = self._message_type()
        message.linear.x = float(linear_x)
        if not self._publisher.publish(message):
            raise RuntimeError("Gazebo Transport rejected base command")

    def stop(self) -> None:
        for _ in range(5):
            self.publish(0.0)
            time.sleep(0.02)

    def close(self) -> None:
        self._publisher = None


def run(config: dict[str, Any]) -> None:
    try:
        from gz.msgs.contacts_pb2 import Contacts
        from gz.msgs.model_pb2 import Model
        from gz.msgs.pose_v_pb2 import Pose_V
    except ImportError as error:
        raise RuntimeError("Gazebo observation bindings are unavailable") from error

    model_name = str(config["model"])
    object_name = str(config["object"])
    left_wrist_name = str(config["left_wrist_link"])
    right_wrist_name = str(config["right_wrist_link"])
    open_targets = {name: float(value) for name, value in config["open"].items()}
    closed_targets = {name: float(value) for name, value in config["closed"].items()}
    lift_home = {name: float(value) for name, value in config["lift_home"].items()}
    lifted = {name: float(value) for name, value in config["lifted"].items()}
    separation_home = {name: float(value) for name, value in config["separation_home"].items()}
    separated = {name: float(value) for name, value in config["separated"].items()}
    all_joints = list(open_targets) + list(lift_home) + list(separation_home)

    commander = NativeJointCommander(model_name, str(config["topic_template"]), all_joints)
    base = BaseCommander(commander.node, str(config["base_topic"]))
    poses = PoseMonitor()
    joints = JointMonitor()
    left_contacts = ContactMonitor()
    right_contacts = ContactMonitor()
    subscriptions = (
        commander.subscribe(Pose_V, str(config["pose_topic"]), poses.callback),
        commander.subscribe(Model, str(config["joint_state_topic"]), joints.callback),
        commander.subscribe(Contacts, str(config["left_contact_topic"]), left_contacts.callback),
        commander.subscribe(Contacts, str(config["right_contact_topic"]), right_contacts.callback),
    )
    if not all(subscriptions):
        raise RuntimeError("Gazebo Transport rejected a bimanual observation subscription")
    commander.wait_for_connections(12.0)
    base.wait_for_connection(12.0)
    poses.wait_for((model_name, object_name, left_wrist_name, right_wrist_name), 12.0)
    joints.wait_for(tuple(all_joints), 12.0)

    def world_link(name: str) -> tuple[float, float, float]:
        return add(
            poses.get(model_name),
            rotate(poses.get_orientation(model_name), poses.get(name)),
        )

    def wrists() -> tuple[tuple[float, float, float], tuple[float, float, float]]:
        return world_link(left_wrist_name), world_link(right_wrist_name)

    def require_workspace(stage: str) -> tuple[tuple[float, float, float], tuple[float, float, float]]:
        left, right = wrists()
        separation = distance(left, right)
        clearance = float(config["minimum_midline_clearance_m"])
        if separation < float(config["minimum_wrist_separation_m"]):
            raise RuntimeError(f"{stage} wrist separation is only {separation:.4f} m")
        if left[1] < clearance or right[1] > -clearance:
            raise RuntimeError(f"{stage} wrist crossed the protected center workspace")
        if abs(left[2] - right[2]) > float(config["maximum_wrist_height_delta_m"]):
            raise RuntimeError(f"{stage} wrist height mismatch exceeds the workspace limit")
        return left, right

    def require_grasp_contacts(label: str, monitor: ContactMonitor, hand: str) -> None:
        pairs = [" ".join(pair) for pair in monitor.pairs()]
        hand_pairs = [pair for pair in pairs if object_name in pair and f"{hand}_hand_" in pair]
        if not any("thumb" in pair for pair in hand_pairs):
            raise RuntimeError(f"{label} grasp has no thumb contact")
        if not any("index" in pair or "middle" in pair for pair in hand_pairs):
            raise RuntimeError(f"{label} grasp has no opposing finger contact")

    def wait_for_joint_targets(targets: dict[str, float], tolerance: float, timeout: float) -> float:
        deadline = time.monotonic() + timeout
        while True:
            positions = joints.get_many(list(targets))
            maximum_error = max(abs(positions[name] - target) for name, target in targets.items())
            if maximum_error <= tolerance:
                return maximum_error
            if time.monotonic() >= deadline:
                raise RuntimeError(f"finger feedback did not converge; maximum error={maximum_error:.4f} rad")
            time.sleep(0.05)

    def require_relative_grasp(
        reference_object: tuple[float, float, float],
        reference_left: tuple[float, float, float],
        reference_right: tuple[float, float, float],
        stage: str,
    ) -> float:
        current_object = poses.get(object_name)
        current_left, current_right = require_workspace(stage)
        left_drift = distance(
            subtract(current_object, current_left),
            subtract(reference_object, reference_left),
        )
        right_drift = distance(
            subtract(current_object, current_right),
            subtract(reference_object, reference_right),
        )
        drift = max(left_drift, right_drift)
        if drift > float(config["maximum_relative_drift_m"]):
            raise RuntimeError(f"{stage} payload-relative drift is {drift:.4f} m")
        return drift

    def require_on_support(
        position: tuple[float, float, float],
        center_field: str,
        initial_z: float,
        stage: str,
    ) -> None:
        center = tuple(float(value) for value in config[center_field])
        support_half = tuple(float(value) for value in config["support_half_extent_xy"])
        object_half = tuple(float(value) for value in config["object_half_extent_xy"])
        if abs(position[2] - initial_z) > float(config["support_height_tolerance_m"]):
            raise RuntimeError(f"{stage} payload height is outside support tolerance")
        margin = float(config["support_edge_margin_m"])
        for axis, name in ((0, "x"), (1, "y")):
            allowed = support_half[axis] - object_half[axis] - margin
            offset = abs(position[axis] - center[axis])
            if allowed <= 0 or offset > allowed:
                raise RuntimeError(f"{stage} payload is outside {name} support boundary")

    commander.publish(open_targets | lift_home | separation_home)
    base.stop()
    time.sleep(1.5)
    initial_object = poses.get(object_name)
    initial_model = poses.get(model_name)
    initial_yaw = yaw(poses.get_orientation(model_name))
    initial_left, initial_right = require_workspace("initial")
    require_on_support(initial_object, "source_support_center_xy", initial_object[2], "initial")
    print(f"stage=source payload={initial_object} wrists=({initial_left}, {initial_right})")

    commander.interpolate(open_targets, closed_targets, float(config["close_seconds"]))
    time.sleep(float(config["close_settle_seconds"]))
    closed_object = poses.get(object_name)
    if distance(closed_object, initial_object) > float(config["maximum_closure_displacement_m"]):
        raise RuntimeError("dual-hand closure displaced payload outside the grasp window")
    require_grasp_contacts("left", left_contacts, "left")
    require_grasp_contacts("right", right_contacts, "right")
    require_workspace("closed")
    print(
        f"stage=closed payload={closed_object} "
        f"left_contacts={len(left_contacts.pairs())} right_contacts={len(right_contacts.pairs())}"
    )

    commander.interpolate(lift_home, lifted, float(config["lift_seconds"]))
    time.sleep(0.8)
    lifted_object = poses.get(object_name)
    lifted_left, lifted_right = require_workspace("lifted")
    lift_height = lifted_object[2] - initial_object[2]
    if lift_height < float(config["minimum_lift_m"]):
        raise RuntimeError(f"bimanual lift was only {lift_height:.4f} m")
    max_relative_drift = 0.0
    hold_deadline = time.monotonic() + float(config["precarry_hold_seconds"])
    while time.monotonic() < hold_deadline:
        time.sleep(0.1)
        max_relative_drift = max(
            max_relative_drift,
            require_relative_grasp(lifted_object, lifted_left, lifted_right, "precarry hold"),
        )
    print(f"stage=lifted payload={lifted_object} lift={lift_height:.4f}m")

    carry_started = time.monotonic()
    while True:
        model_position = poses.get(model_name)
        travel = model_position[0] - initial_model[0]
        if travel >= float(config["base_travel_m"]):
            break
        if time.monotonic() - carry_started > float(config["base_timeout_seconds"]):
            raise RuntimeError(f"base carry timed out at {travel:.4f} m")
        remaining = float(config["base_travel_m"]) - travel
        velocity = (
            float(config["base_slow_velocity_m_s"])
            if remaining <= float(config["base_slow_distance_m"])
            else float(config["base_velocity_m_s"])
        )
        base.publish(velocity)
        max_relative_drift = max(
            max_relative_drift,
            require_relative_grasp(lifted_object, lifted_left, lifted_right, "mobile carry"),
        )
        if poses.get(object_name)[2] - initial_object[2] < float(config["minimum_lift_m"]) * 0.80:
            raise RuntimeError("payload dropped during mobile carry")
        time.sleep(0.05)
    base.stop()
    stopped_a = poses.get(model_name)
    time.sleep(0.6)
    stopped_b = poses.get(model_name)
    stop_drift = distance(stopped_a, stopped_b)
    base_travel = stopped_b[0] - initial_model[0]
    lateral = abs(stopped_b[1] - initial_model[1])
    yaw_error = abs(yaw(poses.get_orientation(model_name)) - initial_yaw)
    if stop_drift > float(config["maximum_stop_drift_m"]):
        raise RuntimeError(f"base stop drift is {stop_drift:.4f} m")
    if lateral > float(config["maximum_base_lateral_m"]) or yaw_error > float(config["maximum_base_yaw_rad"]):
        raise RuntimeError("base left the bimanual carry corridor")
    max_relative_drift = max(
        max_relative_drift,
        require_relative_grasp(lifted_object, lifted_left, lifted_right, "stopped carry"),
    )
    print(f"stage=carried base={base_travel:.4f}m stop_drift={stop_drift:.4f}m")

    commander.interpolate(lifted, lift_home, float(config["lower_seconds"]))
    time.sleep(0.8)
    lowered_object = poses.get(object_name)
    require_on_support(lowered_object, "destination_support_center_xy", initial_object[2], "lowered")
    commander.interpolate(closed_targets, open_targets, float(config["open_seconds"]))
    open_error = wait_for_joint_targets(
        open_targets,
        float(config["maximum_open_joint_error_rad"]),
        float(config["open_feedback_timeout_seconds"]),
    )
    time.sleep(0.5)
    released_object = poses.get(object_name)
    released_left, released_right = require_workspace("released")
    require_on_support(released_object, "destination_support_center_xy", initial_object[2], "released")

    commander.interpolate(separation_home, separated, float(config["separation_seconds"]))
    time.sleep(0.8)
    separated_object = poses.get(object_name)
    separated_left, separated_right = require_workspace("release probe")
    release_object_drift = distance(separated_object, released_object)
    left_probe = distance(separated_left, released_left)
    right_probe = distance(separated_right, released_right)
    if release_object_drift > float(config["maximum_release_object_drift_m"]):
        raise RuntimeError(f"released payload followed the hands by {release_object_drift:.4f} m")
    if min(left_probe, right_probe) < float(config["minimum_release_separation_m"]):
        raise RuntimeError("release probe did not separate both wrists from the payload")

    commander.interpolate(separated, separation_home, float(config["separation_seconds"]))
    commander.publish(open_targets)
    base.stop()
    print(
        "bimanual mobile manipulation verified: "
        f"lift={lift_height:.4f}m base={base_travel:.4f}m "
        f"max_relative_drift={max_relative_drift:.4f}m stop_drift={stop_drift:.4f}m "
        f"release_drift={release_object_drift:.4f}m "
        f"wrist_probes=({left_probe:.4f},{right_probe:.4f})m "
        f"open_error={open_error:.4f}rad"
    )
    base.close()
    commander.close()


def main() -> int:
    parser = argparse.ArgumentParser(description="Native bimanual mobile manipulation acceptance")
    parser.add_argument("--config", type=Path, default=default_config())
    args = parser.parse_args()
    try:
        run(load_config(args.config))
        return 0
    except (OSError, ValueError, RuntimeError) as error:
        print(f"bimanual mobile manipulation failed: {error}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
