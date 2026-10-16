#!/usr/bin/env bash
# Builds, packages and signs this repository's system services, producing directory trees that can be put
# straight into /usr/lib/nervus/system-packages/.
#
# Usage:
#   scripts/build-image-tree.sh <output dir> <signing-key.pem> [ABI]
#
# ABI defaults to linux-arm64 (RK3588 target). Use linux-x86_64 when running nervud on a PC.
#
# The output is an [unpacked directory tree], not a .nspkg: the kernel's boot scan globs
# <systemPackagesDir>/*/manifest.json. .nspkg is the zstd+tar format for dynamic install (nervusctl install),
# and the two paths must not be mixed.
#
# Generate the private key:
#   openssl genpkey -algorithm ed25519 -out signing/platform-release.pem

set -euo pipefail

OUT="${1:?usage: $0 <output dir> <private-key.pem> [ABI]}"
KEY="${2:?usage: $0 <output dir> <private-key.pem> [ABI]}"
ABI="${3:-linux-arm64}"

cd "$(dirname "$0")/.."
REPO="$PWD"

# ABI token → GOARCH. The kernel only accepts these three canonical tokens, not Android NDK names
# (arm64-v8a) or bare CPU names (aarch64), and doesn't normalise them, so neither do we.
case "$ABI" in
	linux-arm64)   GOARCH=arm64 ;;
	linux-armv7)   GOARCH=arm   ;;
	linux-x86_64)  GOARCH=amd64 ;;
	*) echo "unsupported ABI: $ABI (only linux-arm64 / linux-armv7 / linux-x86_64)" >&2; exit 1 ;;
esac

# Every system service in this repository: <package_id>:<service directory>
#
# The service directory is also the binary name and the Go package path (./<dir>), and the manifest template is always
# <dir>/manifest.json.in: everything about a service is in its own directory.
# To add a service, add a line here.
SERVICES=(
	"nervus.pkgmanagerd:pkgmanagerd"
	"nervus.safety.recovery:safetyrecoveryd"
	"nervus.camerad:camerad"
)

# Board camera configuration. camerad's resource declarations are generated from it, so [a different board needs a new package]:
# the board config goes into the digests and is signed with the image, which closes off the trick of
# adding yourself a "front camera" by editing one line of JSON at run time.
BOARD="${NERVUS_BOARD:-reference}"

echo "==> target ABI: $ABI (GOARCH=$GOARCH)"
echo "==> board config: $BOARD"
mkdir -p "$OUT"

for entry in "${SERVICES[@]}"; do
	pkg="${entry%%:*}"
	svc="${entry##*:}"
	tmpl="$REPO/$svc/manifest.json.in"

	if [[ ! -f "$tmpl" ]]; then
		echo "manifest template missing: $tmpl" >&2
		exit 1
	fi

	# The output directory name [must] equal the package_id: the kernel identifies packages by manifest content and the directory is just a
	# container, but a mismatch sends people debugging to the wrong package.
	dir="$OUT/$pkg"
	echo "==> building $pkg (./$svc)"
	rm -rf "$dir"
	mkdir -p "$dir/bin"

	# CGO_ENABLED=0: system services must be static. The target's glibc may differ from the build machine's,
	# system packages run sandboxed with ProtectSystem=strict, and a missing library for a dynamically linked binary
	# shows up as "the process exits as soon as it starts", with systemd only reporting an exit code: painful to debug.
	CGO_ENABLED=0 GOOS=linux GOARCH="$GOARCH" \
		go build -trimpath -ldflags "-s -w" -o "$dir/bin/$svc" "./$svc"

	# Service-specific data files. They must be on disk before providergen (which reads them) and before
	# sysmanifest (which computes the digests).
	#
	# So far only camerad: its resource declarations come from the board JSON, and [the same file] is read
	# again at run time for the role → device mapping. As two copies, "the Catalog has cam.front" and
	# "which device cam.front points at at run time" would drift apart independently, with neither side reporting an error.
	providerArgs=()
	if [[ "$svc" == "camerad" ]]; then
		board="$REPO/camerad/boards/$BOARD.json"
		if [[ ! -f "$board" ]]; then
			echo "board config missing: $board (set it with NERVUS_BOARD)" >&2
			exit 1
		fi
		cp "$board" "$dir/board.json"
		providerArgs=(-profile "$dir/board.json")
	fi

	# Provider contract: services that export interfaces [must] carry provider.binpb + schemas.binpb,
	# or the kernel's loadRequiredProviderArtifacts refuses to load them with ErrProviderArtifactsRequired.
	#
	# It must run before sysmanifest, which computes the digests; later and these two files aren't in the list,
	# and the kernel refuses with "provider not covered by digests", an error far from "the build order was wrong".
	#
	# Built for the host ABI (no GOARCH): providergen is a build-time tool that runs on the build machine
	# and produces architecture-independent protobuf bytes. Cross-compiling it would only give a binary that can't run.
	if [[ -d "$REPO/$svc/providergen" ]]; then
		echo "    provider contract"
		go run "./$svc/providergen" -out "$dir" "${providerArgs[@]}"
	fi

	# The command-line ABI wins: the same template must build for several ABIs.
	#
	# [The temporary template must live outside the package directory]. Inside $dir, sysmanifest would include it in the
	# digests (it's still there at that point), and deleting it afterwards leaves a digest entry pointing at a missing
	# file, so the kernel's VerifyDigests refuses the whole package with Missing, and the error looks
	# like "corrupt image", enough to send someone checking hardware first.
	tmplTmp="$(mktemp)"
	trap 'rm -f "$tmplTmp"' EXIT
	sed "s/\"linux-arm64\"/\"$ABI\"/" "$tmpl" > "$tmplTmp"

	go run ./scripts/sysmanifest -dir "$dir" -template "$tmplTmp" -key "$KEY"
	rm -f "$tmplTmp"
done

echo
echo "==> output"
find "$OUT" -type f | sort | sed 's|^|  |'
echo
