#!/usr/bin/env bash
set -euo pipefail

if [[ $# -lt 1 ]]; then
  echo "usage: $0 POLICY_ONNX [probe arguments...]" >&2
  exit 64
fi

experiment_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
provider_root="$(cd -- "$experiment_dir/../.." && pwd)"
gazebo_root="$(cd -- "$provider_root/../../.." && pwd)"
build_dir="${NERVUS_G1_PROBE_BUILD_DIR:-/tmp/nervus-g1-dynamic-build}"
server_log="${NERVUS_G1_PROBE_SERVER_LOG:-/tmp/nervus-g1-dynamic-server.log}"

export GZ_IP="${GZ_IP:-127.0.0.1}"
export GZ_PARTITION="${GZ_PARTITION:-nervus_g1_dynamic_probe}"
export GZ_SIM_RESOURCE_PATH="$gazebo_root/models${GZ_SIM_RESOURCE_PATH:+:$GZ_SIM_RESOURCE_PATH}"

server_pid=""
cleanup() {
  if [[ -n "$server_pid" ]] && kill -0 "$server_pid" 2>/dev/null; then
    kill "$server_pid" 2>/dev/null || true
    wait "$server_pid" 2>/dev/null || true
  fi
}
trap cleanup EXIT INT TERM

: >"$server_log"
gz sim -s -v 3 "$gazebo_root/worlds/nervus_g1_dynamic.sdf" >"$server_log" 2>&1 &
server_pid=$!

sleep 3
if ! kill -0 "$server_pid" 2>/dev/null; then
  echo "Gazebo server exited during startup:" >&2
  tail -80 "$server_log" >&2
  exit 1
fi

set +e
"$build_dir/nervus_g1_dynamic_probe" "$@"
probe_status=$?
set -e

if [[ $probe_status -ne 0 ]]; then
  echo "Gazebo server log tail:" >&2
  tail -80 "$server_log" >&2
fi
exit "$probe_status"
