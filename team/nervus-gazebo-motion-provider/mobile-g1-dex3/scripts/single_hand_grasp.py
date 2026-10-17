#!/usr/bin/env python3
import argparse
import atexit
import json
import math
import sys
import threading
import time
from pathlib import Path
from typing import Any


def default_config() -> Path:
    return Path(__file__).resolve().parent.parent / "config" / "single-hand-grasp.json"


def distance(left: tuple[float, float, float], right: tuple[float, float, float]) -> float:
    return math.sqrt(sum((a - b) ** 2 for a, b in zip(left, right, strict=True)))


def subtract(left: tuple[float, float, float], right: tuple[float, float, float]) -> tuple[float, float, float]:
    return tuple(a - b for a, b in zip(left, right, strict=True))


def add(left: tuple[float, float, float], right: tuple[float, float, float]) -> tuple[float, float, float]:
    return tuple(a + b for a, b in zip(left, right, strict=True))


def rotate(
    quaternion: tuple[float, float, float, float],
    vector: tuple[float, float, float],
) -> tuple[float, float, float]:
    x, y, z, w = quaternion
    vx, vy, vz = vector
    tx = 2.0 * (y * vz - z * vy)
    ty = 2.0 * (z * vx - x * vz)
    tz = 2.0 * (x * vy - y * vx)
    return (
        vx + w * tx + (y * tz - z * ty),
        vy + w * ty + (z * tx - x * tz),
        vz + w * tz + (x * ty - y * tx),
    )


def load_config(path: Path) -> dict[str, Any]:
    config = json.loads(path.read_text(encoding="utf-8"))
    if config.get("schema_version") != 1:
        raise ValueError("unsupported grasp configuration schema")
    open_targets = config.get("open")
    closed_targets = config.get("closed")
    if not isinstance(open_targets, dict) or set(open_targets) != set(closed_targets or {}):
        raise ValueError("open and closed states must define the same finger joints")
    if len(open_targets) != 7:
        raise ValueError("single-hand grasp requires all seven left Dex3 joints")
    for field in (
        "close_seconds",
        "close_settle_seconds",
        "lift_seconds",
        "hold_seconds",
        "lower_seconds",
        "open_seconds",
        "minimum_lift_m",
        "maximum_closure_displacement_m",
        "maximum_hold_drift_m",
        "maximum_wrist_relative_drift_m",
        "release_position_tolerance_m",
        "minimum_release_separation_m",
    ):
        value = float(config.get(field, 0))
        if not math.isfinite(value) or value <= 0:
            raise ValueError(f"{field} must be positive and finite")
    if float(config["hold_seconds"]) < 10.0:
        raise ValueError("grasp hold acceptance must last at least ten seconds")
    return config


class PoseMonitor:
    def __init__(self) -> None:
        self._condition = threading.Condition()
        self._poses: dict[str, tuple[float, float, float]] = {}
        self._orientations: dict[str, tuple[float, float, float, float]] = {}

    def callback(self, message: Any) -> None:
        poses = {
            pose.name: (float(pose.position.x), float(pose.position.y), float(pose.position.z))
            for pose in message.pose
            if pose.name
        }
        orientations = {
            pose.name: (
                float(pose.orientation.x),
                float(pose.orientation.y),
                float(pose.orientation.z),
                float(pose.orientation.w),
            )
            for pose in message.pose
            if pose.name
        }
        with self._condition:
            self._poses.update(poses)
            self._orientations.update(orientations)
            self._condition.notify_all()

    def wait_for(self, names: tuple[str, ...], timeout: float) -> None:
        deadline = time.monotonic() + timeout
        with self._condition:
            while any(name not in self._poses for name in names):
                remaining = deadline - time.monotonic()
                if remaining <= 0:
                    missing = [name for name in names if name not in self._poses]
                    raise RuntimeError(f"timed out waiting for poses: {missing}")
                self._condition.wait(remaining)

    def get(self, name: str) -> tuple[float, float, float]:
        with self._condition:
            if name not in self._poses:
                raise RuntimeError(f"pose is unavailable: {name}")
            return self._poses[name]

    def get_orientation(self, name: str) -> tuple[float, float, float, float]:
        with self._condition:
            if name not in self._orientations:
                raise RuntimeError(f"orientation is unavailable: {name}")
            return self._orientations[name]


