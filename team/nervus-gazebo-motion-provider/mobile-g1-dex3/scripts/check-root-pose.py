#!/usr/bin/env python3
import math
import re
import sys
from pathlib import Path


MODEL_NAME = "nervus_mobile_g1_dex3"


def value(block: str, field: str, default: float) -> float:
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
    return ""


def model_pose(text: str) -> tuple[float, float, float, float, float, float, float]:
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
            position = submessage(candidate, "position")
            orientation = submessage(candidate, "orientation")
            return (
                value(position, "x", 0.0),
                value(position, "y", 0.0),
                value(position, "z", 0.0),
                value(orientation, "x", 0.0),
                value(orientation, "y", 0.0),
                value(orientation, "z", 0.0),
                value(orientation, "w", 1.0),
            )
        collecting = False
        block = []
    raise ValueError(f"pose for {MODEL_NAME} was not published")


def roll_pitch(pose: tuple[float, ...]) -> tuple[float, float]:
    _, _, _, qx, qy, qz, qw = pose
    roll = math.atan2(2.0 * (qw * qx + qy * qz), 1.0 - 2.0 * (qx * qx + qy * qy))
    pitch_term = max(-1.0, min(1.0, 2.0 * (qw * qy - qz * qx)))
    return roll, math.asin(pitch_term)


def main() -> int:
    if len(sys.argv) != 3:
        print("usage: check-root-pose.py FIRST SECOND", file=sys.stderr)
        return 2
    first = model_pose(Path(sys.argv[1]).read_text(encoding="utf-8"))
    second = model_pose(Path(sys.argv[2]).read_text(encoding="utf-8"))
    first_roll, first_pitch = roll_pitch(first)
    second_roll, second_pitch = roll_pitch(second)
    drift_xy = math.hypot(second[0] - first[0], second[1] - first[1])
    drift_z = abs(second[2] - first[2])

    if not 0.08 <= second[2] <= 0.20:
        raise ValueError(f"base height is outside the stable range: {second[2]:.6f} m")
    if max(abs(first_roll), abs(first_pitch), abs(second_roll), abs(second_pitch)) > 0.10:
        raise ValueError(
            f"base tilt exceeds 0.10 rad: roll={second_roll:.6f} pitch={second_pitch:.6f}"
        )
    if drift_xy > 0.02 or drift_z > 0.02:
        raise ValueError(f"base drift is too large: xy={drift_xy:.6f} z={drift_z:.6f}")

    print(
        "root pose stable: "
        f"xyz=({second[0]:.4f},{second[1]:.4f},{second[2]:.4f}) "
        f"roll={second_roll:.4f} pitch={second_pitch:.4f} "
        f"drift_xy={drift_xy:.4f} drift_z={drift_z:.4f}"
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
