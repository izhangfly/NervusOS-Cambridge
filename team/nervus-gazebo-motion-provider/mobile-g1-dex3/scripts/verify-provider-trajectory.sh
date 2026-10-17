#!/usr/bin/env bash
set -euo pipefail

variant_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
provider_root="$(cd -- "${variant_root}/../nervus-gazebo-motion-provider" && pwd)"
world="${variant_root}/simulation/worlds/nervus_mobile_g1_dex3.sdf"
models="${variant_root}/simulation/models"
log_file="$(mktemp /tmp/nervus-mobile-g1-trajectory.XXXXXX.log)"
partition="nervus_mobile_g1_trajectory_$$"
server_pid=""

cleanup() {
  if [[ -n "${server_pid}" ]] && kill -0 "${server_pid}" 2>/dev/null; then
    kill "${server_pid}" 2>/dev/null || true
    wait "${server_pid}" 2>/dev/null || true
  fi
  rm -f -- "${log_file}"
}
trap cleanup EXIT INT TERM

export GZ_IP=127.0.0.1
export GZ_PARTITION="${partition}"
export GZ_SIM_RESOURCE_PATH="${models}"
export SDF_PATH="${models}"

gz sim -s -r "${world}" >"${log_file}" 2>&1 &
server_pid="$!"

topics_ready=false
topics_ready_samples=0
for _ in $(seq 1 160); do
  if ! kill -0 "${server_pid}" 2>/dev/null; then
    tail -n 120 "${log_file}" >&2
    echo "Gazebo server exited before motion topics became ready." >&2
    exit 1
  fi
  topics=$(gz topic -l 2>/dev/null || true)
  if grep -q '^/model/nervus_mobile_g1_dex3/cmd_vel$' <<<"${topics}" &&
      grep -q '^/model/nervus_mobile_g1_dex3/joint/left_shoulder_pitch_joint/0/cmd_pos$' <<<"${topics}"; then
    topics_ready_samples=$((topics_ready_samples + 1))
    if [[ "${topics_ready_samples}" -ge 8 ]]; then
      topics_ready=true
      break
    fi
  else
    topics_ready_samples=0
  fi
  sleep 0.25
done
if [[ "${topics_ready}" != true ]]; then
  tail -n 120 "${log_file}" >&2
  echo "Timed out waiting for Gazebo motion topics." >&2
  exit 1
fi
timeout 20 gz topic -e \
  -t /world/nervus_mobile_g1_dex3/model/nervus_mobile_g1_dex3/joint_state -n 1 >/dev/null
timeout 20 gz topic -e -t /model/nervus_mobile_g1_dex3/odometry -n 1 >/dev/null

cd "${provider_root}"
if ! go test -tags=jettyintegration ./internal/jetty \
  -run '^TestRealJettyMobileG1TrajectoryCancelAndHome$' -count=1 -v; then
  tail -n 120 "${log_file}" >&2
  exit 1
fi

if ! kill -0 "${server_pid}" 2>/dev/null; then
  tail -n 120 "${log_file}" >&2
  echo "Gazebo server exited during Provider motion acceptance." >&2
  exit 1
fi
