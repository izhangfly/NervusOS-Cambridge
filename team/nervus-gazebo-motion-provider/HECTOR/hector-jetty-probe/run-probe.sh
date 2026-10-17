#!/usr/bin/env bash
set -euo pipefail

probe_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
gazebo_root="$(cd -- "$probe_dir/../../.." && pwd)"
build_dir="${NERVUS_HECTOR_BUILD_DIR:-/tmp/nervus-hector-jetty-build}"
server_log="${NERVUS_HECTOR_SERVER_LOG:-/tmp/nervus-hector-jetty-server.log}"

export GZ_IP="${GZ_IP:-127.0.0.1}"
export GZ_PARTITION="${GZ_PARTITION:-nervus_hector_jetty_probe}"
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
gz sim -s -v 3 "$gazebo_root/worlds/nervus_hector_dynamic.sdf" >"$server_log" 2>&1 &
server_pid=$!
sleep 3
if ! kill -0 "$server_pid" 2>/dev/null; then
  echo "Gazebo server exited during startup:" >&2
  tail -100 "$server_log" >&2
  exit 1
fi

set +e
"$build_dir/nervus_hector_jetty_probe" "$@"
probe_status=$?
set -e
if [[ $probe_status -ne 0 ]]; then
  echo "Gazebo server log tail:" >&2
  tail -100 "$server_log" >&2
fi
exit "$probe_status"
