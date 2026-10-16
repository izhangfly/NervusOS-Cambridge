#!/usr/bin/env bash
# Builds a complete Nervus OS release.
#
#   ./build-release.sh                     interactive
#   ./build-release.sh --non-interactive   environment variables and defaults only
#
# Produces a tar.gz holding everything install-nervus.sh consumes directly:
# the kernel binaries, system services, system apps, trust material and the installer itself.
#
# [The build machine and target can be different architectures]. Go cross-compiles, and the JVM output is
# architecture-independent (apart from skiko's native library, chosen by --abi).
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# ---------------------------------------------------------------- output helpers
c_reset=$'\033[0m'; c_bold=$'\033[1m'; c_dim=$'\033[2m'
c_red=$'\033[31m'; c_green=$'\033[32m'; c_yellow=$'\033[33m'; c_blue=$'\033[36m'

step() { printf '\n%s==> %s%s\n' "$c_bold$c_blue" "$*" "$c_reset"; }
info() { printf '    %s\n' "$*"; }
ok()   { printf '    %s✓%s %s\n' "$c_green" "$c_reset" "$*"; }
warn() { printf '    %s!%s %s\n' "$c_yellow" "$c_reset" "$*" >&2; }
die()  { printf '\n%s✗ %s%s\n' "$c_red$c_bold" "$*" "$c_reset" >&2; exit 1; }

INTERACTIVE=1
[[ "${1:-}" == "--non-interactive" ]] && INTERACTIVE=0

# ask <variable> <prompt> <default>
# In non-interactive mode, use the default (or an existing environment variable of the same name)
ask() {
	local var="$1" prompt="$2" default="${3:-}" cur
	cur="${!var:-}"
	if [[ -n "$cur" ]]; then info "$prompt: $cur ${c_dim}(from environment)${c_reset}"; return; fi
	if (( ! INTERACTIVE )); then printf -v "$var" '%s' "$default"; info "$prompt: $default"; return; fi
	local reply
	read -r -p "    $prompt [$default]: " reply
	printf -v "$var" '%s' "${reply:-$default}"
}

# confirm <prompt> <default y|n>
confirm() {
	local prompt="$1" default="${2:-y}" reply
	if (( ! INTERACTIVE )); then [[ "$default" == y ]]; return; fi
	read -r -p "    $prompt [$( [[ $default == y ]] && echo 'Y/n' || echo 'y/N' )]: " reply
	reply="${reply:-$default}"
	[[ "$reply" =~ ^[Yy] ]]
}

CLEANUP_PATHS=()
cleanup() {
	local p
	for p in "${CLEANUP_PATHS[@]:-}"; do
		[[ -n "$p" ]] && rm -rf "$p"
	done
}
trap cleanup EXIT

make_tmpdir() {
	local var="$1" d
	d="$(mktemp -d)"
	CLEANUP_PATHS+=("$d")
	printf -v "$var" '%s' "$d"
}

ask_optional_path() {
	local var="$1" prompt="$2" hint="${3:-}" cur reply suffix
	cur="${!var:-}"
	if [[ -n "$cur" ]]; then info "$prompt: $cur ${c_dim}(from environment)${c_reset}"; return; fi
	if (( ! INTERACTIVE )); then printf -v "$var" '%s' ""; info "$prompt: empty"; return; fi
	[[ -n "$hint" ]] && suffix=", GitHub: $hint" || suffix=""
	read -r -p "    $prompt [empty${suffix}]: " reply
	printf -v "$var" '%s' "$reply"
}

# ================================================================= preflight checks
step "Checking the build toolchain"

need() { command -v "$1" >/dev/null 2>&1 || die "missing $1. $2"; }
need go       "install Go ≥1.24 (go.dev/dl), or apt install golang-go"
need openssl  "apt install openssl"
need tar      ""
need sha256sum ""

GO_VER="$(go env GOVERSION 2>/dev/null || echo unknown)"
ok "go $GO_VER"
ok "openssl $(openssl version | awk '{print $2}')"

