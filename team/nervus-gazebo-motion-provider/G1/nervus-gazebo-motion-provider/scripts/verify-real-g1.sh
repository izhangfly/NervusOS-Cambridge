#!/usr/bin/env bash

set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
gazebo_root="$(cd "${repo_root}/../../.." && pwd)"
world_file="${gazebo_root}/worlds/nervus_g1.sdf"
partition="${GZ_PARTITION:-nervus-g1-integration-$$}"
log_file="${TMPDIR:-/tmp}/nervus-g1-jetty-$$.log"

export GZ_IP="${GZ_IP:-127.0.0.1}"
export GZ_PARTITION="${partition}"
export GZ_SIM_RESOURCE_PATH="${gazebo_root}/models${GZ_SIM_RESOURCE_PATH:+:${GZ_SIM_RESOURCE_PATH}}"

server_pid=""
keep_log=true
cleanup() {
  if [[ -n "${server_pid}" ]] && kill -0 "${server_pid}" 2>/dev/null; then
    kill "${server_pid}" 2>/dev/null || true
    wait "${server_pid}" 2>/dev/null || true
  fi
  if [[ "${keep_log}" != true ]]; then
    rm -f "${log_file}"
  fi
}
trap cleanup EXIT INT TERM

stdbuf -oL -eL gz sim -s -r -v 3 "${world_file}" >"${log_file}" 2>&1 &
server_pid=$!

ready=false
for _ in $(seq 1 150); do
  services="$(gz service -l 2>/dev/null || true)"
  topics="$(gz topic -l 2>/dev/null || true)"
  controller_count="$(printf '%s\n' "${topics}" |
    grep -Ec '^/model/nervus_g1/joint/[^/]+/0/cmd_pos$' || true)"

  if printf '%s\n' "${services}" | grep -Fxq "/world/nervus_g1/control" &&
    [[ "${controller_count}" -eq 29 ]] &&
    printf '%s\n' "${topics}" |
      grep -Fxq "/world/nervus_g1/model/nervus_g1/joint_state"; then
    ready=true
    break
  fi
  if ! kill -0 "${server_pid}" 2>/dev/null; then
    break
  fi
  sleep 0.1
done

if [[ "${ready}" != true ]]; then
  echo "G1 Jetty server did not become ready. Log: ${log_file}" >&2
  tail -n 200 "${log_file}" >&2 || true
  exit 1
fi

cd "${repo_root}"
go test -tags jettyintegration ./internal/jetty \
  -run TestRealJettyG1MinimumClosedLoop -count=1 -v

keep_log=false
echo "G1 real Jetty integration passed on partition ${partition}."