class JointMonitor:
    def __init__(self) -> None:
        self._condition = threading.Condition()
        self._positions: dict[str, float] = {}

    def callback(self, message: Any) -> None:
        positions = {
            joint.name: float(joint.axis1.position)
            for joint in message.joint
            if joint.name and joint.HasField("axis1")
        }
        with self._condition:
            self._positions.update(positions)
            self._condition.notify_all()

    def wait_for(self, names: tuple[str, ...], timeout: float) -> None:
        deadline = time.monotonic() + timeout
        with self._condition:
            while any(name not in self._positions for name in names):
                remaining = deadline - time.monotonic()
                if remaining <= 0:
                    missing = [name for name in names if name not in self._positions]
                    raise RuntimeError(f"timed out waiting for joint feedback: {missing}")
                self._condition.wait(remaining)

    def get_many(self, names: list[str]) -> dict[str, float]:
        with self._condition:
            return {name: self._positions[name] for name in names}


class ContactMonitor:
    def __init__(self) -> None:
        self._lock = threading.Lock()
        self._pairs: set[tuple[str, str]] = set()

    def callback(self, message: Any) -> None:
        pairs = {(contact.collision1.name, contact.collision2.name) for contact in message.contact}
        with self._lock:
            self._pairs.update(pairs)

    def pairs(self) -> list[tuple[str, str]]:
        with self._lock:
            return sorted(self._pairs)


class NativeJointCommander:
    def __init__(self, model: str, topic_template: str, joints: list[str]) -> None:
        try:
            from gz.msgs.double_pb2 import Double
            from gz.transport import Node
        except ImportError as error:
            raise RuntimeError("Gazebo Transport Python bindings are unavailable") from error

        self._message_type = Double
        self.node = Node()
        self._publishers = {}
        self._topics = {}
        self._subscriptions: list[str] = []
        self._closed = False
        atexit.register(self.close)
        for joint in joints:
            topic = topic_template.format(model=model, joint=joint)
            publisher = self.node.advertise(topic, Double())
            if not publisher.valid():
                raise RuntimeError(f"Gazebo Transport rejected publisher for {joint}")
            self._publishers[joint] = publisher
            self._topics[joint] = topic

    def subscribe(self, message_type: Any, topic: str, callback: Any) -> bool:
        if self._closed:
            raise RuntimeError("Gazebo Transport session is closed")
        accepted = bool(self.node.subscribe(message_type, topic, callback))
        if accepted:
            self._subscriptions.append(topic)
        return accepted

    def close(self) -> None:
        if self._closed:
            return
        self._closed = True
        for topic in reversed(self._subscriptions):
            try:
                self.node.unsubscribe(topic)
            except RuntimeError:
                pass
        self._subscriptions.clear()
        self._publishers.clear()
        time.sleep(0.1)
        self.node = None

    def wait_for_connections(self, timeout: float) -> None:
        deadline = time.monotonic() + timeout
        while True:
            missing = [joint for joint, publisher in self._publishers.items() if not publisher.has_connections()]
            if not missing:
                return
            if time.monotonic() >= deadline:
                topics = [self._topics[joint] for joint in missing]
                raise RuntimeError(f"joint command topics have no subscribers: {topics}")
            time.sleep(0.05)

    def publish(self, targets: dict[str, float]) -> None:
        for joint, target in targets.items():
            message = self._message_type()
            message.data = float(target)
            if not self._publishers[joint].publish(message):
                raise RuntimeError(f"Gazebo Transport rejected command for {joint}")

    def interpolate(self, start: dict[str, float], target: dict[str, float], duration: float) -> None:
        if set(start) != set(target):
            raise ValueError("trajectory endpoints must define identical joints")
        steps = max(1, math.ceil(duration / 0.02))
        started = time.monotonic()
        for step in range(1, steps + 1):
            ratio = step / steps
            self.publish({joint: value + (target[joint] - value) * ratio for joint, value in start.items()})
            wake_at = started + step * duration / steps
            time.sleep(max(0.0, wake_at - time.monotonic()))