# The JVM side needs JDK 21. sdkman installs aren't on the default PATH, so look for one
if ! command -v java >/dev/null 2>&1; then
	for cand in "$HOME/.sdkman/candidates/java/current" /usr/lib/jvm/java-21-openjdk*; do
		[[ -x "$cand/bin/java" ]] && { export JAVA_HOME="$cand"; export PATH="$JAVA_HOME/bin:$PATH"; break; }
	done
fi
command -v java >/dev/null 2>&1 || die "missing java (the system apps need JDK 21). apt install openjdk-21-jdk"
JAVA_MAJOR="$(java -version 2>&1 | sed -n '1s/.*version "\([0-9]*\).*/\1/p')"
[[ "${JAVA_MAJOR:-0}" -ge 21 ]] || die "JDK version is $JAVA_MAJOR; need ≥21"
ok "java $JAVA_MAJOR ($(command -v java))"

# ================================================================= source locations
step "Source locations"
info "When a repository's local directory is left empty, it is cloned from https://github.com/nervus-os."
info "The nervus-ipc local directory only controls the Go replace; left empty, Go uses the go.mod version and Gradle gets the sources from GitHub."
echo

GITHUB_ORG_URL="${GITHUB_ORG_URL:-https://github.com/nervus-os}"
SOURCE_TMP=""

ask_optional_path NERVUD_DIR    "kernel nervud local directory"                 "$GITHUB_ORG_URL/nervud"
ask_optional_path SERVER_DIR    "system services local directory"               "$GITHUB_ORG_URL/nervus-system-server"
ask_optional_path SOFTWARE_DIR  "system apps local directory"                   "$GITHUB_ORG_URL/nervus-system-software"
ask_optional_path APP_SDK_DIR   "app SDK nervus-app-sdk local directory"        "$GITHUB_ORG_URL/nervus-app-sdk"
ask_optional_path PACKAGING_DIR "packaging nervus-packaging local directory"    "$GITHUB_ORG_URL/nervus-packaging"
ask_optional_path IPC_DIR       "nervus-ipc local directory"                    "$GITHUB_ORG_URL/nervus-ipc"

stage_local_repo() {
	local var="$1" repo="$2"
	local source_dir="${!var}" dest_dir
	[[ -d "$source_dir" ]] || die "directory does not exist: $source_dir"
	source_dir="$(cd "$source_dir" && pwd)"
	[[ -n "$SOURCE_TMP" ]] || make_tmpdir SOURCE_TMP
	dest_dir="$SOURCE_TMP/$repo"
	info "$repo: copying from local $source_dir"
	cp -a -- "$source_dir" "$dest_dir"
	printf -v "$var" '%s' "$dest_dir"
	ok "$repo copied to a temporary directory"
}

prepare_repo() {
	local var="$1" repo="$2" dir
	dir="${!var:-}"
	if [[ -n "$dir" ]]; then
		stage_local_repo "$var" "$repo"
		return 0
	fi

	command -v git >/dev/null 2>&1 || die "missing git. It's needed to fetch sources from GitHub when a directory is left empty"
	[[ -n "$SOURCE_TMP" ]] || make_tmpdir SOURCE_TMP
	dir="$SOURCE_TMP/$repo"
	info "$repo: fetching $GITHUB_ORG_URL/$repo.git"
	git clone --depth 1 "$GITHUB_ORG_URL/$repo.git" "$dir" >/dev/null
	printf -v "$var" '%s' "$dir"
	ok "$repo ready"
}

prepare_repo NERVUD_DIR   "nervud"
prepare_repo SERVER_DIR   "nervus-system-server"
prepare_repo SOFTWARE_DIR "nervus-system-software"
prepare_repo APP_SDK_DIR  "nervus-app-sdk"
prepare_repo PACKAGING_DIR "nervus-packaging"

[[ -f "$NERVUD_DIR/go.mod" ]]   || die "$NERVUD_DIR doesn't look like the nervud repository (no go.mod)"
[[ -d "$SERVER_DIR/scripts" ]]  || die "$SERVER_DIR doesn't look like the system-server repository (no scripts/)"
[[ -f "$SOFTWARE_DIR/settings.gradle.kts" ]] || die "$SOFTWARE_DIR doesn't look like the system-software repository"
[[ -f "$APP_SDK_DIR/settings.gradle.kts" ]] || die "$APP_SDK_DIR doesn't look like the nervus-app-sdk repository"
[[ -f "$PACKAGING_DIR/settings.gradle.kts" ]] || die "$PACKAGING_DIR doesn't look like the nervus-packaging repository"

