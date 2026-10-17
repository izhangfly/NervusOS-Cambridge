#!/usr/bin/env bash
# Build everything NervusOS needs on this machine.
#
#   scripts/build.sh            release + store apps + store catalog
#   scripts/build.sh release    just the system release tarball
#   scripts/build.sh apps       just the third-party store apps and catalog
#
# Outputs go to out/:
#   out/nervus-os-<abi>.tar.gz      kernel, system services, system apps, JRE, trust
#   out/store/catalog.ini           what the App Store offers
#   out/store/packages/*.nspkg      the signed app packages
#
# Signing keys live in keys/ (never committed). Missing keys are generated on
# first build: fine for a development device, not for a product.
set -euo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SRC="$REPO/src"
OUT="$REPO/out"
KEYS="$REPO/keys"
ABI="${NERVUS_ABI:-linux-x86_64}"
WHAT="${1:-all}"

# Toolchain: prefer the user-space installs from docs/SETUP.md, then PATH
for d in "$HOME/.local/opt/go/bin" "$HOME/go/bin"; do [[ -d "$d" ]] && PATH="$d:$PATH"; done
if [[ -z "${JAVA_HOME:-}" ]]; then
	for d in "$HOME/.local/opt/jdk-21" /usr/lib/jvm/java-21-openjdk-amd64 /usr/lib/jvm/java-21-openjdk-arm64; do
		[[ -x "$d/bin/java" ]] && { export JAVA_HOME="$d"; break; }
	done
fi
[[ -n "${JAVA_HOME:-}" ]] && PATH="$JAVA_HOME/bin:$PATH"
export PATH

step() { printf '\n\033[1;36m==> %s\033[0m\n' "$*"; }
die() { printf '\033[1;31mbuild: %s\033[0m\n' "$*" >&2; exit 1; }

command -v go >/dev/null || die "Go not found (needs 1.26+, see docs/SETUP.md)"
command -v java >/dev/null || die "JDK 21 not found (see docs/SETUP.md)"
mkdir -p "$OUT" "$KEYS"
chmod 700 "$KEYS"

genkey() { [[ -f "$1" ]] || { openssl genpkey -algorithm ed25519 -out "$1" 2>/dev/null; chmod 600 "$1"; echo "    generated $(basename "$1")"; }; }

build_release() {
	step "System release ($ABI)"
	NERVUD_DIR="$SRC/nervud" SERVER_DIR="$SRC/nervus-system-server" \
	SOFTWARE_DIR="$SRC/nervus-system-software" APP_SDK_DIR="$SRC/nervus-app-sdk" \
	PACKAGING_DIR="$SRC/nervus-packaging" IPC_DIR="$SRC/nervus-ipc" \
	TARGET_ABI="$ABI" JRE_SOURCE="${JRE_SOURCE:-auto}" JRE_VERSION=21 KEY_DIR="$KEYS" \
	NERVUS_BOARD="${NERVUS_BOARD:-dell-latitude}" \
	REQUIRE_OEM_COUNTERSIGN=false TARBALL="$OUT/nervus-os-$ABI.tar.gz" \
		bash "$SRC/nervus-deploy/build-release.sh" --non-interactive
}

# build_app <dir> <signing key path inside the project>
build_app() {
	local dir="$1" key="$2"
	step "App: $(basename "$dir")"
	genkey "$KEYS/developer.pem"
	mkdir -p "$(dirname "$dir/$key")"
	cp "$KEYS/developer.pem" "$dir/$key"
	(cd "$dir" && sed -i 's/\r$//' gradlew && chmod +x gradlew &&
		./gradlew nspkg --console=plain -q -Pnervus.abi="$ABI")
	local pkg
	for pkg in "$dir"/build/outputs/nspkg/*.nspkg; do
		cp "$pkg" "$OUT/store/packages/"
		echo "    $(basename "$pkg") ($(du -h "$pkg" | cut -f1))"
	done
}

build_apps() {
	rm -rf "$OUT/store"
	mkdir -p "$OUT/store/packages"
	build_app "$SRC/robot-arm" "signing/developer.pem"
	build_app "$SRC/hand-teleop" "signing/developer.pem"
	build_app "$SRC/nervus-app-example" "signing/private.pem"
	cp "$REPO/store/catalog.ini" "$OUT/store/catalog.ini"
	step "Store catalog"
	local f
	for f in $(sed -n 's/^file *= *//p' "$OUT/store/catalog.ini"); do
		[[ -f "$OUT/store/packages/$f" ]] && echo "    ok       $f" || die "catalog lists $f but it was not built"
	done
}

case "$WHAT" in
	all) build_release; build_apps ;;
	release) build_release ;;
	apps) build_apps ;;
	*) die "unknown target '$WHAT' (all | release | apps)" ;;
esac
step "Done. Install with: sudo hosted/install-hosted.sh"
