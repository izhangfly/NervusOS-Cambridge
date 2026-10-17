#!/usr/bin/env bash
set -euo pipefail

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
variant_root=$(cd -- "$script_dir/.." && pwd)
model_root="$variant_root/simulation/models"
world="$variant_root/simulation/worlds/nervus_mobile_g1_dex3.sdf"

export GZ_IP=127.0.0.1
export GZ_PARTITION="${GZ_PARTITION:-nervus_mobile_g1_verify_$$}"
export GZ_SIM_RESOURCE_PATH="$model_root${GZ_SIM_RESOURCE_PATH:+:$GZ_SIM_RESOURCE_PATH}"
export SDF_PATH="$model_root${SDF_PATH:+:$SDF_PATH}"

log_file=$(mktemp /tmp/nervus-mobile-g1-jetty.XXXXXX.log)
scene_file=$(mktemp /tmp/nervus-mobile-g1-scene.XXXXXX.txt)
pose_first=$(mktemp /tmp/nervus-mobile-g1-pose-first.XXXXXX.txt)
pose_second=$(mktemp /tmp/nervus-mobile-g1-pose-second.XXXXXX.txt)
server_pid=""

cleanup() {
  if [[ -n "$server_pid" ]]; then
    kill "$server_pid" 2>/dev/null || true
    wait "$server_pid" 2>/dev/null || true
  fi
  rm -f -- "$log_file" "$scene_file" "$pose_first" "$pose_second"
}
trap cleanup EXIT

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
controller_count=$(grep -Ec '^/model/nervus_mobile_g1_dex3/joint/.+/0/cmd_pos$' <<<"$topics" || true)
joint_state_count=$(grep -Ec '^/world/nervus_mobile_g1_dex3/model/nervus_mobile_g1_dex3/joint_state$' <<<"$topics" || true)
command_count=$(grep -Ec '^/model/nervus_mobile_g1_dex3/cmd_vel$' <<<"$topics" || true)
odometry_count=$(grep -Ec '^/model/nervus_mobile_g1_dex3/odometry$' <<<"$topics" || true)

if [[ "$controller_count" -ne 31 ]]; then
  printf 'Expected 31 upper-body controller topics, found %s.\n' "$controller_count" >&2
  printf '%s\n' "$topics" >&2
  exit 1
fi
if [[ "$joint_state_count" -ne 1 ]]; then
  echo "The model joint-state topic is unavailable." >&2
  printf '%s\n' "$topics" >&2
  exit 1
fi
if [[ "$command_count" -ne 1 || "$odometry_count" -ne 1 ]]; then
  echo "The base command or three-dimensional odometry topic is unavailable." >&2
  printf '%s\n' "$topics" >&2
  exit 1
fi
if grep -Eq '\[Err\]|Segmentation fault|lib[^ ]* not found' "$log_file"; then
  cat "$log_file"
  echo "Gazebo reported a load error." >&2
  exit 1
fi

pose_topic=/world/nervus_mobile_g1_dex3/pose/info
timeout 10 gz topic -e -t "$pose_topic" -n 1 >"$pose_first"
sleep 1
timeout 10 gz topic -e -t "$pose_topic" -n 1 >"$pose_second"
python3 "$script_dir/check-root-pose.py" "$pose_first" "$pose_second"

printf 'Jetty load verified: partition=%s controllers=%s joint_state=ready base_motion=ready\n' \
  "$GZ_PARTITION" "$controller_count"