IPC_SOURCE_DIR=""
if [[ -n "$IPC_DIR" ]]; then
	stage_local_repo IPC_DIR "nervus-ipc"
	# nervus-ipc keeps go.mod at the repo root (older layouts used a go/ subdir)
	[[ -f "$IPC_DIR/go.mod" || -d "$IPC_DIR/go" ]] || die "$IPC_DIR has no go.mod (or go/ subdirectory)"
	IPC_SOURCE_DIR="$IPC_DIR"
	warn "nervud build will temporarily replace nervus-ipc with the local copy: $IPC_DIR"
else
	prepare_repo IPC_SOURCE_DIR "nervus-ipc"
fi
[[ -d "$IPC_SOURCE_DIR/jvm/protocol/src/main" ]] || die "$IPC_SOURCE_DIR has no jvm/protocol/src/main"

# ================================================================= target configuration
step "Target configuration"

ask TARGET_ABI "target ABI (linux-arm64 | linux-x86_64)" "linux-arm64"
case "$TARGET_ABI" in
	linux-arm64)  GOARCH=arm64 ;;
	linux-x86_64) GOARCH=amd64 ;;
	*) die "unsupported ABI: $TARGET_ABI (only linux-arm64 / linux-x86_64)" ;;
esac
ok "GOARCH=$GOARCH"

# Platform JRE: every runtime=jvm component on the target depends on it.
#
# [It must include the java.desktop module]: a headless JRE can't run Compose, and the symptom is
# NoClassDefFoundError: java/awt/*, which doesn't say the JRE was trimmed.
cat <<'EOF'
    Platform JRE: every runtime=jvm component on the target (desktop / settings / file manager) depends on it.

      auto     download the Eclipse Temurin JRE for the target architecture and bundle it (recommended)
      <path>   use a JDK/JRE directory you prepared yourself
      empty    don't bundle one; the installer apt-installs it on the target

    "empty" fails on Debian 12 / Raspberry Pi OS: their main repositories only have openjdk-17,
    and the components need ≥21. Hence the default is auto.
EOF
echo
ask JRE_SOURCE "platform JRE (auto | directory path | empty)" "auto"
ask JRE_VERSION "Temurin major version (auto only)" "21"

if [[ "$JRE_SOURCE" == auto ]]; then
	case "$TARGET_ABI" in
		linux-arm64)  ADOPT_ARCH=aarch64 ;;
		linux-x86_64) ADOPT_ARCH=x64 ;;
		*) die "auto download doesn't support ABI $TARGET_ABI; give a path instead" ;;
	esac
	command -v curl >/dev/null 2>&1 || die "auto download needs curl (apt install curl)"

	CACHE="$SCRIPT_DIR/.jre-cache"
	mkdir -p "$CACHE"
	TARBALL_JRE="$CACHE/temurin${JRE_VERSION}-${ADOPT_ARCH}.tar.gz"

	if [[ -s "$TARBALL_JRE" ]]; then
		info "reusing the downloaded $(basename "$TARBALL_JRE") ($(du -h "$TARBALL_JRE" | cut -f1))"
	else
		info "downloading the Temurin $JRE_VERSION JRE ($ADOPT_ARCH)…"
		# Adoptium's latest endpoint 302s to the actual file; -L follows it.
		# Download to .part, then rename: an interrupted download doesn't leave a cache that looks complete but is truncated
		curl -fL --progress-bar -o "$TARBALL_JRE.part" \
			"https://api.adoptium.net/v3/binary/latest/${JRE_VERSION}/ga/linux/${ADOPT_ARCH}/jre/hotspot/normal/eclipse" \
			|| die "download failed. You can download it from adoptium.net by hand and put the directory path in JRE_SOURCE"
		mv "$TARBALL_JRE.part" "$TARBALL_JRE"
		ok "downloaded ($(du -h "$TARBALL_JRE" | cut -f1))"
	fi

	EXTRACT="$CACHE/temurin${JRE_VERSION}-${ADOPT_ARCH}"
	if [[ ! -x "$EXTRACT/bin/java" && ! -f "$EXTRACT/bin/java" ]]; then
		rm -rf "$EXTRACT"; mkdir -p "$EXTRACT"
		# Temurin's archive wraps everything in a jdk-21.x.y+z-jre/ directory; --strip-components=1 removes it
		# so $EXTRACT is JAVA_HOME directly
		tar -C "$EXTRACT" --strip-components=1 -xzf "$TARBALL_JRE"
	fi
	JRE_SOURCE="$EXTRACT"
	ok "JRE ready: $JRE_SOURCE"