def run(config: dict[str, Any]) -> None:
    try:
        from gz.msgs.contacts_pb2 import Contacts
        from gz.msgs.model_pb2 import Model
        from gz.msgs.pose_v_pb2 import Pose_V
    except ImportError as error:
        raise RuntimeError("Gazebo Pose_V bindings are unavailable") from error

    object_name = str(config["object"])
    wrist_name = str(config["wrist_link"])
    open_targets = {joint: float(value) for joint, value in config["open"].items()}
    closed_targets = {joint: float(value) for joint, value in config["closed"].items()}
    lift_joint = str(config["lift_joint"])
    lift_home = float(config["lift_home_rad"])
    lift_target = float(config["lift_target_rad"])
    release_probe = float(config["release_probe_rad"])
    joints = list(open_targets) + [lift_joint]
    support_center = tuple(float(value) for value in config["support_center_xy"])
    support_half_extent = tuple(float(value) for value in config["support_half_extent_xy"])
    object_half_extent = tuple(float(value) for value in config["object_half_extent_xy"])
    if any(len(config[field]) != 2 for field in ("support_center_xy", "support_half_extent_xy", "object_half_extent_xy")):
        raise ValueError("support and object XY extents must contain two values")

    def require_on_support(position: tuple[float, float, float], initial_z: float, stage: str) -> None:
        if abs(position[2] - initial_z) > float(config["release_position_tolerance_m"]):
            raise RuntimeError(f"{stage} object height is outside support tolerance")
        margin = float(config["support_edge_margin_m"])
        for axis, label in ((0, "x"), (1, "y")):
            allowed = support_half_extent[axis] - object_half_extent[axis] - margin
            offset = abs(position[axis] - support_center[axis])
            if allowed <= 0 or offset > allowed:
                raise RuntimeError(
                    f"{stage} object is outside support in {label}: offset={offset:.4f} m"
                )

    commander = NativeJointCommander(str(config["model"]), str(config["topic_template"]), joints)
    monitor = PoseMonitor()
    joint_monitor = JointMonitor()
    contact_monitor = ContactMonitor()
    if not commander.subscribe(Pose_V, str(config["pose_topic"]), monitor.callback):
        raise RuntimeError("Gazebo Transport rejected pose subscription")
    if not commander.subscribe(Model, str(config["joint_state_topic"]), joint_monitor.callback):
        raise RuntimeError("Gazebo Transport rejected joint-state subscription")
    if not commander.subscribe(Contacts, str(config["contact_topic"]), contact_monitor.callback):
        raise RuntimeError("Gazebo Transport rejected contact subscription")
    commander.wait_for_connections(12.0)
    monitor.wait_for((object_name, wrist_name), 12.0)
    joint_monitor.wait_for(tuple(joints), 12.0)

    def world_link(name: str) -> tuple[float, float, float]:
        model_position = monitor.get(str(config["model"]))
        model_orientation = monitor.get_orientation(str(config["model"]))
        return add(model_position, rotate(model_orientation, monitor.get(name)))

    def world_collision_center(
        name: str, offset: tuple[float, float, float]
    ) -> tuple[float, float, float]:
        local_center = add(monitor.get(name), rotate(monitor.get_orientation(name), offset))
        model_position = monitor.get(str(config["model"]))
        model_orientation = monitor.get_orientation(str(config["model"]))
        return add(model_position, rotate(model_orientation, local_center))

    current_fingers = dict(open_targets)
    current_lift = {lift_joint: lift_home}
    commander.publish(current_fingers | current_lift)
    time.sleep(1.5)
    initial_object = monitor.get(object_name)
    initial_wrist = world_link(wrist_name)
    robot_model = monitor.get(str(config["model"]))
    robot_base = monitor.get("base_link")
    print(
        f"stage=supported object={initial_object} wrist={initial_wrist} "
        f"robot_model={robot_model} base_link={robot_base}"
    )

    commander.interpolate(current_fingers, closed_targets, float(config["close_seconds"]))
    current_fingers = dict(closed_targets)
    time.sleep(float(config["close_settle_seconds"]))
    grasped_object = monitor.get(object_name)
    fingertip_poses = {
        name: monitor.get(name)
        for name in (
            "left_hand_thumb_2_link",
            "left_hand_middle_1_link",
            "left_hand_index_1_link",
        )
    }
    collision_centers = {
        "thumb": world_collision_center("left_hand_thumb_2_link", (0.0, -0.026, 0.0)),
        "middle": world_collision_center("left_hand_middle_1_link", (0.026, 0.0, 0.0)),
        "index": world_collision_center("left_hand_index_1_link", (0.026, 0.0, 0.0)),
    }
    actual_fingers = joint_monitor.get_many(list(closed_targets))
    contact_pairs = contact_monitor.pairs()
    print(
        f"stage=closed object={grasped_object} fingertips={fingertip_poses} "
        f"collision_centers={collision_centers} joints={actual_fingers} "
        f"contacts={contact_pairs}"
    )
    finger_contacts = [
        " ".join(pair)
        for pair in contact_pairs
        if object_name in " ".join(pair) and "left_hand_" in " ".join(pair)
    ]
    if not any("thumb" in pair for pair in finger_contacts):
        raise RuntimeError("closed grasp has no thumb contact")
    if not any("index" in pair or "middle" in pair for pair in finger_contacts):
        raise RuntimeError("closed grasp has no opposing finger contact")
    if distance(initial_object, grasped_object) > float(config["maximum_closure_displacement_m"]):
        raise RuntimeError(
            f"finger closure displaced object before lift: {distance(initial_object, grasped_object):.4f} m"
        )

    lifted_joint = {lift_joint: lift_target}
    commander.interpolate(current_lift, lifted_joint, float(config["lift_seconds"]))
    current_lift = lifted_joint
    time.sleep(0.8)
    lifted_object = monitor.get(object_name)
    lifted_wrist = world_link(wrist_name)
    lift_height = lifted_object[2] - initial_object[2]
    wrist_lift = lifted_wrist[2] - initial_wrist[2]
    print(f"stage=lifted object={lifted_object} wrist={lifted_wrist}")
    if lift_height < float(config["minimum_lift_m"]):
        raise RuntimeError(
            f"object lift was {lift_height:.4f} m while wrist lift was {wrist_lift:.4f} m"
        )

    lifted_relative = subtract(lifted_object, lifted_wrist)
    max_hold_drift = 0.0
    max_relative_drift = 0.0
    minimum_hold_height = lift_height
    hold_deadline = time.monotonic() + float(config["hold_seconds"])
    samples = 0
    while time.monotonic() < hold_deadline:
        time.sleep(0.1)
        held_object = monitor.get(object_name)
        held_wrist = world_link(wrist_name)
        hold_height = held_object[2] - initial_object[2]
        hold_drift = distance(held_object, lifted_object)
        relative_drift = distance(subtract(held_object, held_wrist), lifted_relative)
        minimum_hold_height = min(minimum_hold_height, hold_height)
        max_hold_drift = max(max_hold_drift, hold_drift)
        max_relative_drift = max(max_relative_drift, relative_drift)
        samples += 1
        if hold_height < float(config["minimum_lift_m"]) * 0.80:
            raise RuntimeError(f"object dropped during hold: height={hold_height:.4f} m")
        if hold_drift > float(config["maximum_hold_drift_m"]):
            raise RuntimeError(f"object drifted {hold_drift:.4f} m during hold")
        if relative_drift > float(config["maximum_wrist_relative_drift_m"]):
            raise RuntimeError(f"object moved {relative_drift:.4f} m relative to wrist")

    home_joint = {lift_joint: lift_home}
    commander.interpolate(current_lift, home_joint, float(config["lower_seconds"]))
    current_lift = home_joint
    time.sleep(0.8)
    lowered_object = monitor.get(object_name)
    require_on_support(lowered_object, initial_object[2], "lowered")

    commander.interpolate(current_fingers, open_targets, float(config["open_seconds"]))
    current_fingers = dict(open_targets)
    time.sleep(1.0)
    released_object = monitor.get(object_name)
    released_wrist = world_link(wrist_name)
    require_on_support(released_object, initial_object[2], "released")

    probe_joint = {lift_joint: release_probe}
    commander.interpolate(current_lift, probe_joint, 1.2)
    current_lift = probe_joint
    time.sleep(0.8)
    separated_object = monitor.get(object_name)
    separated_wrist = world_link(wrist_name)
    object_release_drift = distance(separated_object, released_object)
    wrist_release_motion = distance(separated_wrist, released_wrist)
    if object_release_drift > float(config["release_position_tolerance_m"]):
        raise RuntimeError(f"object followed the hand after release: {object_release_drift:.4f} m")
    if wrist_release_motion < float(config["minimum_release_separation_m"]):
        raise RuntimeError(f"release separation probe moved wrist only {wrist_release_motion:.4f} m")

    commander.interpolate(current_lift, home_joint, 1.2)
    commander.publish(open_targets)
    print(
        "single-hand grasp verified: "
        f"lift={lift_height:.4f}m hold={float(config['hold_seconds']):.1f}s "
        f"samples={samples} min_hold={minimum_hold_height:.4f}m "
        f"max_drift={max_hold_drift:.4f}m relative_drift={max_relative_drift:.4f}m "
        f"release_drift={object_release_drift:.4f}m wrist_probe={wrist_release_motion:.4f}m"
    )
    commander.close()


def main() -> int:
    parser = argparse.ArgumentParser(description="Native Gazebo Transport single-hand grasp acceptance")
    parser.add_argument("--config", type=Path, default=default_config())
    args = parser.parse_args()
    try:
        run(load_config(args.config))
        return 0
    except (OSError, ValueError, RuntimeError) as error:
        print(f"single-hand grasp failed: {error}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
