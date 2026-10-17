#!/usr/bin/env bash
set -euo pipefail

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
variant_root=$(cd -- "$script_dir/.." && pwd)
model_root="$variant_root/simulation/models"
model_sdf="$model_root/nervus_mobile_g1_dex3/model.sdf"
world="$variant_root/simulation/worlds/nervus_mobile_g1_dex3_grasp.sdf"
config="$variant_root/config/single-hand-grasp.json"

export GZ_IP=127.0.0.1
export GZ_PARTITION="${GZ_PARTITION:-nervus_mobile_g1_grasp_$$}"
export GZ_SIM_RESOURCE_PATH="$model_root${GZ_SIM_RESOURCE_PATH:+:$GZ_SIM_RESOURCE_PATH}"
export SDF_PATH="$model_root${SDF_PATH:+:$SDF_PATH}"

work_dir=$(mktemp -d /tmp/nervus-mobile-g1-grasp.XXXXXX)
log_file="$work_dir/server.log"
server_pid=""

cleanup() {
  if [[ -n "$server_pid" ]] && kill -0 "$server_pid" 2>/dev/null; then
    kill "$server_pid" 2>/dev/null || true
    wait "$server_pid" 2>/dev/null || true
  fi
  if [[ "${KEEP_RESULTS:-0}" == 1 ]]; then
    printf 'Preserved grasp-test results: %s\n' "$work_dir" >&2
  else
    rm -rf -- "$work_dir"
  fi
}
trap cleanup EXIT

gz sdf -k "$model_sdf" | grep -q '^Valid\.$'
gz sdf -k "$world" | grep -q '^Valid\.$'
python3 -m py_compile "$script_dir/single_hand_grasp.py"

gz sim -s -r "$world" >"$log_file" 2>&1 &
server_pid=$!

python3 "$script_dir/single_hand_grasp.py" --config "$config" | tee "$work_dir/result.txt"

if ! kill -0 "$server_pid" 2>/dev/null; then
  cat "$log_file"
  echo "Gazebo server exited during grasp acceptance." >&2
  exit 1
fi
if grep -Eq '\[Err\]|Segmentation fault|lib[^ ]* not found' "$log_file"; then
  cat "$log_file"
  echo "Gazebo reported a load or runtime error." >&2
  exit 1
fi
