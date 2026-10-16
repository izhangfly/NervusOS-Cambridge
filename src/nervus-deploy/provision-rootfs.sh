#!/usr/bin/env bash
# Lays out the Nervus OS directory tree on a [mounted target rootfs].
#
#   sudo ./provision-rootfs.sh /mnt/target-rootfs
#
# nervud's preflight checks these directories and modes one by one (internal/preflight/
# preflight.go). A mismatch in the read-only image area is fatal and nervud refuses to start; drift in
# the writable areas gets fixed in place, but relying on that is a bad habit. Get the image right the first time.
#
# This script [only creates directories]; it doesn't place binaries. For deploying binaries see README.md.
set -euo pipefail

ROOT="${1:?usage: $0 <target rootfs mount point>}"
[[ -d "$ROOT" ]] || { echo "not a directory: $ROOT" >&2; exit 1; }

# directory:mode
# Order matters: parents first. install -d doesn't fail when the parent exists, but modes
# must be set from the outside in, or recreating a parent could override a child's setting.
LAYOUT=(
	# ---- read-only image area ----
	"usr/libexec/nervus:0755"
	"usr/lib/nervus:0755"
	"usr/lib/nervus/system-packages:0755"   # system services and system apps both live here
	"usr/lib/nervus/jre:0755"               # the platform JRE (for runtime=jvm components)
	"usr/share/nervus:0755"
	"usr/share/nervus/trust:0755"           # the trust root bundle

	# ---- logs ----
	# journald's persistent directory. [Whether it exists is the switch]: with Storage=auto and no such
	# directory, journald falls back to /run/log/journal (tmpfs), and everything before a reboot is lost.
	#
	# 2755 = setgid; journald uses the group bit (systemd-journal) to control who can read.
	# The directory alone isn't enough; size and Storage come from journald-nervus.conf, see the README.
	"var/log/journal:2755"

	# ---- nervud's own writable area ----
	"var/lib/nervus:0755"
	"var/lib/nervus/registry:0700"          # per-package records + UID allocator, only nervud can read it
	"var/lib/nervus/packages:0755"          # code of dynamically installed packages
	"var/lib/nervus/package-data:0755"      # [root] of each package's private data dir; the kernel creates the children
	"var/lib/nervus/staging:0711"           # install staging; 0711 = services can pass through but can't list it
	"var/lib/nervus/jvm-cache:0755"
	# The user documents area shared across packages. 1777 = sticky + world-writable, the same shape as /tmp:
	# anyone can put things in, but [can only delete their own]. Only packages that declare perm.storage.user
	# get write access to it inside their sandbox.
	"var/lib/nervus/user-data:1777"
)

echo "==> laying out directories under $ROOT"
for entry in "${LAYOUT[@]}"; do
	rel="${entry%%:*}"
	mode="${entry##*:}"
	install -d -m "$mode" -o 0 -g 0 "$ROOT/$rel"
	printf '    %-42s %s\n' "/$rel" "$mode"
done

# /run/nervus is [not created here]: /run is tmpfs and rebuilt on every boot.
# nervud.service's RuntimeDirectory=nervus + RuntimeDirectoryMode=1755
# takes care of it; one created on the rootfs would just be hidden by the tmpfs.
echo
echo "    /run/nervus is created by nervud.service's RuntimeDirectory; this script leaves it alone"

echo
echo "==> done. Next, place the binaries and signed artifacts; see section 3 of README.md."