fi

if [[ -n "$JRE_SOURCE" ]]; then
	JRE_SOURCE="$(readlink -f "$JRE_SOURCE")"
	[[ -f "$JRE_SOURCE/bin/java" ]] || die "$JRE_SOURCE/bin/java doesn't exist (this directory doesn't look like a JDK/JRE root)"

	# Check that java.desktop is there. Without it Compose can't run a single line, and the symptom
	# (NoClassDefFoundError: java/awt/*) doesn't point at a trimmed JRE.
	#
	# [Running java --list-modules won't do]: when cross-packaging, the JRE is for the target architecture
	# (an arm64 package built on x86_64) and can't run here. The `release` file in the JDK root
	# has a line MODULES="java.base java.desktop ...", which can be read regardless of architecture.
	if [[ -f "$JRE_SOURCE/release" ]] && grep -q '^MODULES=' "$JRE_SOURCE/release"; then
		if grep '^MODULES=' "$JRE_SOURCE/release" | grep -q 'java\.desktop'; then
			ok "JRE includes java.desktop (from the release file)"
		else
			die "this JRE's MODULES has no java.desktop, so Compose can't run.
     Use a full JRE/JDK (not -headless), or add --add-modules java.desktop when running jlink"
		fi
		# Also check the target architecture, so we don't bundle a JRE of the wrong one
		JRE_ARCH="$(sed -n 's/^OS_ARCH="\(.*\)"/\1/p' "$JRE_SOURCE/release")"
		case "$TARGET_ABI:$JRE_ARCH" in
			linux-arm64:aarch64|linux-x86_64:x86_64|*:) ;;
			*) die "the JRE architecture is $JRE_ARCH but the target ABI is $TARGET_ABI; it would fail with Exec format error on the target" ;;
		esac
		[[ -n "$JRE_ARCH" ]] && ok "JRE architecture $JRE_ARCH matches $TARGET_ABI"
	elif [[ -x "$JRE_SOURCE/bin/java" ]] && "$JRE_SOURCE/bin/java" -version >/dev/null 2>&1; then
		# No release file, but it runs here (same architecture): fall back to running it
		"$JRE_SOURCE/bin/java" --list-modules 2>/dev/null | grep -q '^java\.desktop' \
			|| die "this JRE lacks the java.desktop module, so Compose can't run"
		ok "JRE includes java.desktop (--list-modules)"
	else
		die "$JRE_SOURCE has no release file and its java can't run here, so java.desktop can't be checked.
     Official packages from adoptium.net always include the release file; for your own jlink, add --add-modules java.desktop"
	fi
fi

# ================================================================= signing material
step "Signing material"
cat <<'EOF'
    Nervus uses three Ed25519 private keys with different jobs:

      platform-root       signs the trust bundle; its [public key is compiled into nervud]
      platform-release    signs system services (can get the most powerful permissions such as perm.safety.rearm)
      platform-systemapp  signs system apps (Platform trust, but not the permissions above)

    A build without a platform root is a development build: every system package drops to ordinary trust
    and trust levels stop working. A production image must never look like that.

    To generate the three keys by hand (in the default key directory):

      openssl genpkey -algorithm ed25519 -out keys/platform-root.pem
      openssl genpkey -algorithm ed25519 -out keys/platform-release.pem
      openssl genpkey -algorithm ed25519 -out keys/platform-systemapp.pem
