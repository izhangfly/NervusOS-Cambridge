#!/usr/bin/env bash
set -euo pipefail

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
variant_root=$(cd -- "$script_dir/.." && pwd)
provider_root=$(cd -- "$variant_root/../nervus-gazebo-motion-provider" && pwd)
model_root="$variant_root/simulation/models"
run_real=true

if [[ $# -gt 1 ]]; then
  echo "usage: $0 [--static-only]" >&2
  exit 2
fi
if [[ $# -eq 1 ]]; then
  if [[ "$1" != "--static-only" ]]; then
    echo "usage: $0 [--static-only]" >&2
    exit 2
  fi
  run_real=false
fi

for command in bash cmake ctest git go gz python3; do
  if ! command -v "$command" >/dev/null 2>&1; then
    printf 'Required command is unavailable: %s\n' "$command" >&2
    exit 1
  fi
done

work_dir=$(mktemp -d /tmp/nervus-mobile-g1-final.XXXXXX)
cleanup() {
  rm -rf -- "$work_dir"
}
trap cleanup EXIT

export GZ_IP=127.0.0.1
export GZ_SIM_RESOURCE_PATH="$model_root${GZ_SIM_RESOURCE_PATH:+:$GZ_SIM_RESOURCE_PATH}"
export SDF_PATH="$model_root${SDF_PATH:+:$SDF_PATH}"
export PYTHONPYCACHEPREFIX="$work_dir/pycache"
export PATH="/root/go/bin:$PATH"

python3 "$script_dir/check-release-assets.py"
python3 -m py_compile "$script_dir"/*.py
bash -n "$script_dir"/*.sh
gz sdf -k "$model_root/nervus_mobile_g1_dex3/model.sdf" | grep -q '^Valid\.$'
for world in "$variant_root"/simulation/worlds/*.sdf; do
  gz sdf -k "$world" | grep -q '^Valid\.$'
done

cmake -S "$provider_root/native" -B "$work_dir/native" \
  -DCMAKE_BUILD_TYPE=RelWithDebInfo -DBUILD_TESTING=ON
cmake --build "$work_dir/native" --parallel 2
ctest --test-dir "$work_dir/native" --output-on-failure

cmake -S "$provider_root/native" -B "$work_dir/native-sanitized" \
  -DCMAKE_BUILD_TYPE=Debug -DBUILD_TESTING=ON -DNERVUS_ENABLE_SANITIZERS=ON
cmake --build "$work_dir/native-sanitized" --parallel 2
ASAN_OPTIONS=detect_leaks=1:halt_on_error=1 \
UBSAN_OPTIONS=halt_on_error=1:print_stacktrace=1 \
  ctest --test-dir "$work_dir/native-sanitized" --output-on-failure

(
  cd "$provider_root"
  bash scripts/verify-contract.sh
  go test ./...
  go vet ./...
  go build -trimpath -o "$work_dir/gazebod" ./cmd/gazebod
)

if [[ "$run_real" == true ]]; then
  # Let Gazebo Transport retire discovery records created by native / Go unit
  # tests before starting the first cross-process Jetty partition.
  sleep 5
  real_tests=(
    verify-provider-trajectory.sh
    verify-jetty.sh
    verify-joint-control.sh
    verify-base-motion.sh
    verify-single-hand-grasp.sh
    verify-bimanual-mobile.sh
  )
  for test in "${real_tests[@]}"; do
    "$script_dir/$test"
    sleep 2
  done
fi

printf 'Final verification passed: static=%s real_jetty=%s\n' true "$run_real"
