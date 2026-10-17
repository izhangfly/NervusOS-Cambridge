#!/usr/bin/env bash
set -euo pipefail

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
variant_root=$(cd -- "$script_dir/.." && pwd)
model_root="$variant_root/simulation/models"
world="$variant_root/simulation/worlds/nervus_mobile_g1_dex3.sdf"

export GZ_IP=127.0.0.1
export GZ_PARTITION="${GZ_PARTITION:-nervus_mobile_g1_motion_$$}"
export GZ_SIM_RESOURCE_PATH="$model_root${GZ_SIM_RESOURCE_PATH:+:$GZ_SIM_RESOURCE_PATH}"
export SDF_PATH="$model_root${SDF_PATH:+:$SDF_PATH}"

work_dir=$(mktemp -d /tmp/nervus-mobile-g1-motion.XXXXXX)
log_file="$work_dir/server.log"
scene_file="$work_dir/scene.txt"
server_pid=""

cleanup() {
  publish_twist 0 0 >/dev/null 2>&1 || true
  if [[ -n "$server_pid" ]]; then
    kill "$server_pid" 2>/dev/null || true
    wait "$server_pid" 2>/dev/null || true
  fi
  if [[ "${KEEP_RESULTS:-0}" == 1 ]]; then
    printf 'Preserved motion-test results: %s\n' "$work_dir" >&2
  else
    rm -rf -- "$work_dir"
  fi
}
trap cleanup EXIT

publish_twist() {
  local linear_x=$1
  local angular_z=$2
  gz topic -t /model/nervus_mobile_g1_dex3/cmd_vel \
    -m gz.msgs.Twist \
    -p "linear: {x: ${linear_x}} angular: {z: ${angular_z}}"
}

capture_pose() {
  local name=$1
  timeout 10 gz topic -e -t /world/nervus_mobile_g1_dex3/pose/info -n 1 \
    >"$work_dir/${name}.pose"
}

capture_odom() {
  local name=$1
  timeout 10 gz topic -e -t /model/nervus_mobile_g1_dex3/odometry -n 1 \
    >"$work_dir/${name}.odom"
}

gz sdf -k "$variant_root/simulation/models/nervus_mobile_g1_dex3/model.sdf" \
  | grep -q '^Valid\.$'
gz sdf -k "$world" | grep -q '^Valid\.$'

gz sim -s -r "$world" >"$log_file" 2>&1 &
server_pid=$!

scene_ready=false
for _ in $(seq 1 80); do
  if ! kill -0 "$server_pid" 2>/dev/null; then
    cat "$log_file"
    echo "Gazebo server exited before becoming ready." >&2
    exit 1
  fi
  if gz service -s /world/nervus_mobile_g1_dex3/scene/info \
      --reqtype gz.msgs.Empty \
      --reptype gz.msgs.Scene \
      --timeout 1000 \
      --req '' >"$scene_file" 2>/dev/null \
      && grep -q 'name: "nervus_mobile_g1_dex3"' "$scene_file"; then
    scene_ready=true
    break
  fi
  sleep 0.25
done

if [[ "$scene_ready" != true ]]; then
  cat "$log_file"
  echo "Timed out waiting for the mobile G1 scene." >&2
  exit 1
fi

topics=$(gz topic -l)
for required_topic in \
  /model/nervus_mobile_g1_dex3/cmd_vel \
  /model/nervus_mobile_g1_dex3/wheel_odometry \
  /model/nervus_mobile_g1_dex3/odometry; do
  if ! grep -Fxq "$required_topic" <<<"$topics"; then
    printf 'Required topic is unavailable: %s\n' "$required_topic" >&2
    printf '%s\n' "$topics" >&2
    exit 1
  fi
done

sleep 1
capture_pose initial
capture_odom initial

publish_twist 0.20 0
sleep 4
publish_twist 0 0
sleep 1.2
capture_pose forward
capture_odom forward
sleep 1
capture_pose forward_stopped

publish_twist -0.15 0
sleep 3
publish_twist 0 0
sleep 1.2
capture_pose reverse

publish_twist 0 0.45
sleep 3
publish_twist 0 0
sleep 1.2
capture_pose turn
capture_odom turn
sleep 1
capture_pose final_stopped
capture_odom final_stopped

python3 "$script_dir/check-base-motion.py" "$work_dir"

if grep -Eq '\[Err\]|Segmentation fault|lib[^ ]* not found' "$log_file"; then
  cat "$log_file"
  echo "Gazebo reported a load or runtime error." >&2
  exit 1
fi

printf 'Base motion verified: partition=%s command=ready odometry_3d=ready\n' "$GZ_PARTITION"
