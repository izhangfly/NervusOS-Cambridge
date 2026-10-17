#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${repo_root}"

tracked_outputs=(
  internal/gzmsgs/nervus_subset.pb.go
  gen/go/gazebo/simulation/motion/profile/v1/profile.pb.go
  gen/go/gazebo/simulation/provider/v1/manager.pb.go
  gen/go/gazebo/simulation/provider/v1/control.pb.go
  provider/provider.binpb
  provider/schemas.binpb
)

before="$(sha256sum "${tracked_outputs[@]}")"
bash scripts/generate-proto.sh
bash scripts/generate-provider.sh
after="$(sha256sum "${tracked_outputs[@]}")"

if [[ "${before}" != "${after}" ]]; then
  echo "verify-contract: generated outputs changed during regeneration" >&2
  exit 1
fi

go mod verify
go test -race ./...
CGO_ENABLED=0 go test ./...

echo "verify-contract: deterministic regeneration and Go checks passed"
