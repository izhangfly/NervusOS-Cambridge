#!/usr/bin/env python3
import math
import re
import sys
from pathlib import Path

from joint_batch import command_topics, load_control_definition


def submessage(block: str, name: str) -> str:
    collecting = False
    depth = 0
    result: list[str] = []
    for line in block.splitlines():
        if not collecting and re.match(rf"^\s*{re.escape(name)}\s*{{\s*$", line):
            collecting = True
        if collecting:
            result.append(line)
            depth += line.count("{") - line.count("}")
            if depth == 0:
                return "\n".join(result)
    raise ValueError(f"missing {name} submessage")


def field(block: str, name: str, default: float = 0.0) -> float:
    match = re.search(rf"^\s*{re.escape(name)}:\s*([-+0-9.eE]+)\s*$", block, re.MULTILINE)
    return float(match.group(1)) if match else default


def joint_blocks(text: str) -> list[str]:
    result: list[str] = []
    collecting = False
    depth = 0
    block: list[str] = []
    for line in text.splitlines():
        if not collecting and re.match(r"^\s*joint\s*{\s*$", line):
            collecting = True
        if not collecting:
            continue
        block.append(line)
        depth += line.count("{") - line.count("}")
        if depth == 0:
            result.append("\n".join(block))
            collecting = False
            block = []
    return result


def joint_state(path: Path) -> dict[str, tuple[float, float]]:
    text = path.read_text(encoding="utf-8")
    # A newly discovered publisher may deliver a cached sample followed by the
    # requested live sample before the CLI exits. Evaluate only the latest
    # complete gz.msgs.Model frame.
    frames = [frame for frame in re.split(r"(?=^header\s*{)", text, flags=re.MULTILINE) if "joint {" in frame]
    if not frames:
        raise ValueError(f"no joint-state frame was published in {path}")
    states: dict[str, tuple[float, float]] = {}
    for block in joint_blocks(frames[-1]):
        match = re.search(r'^\s*name:\s*"([^"]+)"\s*$', block, re.MULTILINE)
        if not match:
            continue
        name = match.group(1)
        axis = submessage(block, "axis1")
        if name in states:
            raise ValueError(f"duplicate joint-state entry for {name}")
        states[name] = (field(axis, "position"), field(axis, "velocity"))
    if not states:
        raise ValueError(f"no joint states were published in {path}")
    return states


def check_targets(
    label: str,
    actual: dict[str, tuple[float, float]],
    targets: dict[str, float],
    config: dict,
) -> float:
    errors: list[tuple[float, str, float, float]] = []
    for joint, target in targets.items():
        if joint not in actual:
            raise ValueError(f"{label} feedback is missing {joint}")
        position = actual[joint][0]
        error = abs(position - float(target))
        group = config["joint_to_group"][joint]
        tolerance = float(config["tolerance_rad"][group])
        if error > tolerance:
            errors.append((error, joint, position, float(target)))
    if errors:
        error, joint, position, target = max(errors)
        raise ValueError(
            f"{label} did not converge: {joint} actual={position:.6f} "
            f"target={target:.6f} error={error:.6f}"
        )
    return max(abs(actual[joint][0] - float(target)) for joint, target in targets.items())


def main() -> int:
    if len(sys.argv) != 7:
        print(
            "usage: check-joint-control.py CONFIG URDF TOPICS INITIAL PROBE HOME",
            file=sys.stderr,
        )
        return 2

    config_path, urdf_path, topics_path, initial_path, probe_path, home_path = map(
        Path, sys.argv[1:]
    )
    config = load_control_definition(config_path, urdf_path)
    all_joints = config["groups"]["all_upper_body"]

    topics = set(topics_path.read_text(encoding="utf-8").splitlines())
    expected_topics = set(command_topics(config, all_joints))
    missing_topics = sorted(expected_topics - topics)
    if missing_topics:
        raise ValueError(f"missing controller topics: {missing_topics}")

    initial = joint_state(initial_path)
    probe = joint_state(probe_path)
    home = joint_state(home_path)
    expected_feedback = set(all_joints)
    for label, state in (("initial", initial), ("probe", probe), ("home", home)):
        missing = sorted(expected_feedback - set(state))
        if missing:
            raise ValueError(f"{label} joint-state message is missing: {missing}")

    initial_error = check_targets("initial home", initial, config["states"]["home"], config)
    probe_error = check_targets("probe", probe, config["states"]["probe"], config)
    home_error = check_targets("returned home", home, config["states"]["home"], config)

    unmoved = [
        joint
        for joint in all_joints
        if abs(probe[joint][0] - initial[joint][0]) < 0.05
    ]
    if unmoved:
        raise ValueError(f"controllers did not produce measurable motion: {unmoved}")

    moving = [(joint, state[1]) for joint, state in home.items() if joint in expected_feedback and abs(state[1]) > 0.05]
    if moving:
        joint, velocity = max(moving, key=lambda item: abs(item[1]))
        raise ValueError(f"joint did not settle after home: {joint} velocity={velocity:.6f} rad/s")

    if not all(math.isfinite(state[joint][0]) for state in (initial, probe, home) for joint in all_joints):
        raise ValueError("non-finite joint feedback detected")

    print(
        "joint control accepted: "
        f"controllers={len(expected_topics)} feedback={len(expected_feedback)} "
        f"initial_error={initial_error:.6f}rad probe_error={probe_error:.6f}rad "
        f"home_error={home_error:.6f}rad"
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