EOF
echo

ask KEY_DIR "key directory" "$SCRIPT_DIR/keys"
mkdir -p "$KEY_DIR"; chmod 700 "$KEY_DIR"

K_ROOT="$KEY_DIR/platform-root.pem"
K_RELEASE="$KEY_DIR/platform-release.pem"
K_SYSAPP="$KEY_DIR/platform-systemapp.pem"

missing=()
for k in "$K_ROOT" "$K_RELEASE" "$K_SYSAPP"; do [[ -f "$k" ]] || missing+=("$(basename "$k")"); done

if (( ${#missing[@]} )); then
	warn "missing keys: ${missing[*]}"
	if confirm "Generate a new set now (fine for development/demos; production keys should be kept offline)?" y; then
		for k in "$K_ROOT" "$K_RELEASE" "$K_SYSAPP"; do
			[[ -f "$k" ]] && continue
			openssl genpkey -algorithm ed25519 -out "$k" 2>/dev/null
			chmod 600 "$k"
			ok "generated $(basename "$k")"
		done
	else
		die "put these three .pem files in $KEY_DIR and run again"
	fi
fi

# raw_pub <pem>: base64 of the raw 32-byte Ed25519 public key.
#
# openssl exports a DER-encoded SubjectPublicKeyInfo; the first 12 bytes are the algorithm header and
# the raw key is the last 32 bytes. The kernel's decodePubKey wants exactly those 32 bytes in base64.
raw_pub() { openssl pkey -in "$1" -pubout -outform DER 2>/dev/null | tail -c 32 | base64 -w0; }
# key_id matches the kernel's keyIDOf: sha256: + the hex digest of the raw public key
key_id()  { openssl pkey -in "$1" -pubout -outform DER 2>/dev/null | tail -c 32 | sha256sum | awk '{print "sha256:"$1}'; }

ROOT_PUB_B64="$(raw_pub "$K_ROOT")"
[[ ${#ROOT_PUB_B64} -eq 44 ]] || die "unexpected platform root public key length ($ROOT_PUB_B64); check that the key is ed25519"
ok "platform root key_id $(key_id "$K_ROOT")"

ask REQUIRE_OEM_COUNTERSIGN "require an OEM countersignature on third-party packages (true/false)" "false"

# ================================================================= build
make_tmpdir OUT_ROOT
STAGE="$OUT_ROOT/nervus-release"
mkdir -p "$STAGE"/{bin,system-packages,trust,systemd,scripts}

use_local_ipc_replace() {
	local label="$1"
	local mod_bak="$OUT_ROOT/$label.go.mod.bak"
	local sum_bak="$OUT_ROOT/$label.go.sum.bak"
	[[ -n "$IPC_DIR" ]] || return 0
	if [[ ! -f go.mod ]]; then
		warn "$label has no go.mod; skipping the local ipc replace"
		return
	fi
	cp go.mod "$mod_bak"
	[[ -f go.sum ]] && cp go.sum "$sum_bak" || rm -f "$sum_bak"
	trap "cp '$mod_bak' go.mod; if [[ -f '$sum_bak' ]]; then cp '$sum_bak' go.sum; else rm -f go.sum; fi" EXIT
	# [The module path has no /go suffix]. nervus-ipc's go.mod is at the repository root; the old
	# go/ subdirectory layout is gone. Writing .../nervus-ipc/go would point the replace at a
	# [module path that doesn't exist]. Go doesn't complain, the replace just doesn't apply, so the local ipc directory is
	# silently ignored and each repository builds against its own go.mod version. That really happened: nervud and
	# pkgmanagerd ended up on different ipc commits, and it was only noticed on the machine.
	go mod edit -replace="$ipcModule=$IPC_DIR"
	ok "$label uses the local nervus-ipc ($IPC_DIR)"
}

# ipcModule is nervus-ipc's Go module path, defined once for the replace and the consistency check.
ipcModule="github.com/nervus-os/nervus-ipc"

# assert_same_ipc stops "two repositories built against different ipc commits".
#
# # Why this has to be a hard failure at build time
#
# An interface's schema_hash covers the whole FileDescriptorSet, [including every recursive import].
# Touching nervus/ipc/v1/method_registry.proto changes every interface's hash. So
# two repositories one commit apart is enough for a Provider's RegisterEndpoint to be refused:
#
#     err="interface schema hash does not match the catalog"
#
# and that failure shows up [only on the target, only at run time]. Both sides build, run and pass their tests.
# The symptom also gets passed along: the Provider can't register -> the caller's Resolve takes the on-demand branch ->
# it gets a RESOURCE_NOT_FOUND unrelated to the real cause, pointing at the selector.
#
# README §4.4 always said "the three repositories must point at the same commit", but nothing enforced it.
assert_same_ipc() {
	[[ -z "$IPC_DIR" ]] || { info "using a local nervus-ipc; skipping the version consistency check"; return 0; }
	local nervud_ver server_ver
	nervud_ver="$(cd "$NERVUD_DIR" && go list -m -f '{{.Version}}' "$ipcModule" 2>/dev/null)"
	server_ver="$(cd "$SERVER_DIR" && go list -m -f '{{.Version}}' "$ipcModule" 2>/dev/null)"
	if [[ -z "$nervud_ver" || -z "$server_ver" ]]; then
		warn "can't read the nervus-ipc versions; skipping the consistency check (nervud='$nervud_ver' server='$server_ver')"
		return 0
	fi
	if [[ "$nervud_ver" != "$server_ver" ]]; then
		die "nervud and system-server point at [different nervus-ipc versions]:
       nervud        $nervud_ver
       system-server $server_ver

     They must match, or the interface schema_hash won't, and Providers on the target
     get refused at registration (\"interface schema hash does not match the catalog\"),
     while each side compiles and passes its tests.

     Fix: run scripts/sync-ipc.sh in the repository that's behind, then commit go.mod/go.sum."
	fi
	ok "nervus-ipc versions match: $nervud_ver"
}

step "⓪ Checking the nervus-ipc version"
assert_same_ipc

step "① Building the nervud kernel ($TARGET_ABI)"
(
	cd "$NERVUD_DIR"
	use_local_ipc_replace nervud
	export CGO_ENABLED=0 GOOS=linux GOARCH="$GOARCH" GOPRIVATE=github.com/nervus-os GOTOOLCHAIN=auto
	# -X injects the platform root public key. This is the one difference between a "production build" and a "development build"
	LDF="-s -w -X github.com/nervus-os/nervud/internal/pkgregistry.embeddedPlatformRootB64=$ROOT_PUB_B64"
	go build -trimpath -ldflags "$LDF" -o "$STAGE/bin/nervud" .
	go build -trimpath -ldflags "-s -w" -o "$STAGE/bin/nervusctl" ./cmd/nervusctl
)
ok "nervud     $(du -h "$STAGE/bin/nervud" | cut -f1)"
ok "nervusctl  $(du -h "$STAGE/bin/nervusctl" | cut -f1)"

step "② Building system services"
(
	cd "$SERVER_DIR"
	use_local_ipc_replace system-server
	export PATH GOPRIVATE=github.com/nervus-os GOTOOLCHAIN=auto
	bash ./scripts/build-image-tree.sh "$OUT_ROOT/server-out" "$K_RELEASE" "$TARGET_ABI"
)
if compgen -G "$OUT_ROOT/server-out/*" >/dev/null; then
	cp -r "$OUT_ROOT"/server-out/* "$STAGE/system-packages/"
	for p in "$OUT_ROOT"/server-out/*/; do ok "$(basename "$p") (platform-release)"; done
else
	warn "system services produced no packages; check the output of build-image-tree.sh"
fi

step "③ Building system apps (Compose Desktop)"
(
	cd "$SOFTWARE_DIR"
	# gradlew must have LF line endings; a Windows checkout brings CRLF,
	# and the error is "cannot execute: required file not found", which looks like a missing file
	if head -c 200 gradlew | grep -q $'\r'; then
		warn "gradlew has CRLF line endings; fixing in place"
		sed -i 's/\r$//' gradlew
	fi
	chmod +x gradlew
	mkdir -p signing
	cp "$K_SYSAPP" signing/platform-systemapp.pem
	# permissionui and store sign platform-release: they need perm.permission.admin,
	# which requires the platform-release signing role that systemapp can't satisfy.
	# Each module picks its signing key [itself] in its own build.gradle.kts (signing.role +
	# signing.keyFile); it isn't set here. So this only makes sure both keys
	# are present, and the module decides which to use
	cp "$K_RELEASE" signing/platform-release.pem
	./gradlew nspkgImageTree --console=plain -Pnervus.abi="$TARGET_ABI" -q
)
for tree in "$SOFTWARE_DIR"/*/build/outputs/image-tree/*/; do
	[[ -d "$tree" ]] || continue
	cp -r "$tree" "$STAGE/system-packages/"
	# The role depends on the module and can't be seen from the output directory name; reporting one per package would mislead, so only name and size
	ok "$(basename "$tree") ($(du -sh "$tree" | cut -f1))"
done

step "④ Generating trust material"
# The trust bundle records which key_id may hold which role. It is itself signed by the platform root,
# whose public key is already compiled into the nervud binary above
cat > "$STAGE/trust/trust-bundle.json" <<EOF
{"format":1,"platform_keys":[{"key_id":"$(key_id "$K_RELEASE")","key":"$(raw_pub "$K_RELEASE")","roles":["platform-release"]},{"key_id":"$(key_id "$K_SYSAPP")","key":"$(raw_pub "$K_SYSAPP")","roles":["platform-systemapp"]}],"oem_keys":[],"policy":{"require_oem_countersign":$REQUIRE_OEM_COUNTERSIGN}}
EOF
# The signature domain prefix matches the kernel's trustBundleSigDomain: nervus-trust-bundle-v1\0
{ printf 'nervus-trust-bundle-v1\0'; cat "$STAGE/trust/trust-bundle.json"; } > "$OUT_ROOT/bundle.msg"
openssl pkeyutl -sign -inkey "$K_ROOT" -rawin -in "$OUT_ROOT/bundle.msg" 2>/dev/null \
	| base64 -w0 > "$STAGE/trust/trust-bundle.sig"
ok "trust-bundle.json + .sig (2 platform roles)"

step "⑤ Bundling the platform JRE"
if [[ -n "$JRE_SOURCE" ]]; then
	mkdir -p "$STAGE/jre"
	cp -r "$JRE_SOURCE"/. "$STAGE/jre/"
	ok "JRE bundled ($(du -sh "$STAGE/jre" | cut -f1))"
else
	info "no JRE provided: the installer will apt install openjdk-21-jre on the target"
	info "${c_yellow}Note: it must be the full build, not -headless, or java.desktop is missing and Compose can't run${c_reset}"
fi

step "⑥ Assembling"

# Source files the release chain needs; none of them may be missing.
#
# [Why check them all up front instead of copying until something fails]: the consequences differ a lot.
# Without nervud.service the build breaks on the spot (cp reports No such file), but without fix-display.sh
# the package just lacks a repair script, and the old `|| true` on that line let it slip through quietly.
# Saying what's missing and why it's needed, all at once, beats a cp error halfway through.
declare -A REQUIRED_FILES=(
	["nervud.service"]="nervud's systemd unit. Installed to /etc/systemd/system/ on the target; the kernel can't run without it"
	["provision-rootfs.sh"]="lays out the directories. install-nervus.sh runs it; the install fails without it"
	["install-nervus.sh"]="the installer itself, shipped inside the release"
	["fix-display.sh"]="display session repair. With a black screen the target often only has ssh, so it must be in the package"
	["check-display-sync.sh"]="checks before release that the two scripts above write the same display configuration"
	["journald-nervus.conf"]="journald persistence and size. Without it logs stay in tmpfs and a reboot loses them all"
)
missing_src=()
for f in "${!REQUIRED_FILES[@]}"; do
	[[ -f "$SCRIPT_DIR/$f" ]] || missing_src+=("$f")
done
if (( ${#missing_src[@]} )); then
	echo >&2
	for f in "${missing_src[@]}"; do
		printf '    %smissing%s %-22s %s\n' "$c_red" "$c_reset" "$f" "${REQUIRED_FILES[$f]}" >&2
	done
	die "the source files above are missing from deploy/.
     They aren't build output; they're part of the release chain and must not be deleted or moved.
     The only build output is the tar.gz in out/, and that is what's safe to wipe."
fi

cp "$SCRIPT_DIR/nervud.service"        "$STAGE/systemd/"
cp "$SCRIPT_DIR/journald-nervus.conf"  "$STAGE/systemd/"
cp "$SCRIPT_DIR/provision-rootfs.sh"   "$STAGE/scripts/"
cp "$SCRIPT_DIR/install-nervus.sh"     "$STAGE/"
cp "$SCRIPT_DIR/fix-display.sh"        "$STAGE/scripts/"

# Both scripts embed the same display configuration, and they drifted once (symptom: black screen after install,
# fixed by running fix-display.sh by hand). Stop it before release so a mismatched version never ships
"$SCRIPT_DIR/check-display-sync.sh" >/dev/null 2>&1 \
	|| die "install-nervus.sh and fix-display.sh have different display configurations.
     Run ./check-display-sync.sh to see the difference."
ok "display configuration matches in both places"

chmod +x "$STAGE"/scripts/*.sh "$STAGE"/install-nervus.sh

# The RELEASE file is a manifest for people and what the installer checks itself against.
#
# [Values with spaces must be quoted]. install-nervus.sh used to read it with source, so
# built_on=Linux 6.6.114 aarch64 was parsed as "assign Linux, then run 6.6.114",
# failing with "6.6.114: command not found", which looks unrelated to the script.
# The installer now extracts values with sed and no longer sources it, but the values stay quoted: people read this file too,
# and downstream readers shouldn't be assumed to parse it safely.
cat > "$STAGE/RELEASE" <<EOF
nervus_release_format=1
built_at="$(date -Iseconds)"
built_on="$(uname -srm)"
target_abi=$TARGET_ABI
goarch=$GOARCH
go_version=$GO_VER
platform_root_key_id=$(key_id "$K_ROOT")
platform_release_key_id=$(key_id "$K_RELEASE")
platform_systemapp_key_id=$(key_id "$K_SYSAPP")
require_oem_countersign=$REQUIRE_OEM_COUNTERSIGN
bundled_jre=$([[ -n "$JRE_SOURCE" ]] && echo yes || echo no)
packages="$(ls -1 "$STAGE/system-packages" 2>/dev/null | tr '\n' ' ')"
EOF

# Output goes to out/, not the deploy/ root.
#
# [Why a separate directory]: every file in deploy/ is a required link in the release chain
# (see the file list in the README), but with a 180 MB tar.gz lying next to them the whole directory
# looks like a junk drawer. It really happened: someone tidied the "loose files" into old/, took nervud.service
# and provision-rootfs.sh with them, and the build broke on the spot.
# With sources and output kept apart, nobody has to guess what's safe to delete.
ask TARBALL "output tarball path" "$SCRIPT_DIR/out/nervus-os-$TARGET_ABI-$(date +%Y%m%d-%H%M).tar.gz"
mkdir -p "$(dirname "$TARBALL")"
tar -C "$OUT_ROOT" -czf "$TARBALL" nervus-release
sha256sum "$TARBALL" > "$TARBALL.sha256"

echo
printf '%sBuild complete%s\n' "$c_bold$c_green" "$c_reset"
printf '  %s\n' "$TARBALL"
printf '  %s\n' "$(du -h "$TARBALL" | cut -f1)  sha256: $(cut -c1-16 < "$TARBALL.sha256")…"
echo
cat "$STAGE/RELEASE" | sed 's/^/  /'
echo
printf '%sTo install on the target:%s\n' "$c_bold" "$c_reset"
printf '  tar xzf %s\n' "$(basename "$TARBALL")"
printf '  sudo ./nervus-release/install-nervus.sh %s\n' "$(basename "$TARBALL")"
echo
