#!/usr/bin/env bash
set -euo pipefail

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
variant_root=$(cd -- "$script_dir/.." && pwd)
model_root="$variant_root/simulation/models"
model_sdf="$model_root/nervus_mobile_g1_dex3/model.sdf"
model_urdf="$model_root/nervus_mobile_g1_dex3/source/nervus_mobile_g1_dex3.urdf"
world="$variant_root/simulation/worlds/nervus_mobile_g1_dex3.sdf"
config="$variant_root/config/native-joint-control.json"
joint_state_topic=/world/nervus_mobile_g1_dex3/model/nervus_mobile_g1_dex3/joint_state

export GZ_IP=127.0.0.1
export GZ_PARTITION="${GZ_PARTITION:-nervus_mobile_g1_joints_$$}"
export GZ_SIM_RESOURCE_PATH="$model_root${GZ_SIM_RESOURCE_PATH:+:$GZ_SIM_RESOURCE_PATH}"
export SDF_PATH="$model_root${SDF_PATH:+:$SDF_PATH}"

work_dir=$(mktemp -d /tmp/nervus-mobile-g1-joints.XXXXXX)
log_file="$work_dir/server.log"
scene_file="$work_dir/scene.txt"
topics_file="$work_dir/topics.txt"
server_pid=""

cleanup() {
  if [[ -n "$server_pid" ]] && kill -0 "$server_pid" 2>/dev/null; then
    python3 "$script_dir/joint_batch.py" --state home >/dev/null 2>&1 || true
    kill "$server_pid" 2>/dev/null || true
    wait "$server_pid" 2>/dev/null || true
  fi
  if [[ "${KEEP_RESULTS:-0}" == 1 ]]; then
    printf 'Preserved joint-test results: %s\n' "$work_dir" >&2
  else
    rm -rf -- "$work_dir"
  fi
}
trap cleanup EXIT

capture_joint_state() {
  local name=$1
  timeout 10 gz topic -e -t "$joint_state_topic" -n 1 >"$work_dir/${name}.joint_state"
}

capture_pose() {
  local name=$1
  timeout 10 gz topic -e -t /world/nervus_mobile_g1_dex3/pose/info -n 1 \
    >"$work_dir/${name}.pose"
}

gz sdf -k "$model_sdf" | grep -q '^Valid\.$'
gz sdf -k "$world" | grep -q '^Valid\.$'
python3 "$script_dir/joint_batch.py" --state home --validate-only
python3 "$script_dir/joint_batch.py" --state probe --validate-only

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

gz topic -l >"$topics_file"
sleep 2.5
capture_pose initial
capture_joint_state initial

python3 "$script_dir/joint_batch.py" --state probe
sleep 5
capture_joint_state probe

python3 "$script_dir/joint_batch.py" --state home
sleep 5
capture_joint_state home
capture_pose final

python3 "$script_dir/check-joint-control.py" \
  "$config" "$model_urdf" "$topics_file" \
  "$work_dir/initial.joint_state" "$work_dir/probe.joint_state" "$work_dir/home.joint_state"
python3 "$script_dir/check-root-pose.py" "$work_dir/initial.pose" "$work_dir/final.pose"

if grep -Eq '\[Err\]|Segmentation fault|lib[^ ]* not found' "$log_file"; then
  cat "$log_file"
  echo "Gazebo reported a load or runtime error." >&2
  exit 1
fi

printf 'Native joint control verified: partition=%s joints=31 groups=5 home=ready\n' \
  "$GZ_PARTITION"
