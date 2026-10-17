#!/usr/bin/env python3
import math
import re
import sys
from pathlib import Path


MODEL_NAME = "nervus_mobile_g1_dex3"


def value(block: str, field: str, default: float = 0.0) -> float:
    match = re.search(rf"^\s*{re.escape(field)}:\s*([-+0-9.eE]+)\s*$", block, re.MULTILINE)
    return float(match.group(1)) if match else default


def submessage(block: str, name: str) -> str:
    lines = block.splitlines()
    collecting = False
    depth = 0
    result: list[str] = []
    for line in lines:
        if not collecting and re.match(rf"^\s*{re.escape(name)}\s*{{\s*$", line):
            collecting = True
        if collecting:
            result.append(line)
            depth += line.count("{") - line.count("}")
            if depth == 0:
                return "\n".join(result)
    raise ValueError(f"missing {name} submessage")


def pose_tuple(block: str) -> tuple[float, float, float, float, float, float, float]:
    position = submessage(block, "position")
    orientation = submessage(block, "orientation")
    return (
        value(position, "x"),
        value(position, "y"),
        value(position, "z"),
        value(orientation, "x"),
        value(orientation, "y"),
        value(orientation, "z"),
        value(orientation, "w", 1.0),
    )


def root_pose(text: str) -> tuple[float, ...]:
    lines = text.splitlines()
    collecting = False
    depth = 0
    block: list[str] = []
    for line in lines:
        if not collecting and re.match(r"^\s*pose\s*{\s*$", line):
            collecting = True
        if not collecting:
            continue
        block.append(line)
        depth += line.count("{") - line.count("}")
        if depth != 0:
            continue
        candidate = "\n".join(block)
        if re.search(rf'^\s*name:\s*"{re.escape(MODEL_NAME)}"\s*$', candidate, re.MULTILINE):
            return pose_tuple(candidate)
        collecting = False
        block = []
    raise ValueError(f"pose for {MODEL_NAME} was not published")


def odom_pose(text: str) -> tuple[float, ...]:
    return pose_tuple(submessage(text, "pose"))


def angles(pose: tuple[float, ...]) -> tuple[float, float, float]:
    _, _, _, qx, qy, qz, qw = pose
    roll = math.atan2(2 * (qw * qx + qy * qz), 1 - 2 * (qx * qx + qy * qy))
    pitch_term = max(-1.0, min(1.0, 2 * (qw * qy - qz * qx)))
    pitch = math.asin(pitch_term)
    yaw = math.atan2(2 * (qw * qz + qx * qy), 1 - 2 * (qy * qy + qz * qz))
    return roll, pitch, yaw


def angle_delta(end: float, start: float) -> float:
    return math.atan2(math.sin(end - start), math.cos(end - start))


def distance(a: tuple[float, ...], b: tuple[float, ...]) -> float:
    return math.hypot(a[0] - b[0], a[1] - b[1])


def load_pose(root: Path, name: str) -> tuple[float, ...]:
    return root_pose((root / f"{name}.pose").read_text(encoding="utf-8"))


def load_odom(root: Path, name: str) -> tuple[float, ...]:
    return odom_pose((root / f"{name}.odom").read_text(encoding="utf-8"))


def main() -> int:
    if len(sys.argv) != 2:
        print("usage: check-base-motion.py RESULT_DIRECTORY", file=sys.stderr)
        return 2

    root = Path(sys.argv[1])
    initial = load_pose(root, "initial")
    forward = load_pose(root, "forward")
    forward_stopped = load_pose(root, "forward_stopped")
    reverse = load_pose(root, "reverse")
    turn = load_pose(root, "turn")
    final_stopped = load_pose(root, "final_stopped")

    initial_yaw = angles(initial)[2]
    forward_dx = forward[0] - initial[0]
    forward_dy = abs(forward[1] - initial[1])
    reverse_dx = reverse[0] - forward_stopped[0]
    turn_yaw = angle_delta(angles(turn)[2], angles(reverse)[2])
    turn_translation = distance(turn, reverse)
    first_stop_drift = distance(forward_stopped, forward)
    final_stop_drift = distance(final_stopped, turn)
    final_stop_yaw_drift = abs(angle_delta(angles(final_stopped)[2], angles(turn)[2]))

    print(
        "measured motion: "
        f"forward={forward_dx:.4f}m lateral={forward_dy:.4f}m "
        f"reverse={reverse_dx:.4f}m turn={turn_yaw:.4f}rad "
        f"turn_translation={turn_translation:.4f}m"
    )

    if forward_dx < 0.30 or forward_dy > 0.10:
        raise ValueError(f"forward motion failed: dx={forward_dx:.4f} dy={forward_dy:.4f}")
    if reverse_dx > -0.18:
        raise ValueError(f"reverse motion failed: dx={reverse_dx:.4f}")
    if abs(turn_yaw) < 0.50 or turn_translation > 0.15:
        raise ValueError(
            f"turn failed: yaw={turn_yaw:.4f} rad translation={turn_translation:.4f} m"
        )
    if first_stop_drift > 0.035:
        raise ValueError(f"forward stop drift is too large: {first_stop_drift:.4f} m")
    if final_stop_drift > 0.035 or final_stop_yaw_drift > 0.05:
        raise ValueError(
            "final stop drift is too large: "
            f"xy={final_stop_drift:.4f} m yaw={final_stop_yaw_drift:.4f} rad"
        )

    for name, pose in {
        "initial": initial,
        "forward": forward,
        "reverse": reverse,
        "turn": turn,
        "final": final_stopped,
    }.items():
        roll, pitch, _ = angles(pose)
        if not 0.08 <= pose[2] <= 0.20 or max(abs(roll), abs(pitch)) > 0.12:
            raise ValueError(
                f"unstable root at {name}: z={pose[2]:.4f} roll={roll:.4f} pitch={pitch:.4f}"
            )

    odom_initial = load_odom(root, "initial")
    odom_forward = load_odom(root, "forward")
    odom_turn = load_odom(root, "turn")
    odom_final = load_odom(root, "final_stopped")
    root_dx = final_stopped[0] - initial[0]
    root_dy = final_stopped[1] - initial[1]
    odom_dx = odom_final[0] - odom_initial[0]
    odom_dy = odom_final[1] - odom_initial[1]
    root_yaw = angle_delta(angles(final_stopped)[2], initial_yaw)
    odom_yaw = angle_delta(angles(odom_final)[2], angles(odom_initial)[2])
    odom_error = math.hypot(root_dx - odom_dx, root_dy - odom_dy)
    odom_yaw_error = abs(angle_delta(odom_yaw, root_yaw))
    if odom_error > 0.04 or odom_yaw_error > 0.05:
        raise ValueError(
            f"3D odometry disagrees with root pose: xy={odom_error:.4f} yaw={odom_yaw_error:.4f}"
        )
    if distance(odom_forward, odom_initial) < 0.30:
        raise ValueError("3D odometry did not report forward displacement")
    if abs(angle_delta(angles(odom_turn)[2], angles(odom_forward)[2])) < 0.50:
        raise ValueError("3D odometry did not report the commanded turn")

    print(
        "base motion accepted: "
        f"forward={forward_dx:.3f}m reverse={reverse_dx:.3f}m "
        f"turn={turn_yaw:.3f}rad stop_drift={final_stop_drift:.3f}m "
        f"odom_error={odom_error:.3f}m"
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
