#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
ipc_proto_root="${NERVUS_IPC_PROTO_ROOT:-${repo_root}/../../../repo/nervus-ipc/proto}"

if ! command -v protoc >/dev/null 2>&1; then
  echo "generate-proto: protoc is not available" >&2
  exit 1
fi
if ! command -v protoc-gen-go >/dev/null 2>&1; then
  go_bin="$(go env GOBIN 2>/dev/null || true)"
  if [[ -z "${go_bin}" ]]; then
    go_path="$(go env GOPATH 2>/dev/null || true)"
    go_bin="${go_path:+${go_path}/bin}"
  fi
  if [[ -n "${go_bin}" && -x "${go_bin}/protoc-gen-go" ]]; then
    export PATH="${go_bin}:${PATH}"
  fi
fi
if ! command -v protoc-gen-go >/dev/null 2>&1; then
  echo "generate-proto: protoc-gen-go is not available" >&2
  exit 1
fi
if [[ ! -f "${ipc_proto_root}/nervus/ipc/v1/method_registry.proto" ]]; then
  echo "generate-proto: Nervus IPC proto root not found: ${ipc_proto_root}" >&2
  exit 1
fi

protoc \
  --proto_path="${repo_root}/proto" \
  --proto_path="${ipc_proto_root}" \
  --go_out="${repo_root}" \
  --go_opt=module=github.com/nervus-os/nervus-gazebo-provider \
  "${repo_root}/proto/gz/msgs/nervus_subset.proto" \
  "${repo_root}/proto/gazebo/simulation/motion/profile/v1/profile.proto" \
  "${repo_root}/proto/gazebo/simulation/provider/v1/manager.proto" \
  "${repo_root}/proto/gazebo/simulation/provider/v1/control.proto"
