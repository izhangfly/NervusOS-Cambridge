#!/usr/bin/env python3
import hashlib
import json
import subprocess
import sys
import xml.etree.ElementTree as ET
from pathlib import Path
from typing import Any


EXPECTED_SOURCE_COMMIT = "daadf41ee9afce8f90fdc09a98506012691fa122"
EXPECTED_PROFILE_ID = "gazebo.unitree.g1.dex3.mobile.v1"
EXPECTED_MODEL = "nervus_mobile_g1_dex3"
EXPECTED_JOINTS = 31
EXPECTED_MESHES = 35


def require(condition: bool, message: str) -> None:
    if not condition:
        raise ValueError(message)


def load_json(path: Path) -> dict[str, Any]:
    value = json.loads(path.read_text(encoding="utf-8"))
    require(isinstance(value, dict), f"JSON root must be an object: {path}")
    return value


def sha256(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as stream:
        for chunk in iter(lambda: stream.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def git_head(repository: Path) -> str:
    result = subprocess.run(
        ["git", "-C", str(repository), "rev-parse", "HEAD"],
        check=True,
        capture_output=True,
        text=True,
    )
    return result.stdout.strip()


def main() -> int:
    variant_root = Path(__file__).resolve().parent.parent
    gazebo_root = variant_root.parents[1]
    source_repository = gazebo_root / "third_party" / "unitree_ros"
    model_root = variant_root / "simulation" / "models" / EXPECTED_MODEL
    model_sdf = model_root / "model.sdf"

    provenance = load_json(model_root / "SOURCE.json")
    require(provenance.get("upstream") == "https://github.com/unitreerobotics/unitree_ros.git", "unexpected Unitree upstream")
    require(provenance.get("commit") == EXPECTED_SOURCE_COMMIT, "generated provenance commit changed")
    require(provenance.get("license") == "BSD-3-Clause", "generated provenance license changed")
    require(provenance.get("upper_body_joint_count") == EXPECTED_JOINTS, "generated joint count changed")
    require(provenance.get("copied_mesh_count") == EXPECTED_MESHES, "generated mesh count changed")
    require(git_head(source_repository) == EXPECTED_SOURCE_COMMIT, "local Unitree checkout is not pinned to the accepted commit")

    copied_license = model_root / "source" / "LICENSE.unitree_ros"
    upstream_license = source_repository / "LICENSE"
    require(copied_license.is_file() and upstream_license.is_file(), "Unitree license is missing")
    require(sha256(copied_license) == sha256(upstream_license), "copied Unitree license differs from upstream")
    meshes = sorted((model_root / "meshes").glob("*.STL"))
    require(len(meshes) == EXPECTED_MESHES, f"expected {EXPECTED_MESHES} generated meshes, found {len(meshes)}")

    model = ET.parse(model_sdf).getroot().find("model")
    require(model is not None and model.get("name") == EXPECTED_MODEL, "generated SDF model identity changed")
    plugins = model.findall("plugin")
    joint_plugins = [plugin for plugin in plugins if plugin.get("name") == "gz::sim::systems::JointPositionController"]
    require(len(joint_plugins) == EXPECTED_JOINTS, f"expected {EXPECTED_JOINTS} joint controllers, found {len(joint_plugins)}")
    controlled_joints = {plugin.findtext("joint_name", default="") for plugin in joint_plugins}
    require(len(controlled_joints) == EXPECTED_JOINTS and "" not in controlled_joints, "joint controller names are incomplete or duplicated")
    require(sum(plugin.get("name") == "gz::sim::systems::DiffDrive" for plugin in plugins) == 1, "DiffDrive plugin is missing or duplicated")
    require(sum(plugin.get("name") == "gz::sim::systems::OdometryPublisher" for plugin in plugins) == 1, "3D odometry plugin is missing or duplicated")

    profile_bundle = load_json(variant_root / "profile" / "mobile-g1-dex3-v1.json")
    profiles = profile_bundle.get("profiles")
    require(isinstance(profiles, list) and len(profiles) == 1, "exactly one Mobile G1 profile is required")
    profile = profiles[0]
    require(profile.get("profile_id") == EXPECTED_PROFILE_ID, "profile identity changed")
    all_groups = {group.get("role"): group for group in profile.get("joint_groups", [])}
    all_upper_body = all_groups.get("all_upper_body", {}).get("joints", [])
    require(len(all_upper_body) == EXPECTED_JOINTS, "Profile all_upper_body group is incomplete")
    require({joint.get("name") for joint in all_upper_body} == controlled_joints, "Profile joints differ from generated controllers")
    capabilities = {capability.get("role") for capability in profile.get("capabilities", [])}
    require(capabilities == {"base.motion", "compatibility.joints"}, "unexpected public motion capability in Mobile G1 profile")
    require(not profile.get("sensors"), "sensor declarations are forbidden in the Motion Provider profile")

    runtime = load_json(variant_root / "profile" / "runtime-mobile-g1-dex3-v1.json")
    require(runtime.get("world") == EXPECTED_MODEL, "runtime world does not match the generated world")
    instances = runtime.get("instances")
    require(isinstance(instances, list) and len(instances) == 1, "runtime must bind exactly one robot instance")
    require(instances[0].get("profile_id") == EXPECTED_PROFILE_ID, "runtime profile binding changed")
    require(instances[0].get("model_name") == EXPECTED_MODEL, "runtime model binding changed")

    expected_worlds = {
        "nervus_mobile_g1_dex3.sdf": EXPECTED_MODEL,
        "nervus_mobile_g1_dex3_grasp.sdf": "nervus_mobile_g1_dex3_grasp",
        "nervus_mobile_g1_dex3_bimanual.sdf": "nervus_mobile_g1_dex3_bimanual",
    }
    for filename, name in expected_worlds.items():
        world = ET.parse(variant_root / "simulation" / "worlds" / filename).getroot().find("world")
        require(world is not None and world.get("name") == name, f"world identity changed: {filename}")

    markdown = sorted(path.relative_to(variant_root) for path in variant_root.rglob("*.md"))
    misplaced = [path for path in markdown if path != Path("README.md") and path.parts[0] != "docs"]
    require(not misplaced, f"Markdown files must be README.md or under docs/: {misplaced}")
    required_docs = {"TASKS.md", "OPERATIONS.md", "DEVELOPMENT.md", "PROVENANCE.md", "PACKAGING.md"}
    present_docs = {path.name for path in (variant_root / "docs").glob("*.md")}
    require(required_docs <= present_docs, f"release documentation is incomplete: {sorted(required_docs - present_docs)}")

    print(
        "release assets verified: "
        f"source={EXPECTED_SOURCE_COMMIT[:12]} model={EXPECTED_MODEL} "
        f"controllers={len(controlled_joints)} meshes={len(meshes)} worlds={len(expected_worlds)}"
    )
    return 0


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except (OSError, subprocess.CalledProcessError, ValueError, ET.ParseError, json.JSONDecodeError) as error:
        print(f"release asset verification failed: {error}", file=sys.stderr)
        raise SystemExit(1)
