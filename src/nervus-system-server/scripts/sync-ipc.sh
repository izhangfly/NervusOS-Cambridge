#!/usr/bin/env bash
# Syncs the whole nervus-ipc module this repository depends on to a given version (default: latest master).
#
# Same logic as nervud/scripts/sync-ipc.sh. Both repositories depend on nervus-ipc,
# and they must point at [the same commit], or the kernel and services quietly diverge
# in how they read the same Envelope while both compile and run, differing only in how some field behaves.
#
# Why GOPRIVATE is needed: nervus-ipc is this organisation's own module, and a freshly pushed commit
# often isn't indexed on sum.golang.org yet, so a plain go get hits a 500. go.sum still records the hash,
# so any tampering after the first fetch is still caught; what's switched off is "a third party vouching for me",
# not verification itself.
#
# Usage:
#   scripts/sync-ipc.sh              sync to the latest master
#   scripts/sync-ipc.sh <commit|tag> sync to a given version
#
# nervus-ipc's go.mod is at the repository root; tags no longer use the old go/ subdirectory prefix.

set -euo pipefail

REF="${1:-master}"
MODULE="github.com/nervus-os/nervus-ipc"

cd "$(dirname "$0")/.."

echo "==> syncing ${MODULE}@${REF}"
GOPRIVATE="github.com/nervus-os/*" GOPROXY=direct GOFLAGS=-mod=mod \
	go get "${MODULE}@${REF}"

echo "==> go mod tidy"
GOPRIVATE="github.com/nervus-os/*" GOPROXY=direct go mod tidy

echo "==> current version"
grep nervus-ipc go.mod

echo "==> build and static checks"
go build ./...
go vet ./...

echo "==> tests"
go test ./...

echo
echo "⚠ remember to sync nervud to [the same commit]:"
echo "   cd ../nervud && scripts/sync-ipc.sh ${REF}"
echo "✅ sync complete"
