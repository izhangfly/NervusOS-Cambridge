#!/usr/bin/env bash
# Installs a Nervus OS release on [this machine].
#
#   sudo ./install-nervus.sh nervus-os-linux-x86_64-*.tar.gz
#
# This script changes the machine: it installs dependencies, lays out directories, writes systemd units, and may take over the display session.
# It is idempotent and always overwrites files, so running it again always gives a consistent state.
set -euo pipefail

c_reset=$'\033[0m'; c_bold=$'\033[1m'; c_dim=$'\033[2m'
c_red=$'\033[31m'; c_green=$'\033[32m'; c_yellow=$'\033[33m'; c_blue=$'\033[36m'

step() { printf '\n%s==> %s%s\n' "$c_bold$c_blue" "$*" "$c_reset"; }
info() { printf '    %s\n' "$*"; }
ok()   { printf '    %s✓%s %s\n' "$c_green" "$c_reset" "$*"; }
warn() { printf '    %s!%s %s\n' "$c_yellow" "$c_reset" "$*" >&2; }
die()  { printf '\n%s✗ %s%s\n' "$c_red$c_bold" "$*" "$c_reset" >&2; exit 1; }

confirm() {
	local prompt="$1" default="${2:-y}" reply
	[[ "${ASSUME_YES:-0}" == 1 ]] && { [[ "$default" == y ]]; return; }
	read -r -p "    $prompt [$( [[ $default == y ]] && echo 'Y/n' || echo 'y/N' )]: " reply </dev/tty
	reply="${reply:-$default}"
	[[ "$reply" =~ ^[Yy] ]]
}

[[ $EUID -eq 0 ]] || die "must run with sudo: sudo $0 $*"

# ================================================================= get the tarball
TARBALL="${1:-}"
if [[ -z "$TARBALL" ]]; then
	read -r -p "    release tarball path: " TARBALL </dev/tty
fi
[[ -f "$TARBALL" ]] || die "release not found: $TARBALL"
TARBALL="$(readlink -f "$TARBALL")"

if [[ -f "$TARBALL.sha256" ]]; then
	(cd "$(dirname "$TARBALL")" && sha256sum -c "$(basename "$TARBALL").sha256" --quiet) \
		&& ok "checksum OK" || die "checksum mismatch; the package may be corrupt"
fi

WORK="$(mktemp -d)"; trap 'rm -rf "$WORK"' EXIT
tar -C "$WORK" -xzf "$TARBALL"
SRC="$WORK/nervus-release"
[[ -f "$SRC/RELEASE" ]] || die "bad package layout: nervus-release/RELEASE is missing"

# Read fields from RELEASE.
#
# [Not with source]. RELEASE has values with spaces (built_on=Linux 6.6 aarch64), and
# source parses that as "assign the first word, then run the rest as a command":
# the error is "6.6.114.1-...: command not found", which looks unrelated to the script.
# And sourcing a file that came inside a package means executing it, which we shouldn't do anyway.
release_get() {
	local v
	v="$(sed -n "s/^$1=//p" "$SRC/RELEASE" | head -1)"
	# Tolerate quotes around values (newer RELEASE files quote them, older packages don't)
	v="${v%\"}"; v="${v#\"}"
	printf '%s' "$v"
}

target_abi="$(release_get target_abi)"
bundled_jre="$(release_get bundled_jre)"
platform_root_key_id="$(release_get platform_root_key_id)"
[[ -n "$target_abi" ]] || die "RELEASE has no target_abi; the package may be incomplete"

step "Release information"
sed 's/^/    /' "$SRC/RELEASE"

# Architecture check. Installing the wrong architecture shows up as "Exec format error" on every binary,
# and systemd only reports 203/EXEC, which doesn't say it's the architecture
HOST_ARCH="$(uname -m)"
case "$target_abi" in
	linux-arm64)  want_arch="aarch64" ;;
	linux-x86_64) want_arch="x86_64" ;;
	*) die "unrecognised target_abi in the package: $target_abi" ;;
esac
if [[ "$HOST_ARCH" != "$want_arch" ]]; then
	warn "this machine is $HOST_ARCH, the package is for $want_arch"
	confirm "Continue anyway (it almost certainly won't run)?" n || die "architecture mismatch, aborted"
fi
ok "architecture $HOST_ARCH matches"

# ================================================================= detect the environment
step "Detecting the environment"

IS_WSL=0
if [[ -n "${WSL_DISTRO_NAME:-}" ]] || grep -qi microsoft /proc/version 2>/dev/null; then
	IS_WSL=1
	ok "WSL ($(grep -o 'WSL[0-9]*' /proc/version 2>/dev/null | head -1 || echo WSL))"
fi

command -v systemctl >/dev/null 2>&1 || die "no systemd. nervud has PID 1 start every component over D-Bus; it can't run without systemd"
if ! systemctl is-system-running --quiet 2>/dev/null && ! systemctl list-units --type=service >/dev/null 2>&1; then
	if (( IS_WSL )); then
		die "systemd isn't enabled in WSL. Add to /etc/wsl.conf:
       [boot]
       systemd=true
     then run wsl --shutdown on Windows, come back in and run this again"
	fi
	die "systemd unavailable"
fi
ok "systemd available"

command -v apt-get >/dev/null 2>&1 || die "this script only supports Debian/Ubuntu (it needs apt-get)"

# WSLg: Microsoft has already put an X socket in /tmp/.X11-unix and set DISPLAY.
# In that case don't start our own Xorg; it would fight WSLg for the display
HAS_WSLG=0
if (( IS_WSL )) && [[ -d /tmp/.X11-unix ]] && compgen -G '/tmp/.X11-unix/X*' >/dev/null; then
	HAS_WSLG=1
	ok "WSLg provides an X display ($(ls /tmp/.X11-unix | tr '\n' ' '))"
fi

# ================================================================= dependencies
step "Checking and installing dependencies"

PKGS=()
have() { dpkg -s "$1" >/dev/null 2>&1; }

have dbus || PKGS+=(dbus)
	# xauth generates the shared X cookie; x11-xserver-utils provides xhost/xrandr.
	have xauth || PKGS+=(xauth)
	have x11-xserver-utils || PKGS+=(x11-xserver-utils)
	# glxinfo -B is the most direct evidence of whether Skiko ends up on a real GPU or on llvmpipe.
	have mesa-utils || PKGS+=(mesa-utils)
	# The system navigation bar activates windows through EWMH and delivers Back to the focused window with XTEST.
	have wmctrl || PKGS+=(wmctrl)
	have xdotool || PKGS+=(xdotool)

	if (( ! HAS_WSLG )); then
		# We need to start our own X server
		have xserver-xorg-core || PKGS+=(xserver-xorg-core xserver-xorg-video-all xserver-xorg-input-all xinit)
		# Bare Xorg doesn't manage windows. Openbox handles the single foreground window, focus history and the space for the right-hand bar.
		have openbox || PKGS+=(openbox)
	fi

NEED_APT_JRE=0
JRE_APT_PKG=""
if [[ "${bundled_jre:-no}" != "yes" && ! -x /usr/lib/nervus/jre/bin/java ]]; then
	NEED_APT_JRE=1
	# [It must be a full JRE, not -headless]: headless lacks java.desktop, Compose can't start,
	# and it fails with NoClassDefFoundError: java/awt/*, which doesn't say the JRE was trimmed.
	#
	# [It must be ≥21]: the manifests' runtime_deps.min_java_release is 21.
	#
	# Together these are a real problem on Debian 12 (bookworm): its main repository only has
	# openjdk-17, with 21 in backports. Raspberry Pi OS is based on bookworm and runs straight into it.
	for cand in openjdk-21-jre openjdk-22-jre openjdk-23-jre; do
		if apt-cache show "$cand" >/dev/null 2>&1; then JRE_APT_PKG="$cand"; break; fi
	done
	if [[ -z "$JRE_APT_PKG" ]]; then
		die "the apt sources have no full JRE of openjdk-21 or later (this machine is $( . /etc/os-release 2>/dev/null; echo "${PRETTY_NAME:-unknown distribution}") ).

     Three ways out; pick one:

     ① Bundle it in the release (recommended; the target needs no network)
        Download a $target_abi JDK/JRE on the build machine, then
          JRE_SOURCE=/path/to/jre ./build-release.sh
        The build script checks right away that it includes java.desktop.

     ② Install from backports (Debian 12 / Raspberry Pi OS)
          echo 'deb http://deb.debian.org/debian bookworm-backports main' \\
            | sudo tee /etc/apt/sources.list.d/backports.list
          sudo apt update && sudo apt install -t bookworm-backports openjdk-21-jre

     ③ Install an Adoptium build
          https://adoptium.net/temurin/releases/?arch=aarch64&os=linux
        Unpack it and pass the directory to JRE_SOURCE in ①, or put it straight into /usr/lib/nervus/jre

     Do [not] install a -headless build: it lacks java.desktop, and Compose can't run a single line."
	fi
	have "$JRE_APT_PKG" || PKGS+=("$JRE_APT_PKG")
	info "JRE to install: $JRE_APT_PKG"
fi

if (( ${#PKGS[@]} )); then
	info "will install: ${PKGS[*]}"
	confirm "Continue?" y || die "missing dependencies, aborted"
	apt-get update -qq
	DEBIAN_FRONTEND=noninteractive apt-get install -y -qq "${PKGS[@]}"
	ok "dependencies installed"
else
	ok "all dependencies present"
fi

# ================================================================= stop old services
step "Stopping existing Nervus services"
systemctl stop nervud.service 2>/dev/null || true
systemctl stop 'nervus-*.service' 2>/dev/null || true
# nervud starts the transient units, and when nervud stops they stop too through BindsTo; this is a safety net
for u in $(systemctl list-units --all --plain --no-legend 'nervus-*' 2>/dev/null | awk '{print $1}'); do
	systemctl stop "$u" 2>/dev/null || true
done
ok "stopped"

# ================================================================= lay out directories
step "Laying out directories"
"$SRC/scripts/provision-rootfs.sh" / | sed 's/^/    /'

# ================================================================= log retention
#
# [This must come before installing]. The install writes logs too, and when an install fails those logs
# are often the only clue; done later, a failed half-install followed by a reboot would lose exactly what you need.
step "Configuring log retention"
install -D -m 0644 -o 0 -g 0 "$SRC/systemd/journald-nervus.conf" \
	/etc/systemd/journald.conf.d/10-nervus.conf

# provision-rootfs.sh creates the directory; this covers machines upgraded from older versions.
install -d -m 2755 -o 0 -g 0 /var/log/journal

systemctl restart systemd-journald 2>/dev/null || warn "restarting systemd-journald failed"
# Move what's already in /run/log/journal into /var/log/journal,
# and make journald actually switch to persistent mode (without a flush it keeps using volatile storage).
journalctl --flush 2>/dev/null || true

# [Check that it really took effect].
#
# The symptom of a bad config is "everything's fine until the day you need the logs"; by then the machine has rebooted,
# they're gone, and nothing points at the configuration.
if journalctl --header 2>/dev/null | grep -q '/var/log/journal'; then
	# head returns 0 on empty input, so a `|| echo ?` at the end of the pipe never fires;
	# take the value, then test for empty.
	journal_used="$(journalctl --disk-usage 2>/dev/null | grep -oE '[0-9.]+[KMGT]' | head -1)"
	ok "logs persisted to /var/log/journal (cap 500 MB, current ${journal_used:-unknown})"
else
	# Not die: logs not persisting shouldn't stop a robot from installing. But it must be loud, or it might as well not be said.
	warn "logs are [still in volatile storage] and will all be lost on reboot."
	info "  check whether /etc/systemd/journald.conf hard-codes Storage=volatile"
	info "  (Raspberry Pi OS images after 2025-05-13 do)"
	info "  or just run: journalctl --header | grep journal"
fi

# ================================================================= install files
step "Installing the kernel and ops tools"
install -D -m 0755 -o 0 -g 0 "$SRC/bin/nervud"    /usr/libexec/nervus/nervud
install -D -m 0755 -o 0 -g 0 "$SRC/bin/nervusctl" /usr/bin/nervusctl
ok "/usr/libexec/nervus/nervud"
ok "/usr/bin/nervusctl"

step "Installing system packages (services + apps)"
# Overwrite: an existing package's old content must be replaced entirely, with no leftover files from the previous version;
# manifest.digests would count extra files as Extra and refuse to load the whole package
for pkgdir in "$SRC"/system-packages/*/; do
	[[ -d "$pkgdir" ]] || continue
	name="$(basename "$pkgdir")"
	rm -rf "/usr/lib/nervus/system-packages/$name"
	cp -r "$pkgdir" "/usr/lib/nervus/system-packages/$name"
	chown -R 0:0 "/usr/lib/nervus/system-packages/$name"
	find "/usr/lib/nervus/system-packages/$name" -type d -exec chmod 0755 {} +
	find "/usr/lib/nervus/system-packages/$name" -type f -exec chmod 0644 {} +
	# A native component's entry must be executable; a jvm component's entry is a jar, so 0644 is fine
	if [[ -d "/usr/lib/nervus/system-packages/$name/bin" ]]; then
		chmod 0755 "/usr/lib/nervus/system-packages/$name/bin"/* 2>/dev/null || true
	fi
	ok "$name ($(du -sh "$pkgdir" | cut -f1))"
done

step "Installing the trust root"
install -D -m 0644 -o 0 -g 0 "$SRC/trust/trust-bundle.json" /usr/share/nervus/trust/trust-bundle.json
install -D -m 0644 -o 0 -g 0 "$SRC/trust/trust-bundle.sig"  /usr/share/nervus/trust/trust-bundle.sig
ok "trust-bundle (root key_id ${platform_root_key_id:-?})"

step "Installing the platform JRE"
if [[ -d "$SRC/jre" ]]; then
	rm -rf /usr/lib/nervus/jre; mkdir -p /usr/lib/nervus/jre
	cp -r "$SRC"/jre/. /usr/lib/nervus/jre/
	chown -R 0:0 /usr/lib/nervus/jre
	ok "installed from the package"
elif (( NEED_APT_JRE )); then
	# Copy from where apt installed it. Use dpkg's file list rather than `which java`:
	# that may be an update-alternatives symlink, or even point to a JRE of another version
	SYS_JRE=""
	for d in $(dpkg -L "$JRE_APT_PKG" 2>/dev/null | grep -E '/bin/java$' | xargs -r -n1 dirname | xargs -r -n1 dirname); do
		[[ -x "$d/bin/java" ]] && { SYS_JRE="$d"; break; }
	done
	if [[ -z "$SYS_JRE" ]] && command -v java >/dev/null 2>&1; then
		SYS_JRE="$(dirname "$(dirname "$(readlink -f "$(command -v java)")")")"
	fi
	[[ -x "$SYS_JRE/bin/java" ]] || die "$JRE_APT_PKG is installed but its java executable can't be found"
	rm -rf /usr/lib/nervus/jre; mkdir -p /usr/lib/nervus/jre
	cp -r "$SYS_JRE"/. /usr/lib/nervus/jre/
	ok "copied from $SYS_JRE"
else
	ok "already present, skipped"
fi

# This check deserves its own step: it's the last unknown for whether the GUI can start,
# and the failure (NoClassDefFoundError: java/awt/*) doesn't point at a trimmed JRE
if /usr/lib/nervus/jre/bin/java --list-modules 2>/dev/null | grep -q '^java\.desktop'; then
	ok "JRE includes java.desktop (Compose can run)"
else
	die "the platform JRE lacks the java.desktop module, and Compose can't run a single line.
     Use a full JRE (not -headless), or add --add-modules java.desktop when running jlink"
fi

# ================================================================= display session
step "Configuring the display"

DISPLAY_NUM=":0"

# The cookie goes in /run/nervus-x11, [not] /run/nervus.
#
# nervud.service has RuntimeDirectory=nervus, so systemd deletes all of
# /run/nervus when it stops. With the cookie there, every nervud restart (there's one at the end of install,
# and at boot nervud starting after X counts too) would wipe X's auth file with it,
# and apps connecting to X would get "No protocol specified": no desktop while X looks fine.
# nervus-x11.service gets its own RuntimeDirectory, so each lifetime is managed separately.
XAUTH_FILE="/run/nervus-x11/xauth"
START_OWN_X=0

if (( HAS_WSLG )); then
	# WSLg runs its own X. It sets DISPLAY, and the socket is already in /tmp/.X11-unix.
	# We only need to tell nervud about DISPLAY
	DISPLAY_NUM="${DISPLAY:-:0}"
	XAUTH_FILE="${XAUTHORITY:-}"
	info "reusing WSLg's display DISPLAY=$DISPLAY_NUM"
	info "${c_dim}not starting our own Xorg; it would fight WSLg for the display${c_reset}"
else
	# Look for an existing display manager. It would hold the tty and the display and clash with our session
	EXISTING_DM=""
	for dm in gdm3 gdm lightdm sddm lxdm xdm slim; do
		if systemctl list-unit-files "$dm.service" >/dev/null 2>&1 \
			&& systemctl is-enabled "$dm.service" >/dev/null 2>&1; then
			EXISTING_DM="$dm"; break
		fi
	done
	DEFAULT_TARGET="$(systemctl get-default 2>/dev/null || echo unknown)"

	if [[ -n "$EXISTING_DM" || "$DEFAULT_TARGET" == "graphical.target" ]]; then
		echo
		warn "an existing desktop environment was detected"
		[[ -n "$EXISTING_DM" ]] && info "display manager: $EXISTING_DM"
		info "default target: $DEFAULT_TARGET"
		cat <<-EOF

		    The Nervus desktop needs the display to itself. Continuing will:
		      · disable ${EXISTING_DM:-the existing display manager} (not uninstall it; it can be restored any time)
		      · set the default target to multi-user.target (no more booting into the old desktop)
		      · have nervus-x11.service start Xorg and run the Nervus desktop

		    The script also writes a restore script, so one command undoes it at any time.

		EOF
		if confirm "Replace the existing desktop environment with the Nervus desktop?" n; then
			# [It must be --now; disable alone isn't enough]
			#
			# disable only removes the boot symlink and [doesn't stop the running one]. The old display manager's X server
			# keeps holding DRM master and the VT, so our Xorg can't start, and once the unit hits
			# its start rate limit it's failed for good, leaving a black screen at the end of the install.
			# This happened on real hardware: black after install, black after reboot, fixed by running the display setup by hand again,
			# because by then the old display manager was gone.
			if [[ -n "$EXISTING_DM" ]]; then
				systemctl disable --now "$EXISTING_DM.service" >/dev/null 2>&1 || true
				ok "stopped and disabled $EXISTING_DM"
			fi
			# display-manager.service is the distribution's generic alias for the current display manager. Stop it as a fallback:
			# on some images the display manager is started through this alias and stopping it by its real name doesn't work.
			#
			# [Stop only, don't disable]: this "unit" is itself a symlink under /etc/systemd/system
			# pointing at the real display manager, and systemctl disable would delete that symlink.
			# The distribution's package manager created it, and without it nervus-restore-desktop can't restore anything.
			# Stopping is enough; not booting into it again comes from set-default multi-user.
			systemctl stop display-manager.service >/dev/null 2>&1 || true

			systemctl set-default multi-user.target >/dev/null 2>&1 || true
			ok "default target → multi-user.target"
			START_OWN_X=1

			install -D -m 0755 /dev/stdin /usr/local/sbin/nervus-restore-desktop <<-EOF
				#!/usr/bin/env bash
				# Restores this machine's original desktop environment (generated by install-nervus.sh)
				set -euo pipefail
				[[ \$EUID -eq 0 ]] || { echo "needs sudo" >&2; exit 1; }
				systemctl disable --now nervus-x11.service 2>/dev/null || true
				systemctl disable --now nervud.service 2>/dev/null || true
				${EXISTING_DM:+systemctl enable $EXISTING_DM.service}
				systemctl set-default ${DEFAULT_TARGET}
				echo "Restored. Reboot to return to the original desktop."
			EOF
			ok "restore script: sudo nervus-restore-desktop"
		else
			info "keeping the existing desktop. The Nervus desktop will try to connect to the existing X display"
			info "${c_dim}that only works if the display accepts other UIDs; otherwise the desktop won't start${c_reset}"
		fi
	else
		info "no desktop environment detected; nervus-x11.service will start Xorg"
		START_OWN_X=1
	fi
fi

# --- only needed when we start our own X ---
if (( START_OWN_X )); then
	# On Debian/Ubuntu, /usr/bin/Xorg is usually Xorg.wrap (xserver-xorg-legacy).
	# It checks callers against allowed_users in /etc/X11/Xwrapper.config, defaulting to console,
	# and processes started by systemd have no console, so it exits with
	# "Only console users are allowed to run the X server".
	# start-x11 prefers to exec the real binary directly and bypass the wrapper; this config is a second line of defence
	install -D -m 0644 /dev/stdin /etc/X11/Xwrapper.config <<-'EOF'
		# Written by install-nervus.sh: X is started as root by nervus-x11.service,
		# outside any login session, so it can't be limited to console users
		allowed_users=anybody
		needs_root_rights=yes
	EOF

	# Shared X cookie: the X server is started by root, while every app component runs under its own UID
	# (20000–59999). Without a cookie they can read, X refuses the connection
	# ("No protocol specified"). 0644 lets every app UID read it; that's the trade-off for a single-user
	# device: any process on the machine can connect to the display, the same as xhost +local:
	install -D -m 0755 /dev/stdin /usr/libexec/nervus/start-x11 <<-'EOF'
		#!/usr/bin/env bash
		# Start Xorg and prepare an auth cookie every app UID can read.
		set -euo pipefail
		# Not in /run/nervus: that's nervud's RuntimeDirectory, which systemd deletes as soon as nervud stops
		XAUTH=/run/nervus-x11/xauth
		XLOG=/var/log/nervus-x11.log

		install -d -m 0755 /run/nervus-x11
		rm -f "$XAUTH"; touch "$XAUTH"; chmod 0644 "$XAUTH"
		# mcookie comes from util-linux; fall back to openssl without it
		COOKIE="$(mcookie 2>/dev/null || openssl rand -hex 16)"
		xauth -f "$XAUTH" add :0 MIT-MAGIC-COOKIE-1 "$COOKIE"

		# Find the real X binary directly. /usr/lib/xorg/Xorg comes first to bypass
		# the /usr/bin/Xorg setuid wrapper (see the Xwrapper.config note above)
		XBIN=""
		for c in /usr/lib/xorg/Xorg /usr/bin/Xorg /usr/bin/X; do
			[[ -x "$c" ]] && { XBIN="$c"; break; }
		done
		[[ -n "$XBIN" ]] || { echo "Can't find the Xorg executable; it needs xserver-xorg-core" >&2; exit 78; }

		# Wait up to 30 seconds for a KMS device.
		#
		# [At boot this is essential]: this unit is ordered after systemd-user-sessions, but the GPU's
		# DRM driver is often a module udev loads a little later (especially on SoC boards). Starting
		# Xorg before it means an immediate exit with "no screens found", and starting it again by hand works,
		# because by then the driver has loaded. That's one cause of "black after install, black after reboot,
		# fixed after one manual repair".
		for _ in $(seq 60); do
			compgen -G '/dev/dri/card*' >/dev/null && break
			sleep 0.5
		done

		# Still nothing after the full wait = the kernel GPU driver really isn't loaded, a problem outside X.
		# Start Xorg anyway: letting it write the real error to its log is easier to debug than exiting here
		if ! compgen -G '/dev/dri/card*' >/dev/null; then
			echo "Warning: after 30 seconds there is still no card device under /dev/dri; the GPU KMS driver may not be loaded" >&2
			echo "      Xorg will most likely fail with no screens found; see $XLOG" >&2
		fi

		# [It must be vt7, not vt1]
		#
		# getty@tty1 runs on vt1. Xorg taking it over clears tty1, and if Xorg then crashes while the unit is
		# Restart=always, the screen stays black for good. Not even console text comes back; the machine looks
		# dead and only ssh gets in. That's why every display manager uses vt7.
		#
		# [-ac: drop access control for local clients]
		#
		# The cookie path depends on a chain that breaks easily: nervud's environment must have XAUTHORITY
		# → only then does the kernel bind the cookie file into the component's mount namespace (supervise.go) →
		# only then can the component get it. If any link breaks, the symptom is always the same
		# "Authorization required, but no authorization protocol specified"
		# The component reaches the socket, just without credentials, and the log doesn't say where the chain broke.
		#
		# -ac makes the server not require credentials. This does [not] expose the display: -nolisten tcp already
		# turns off network listening, so only local processes can connect. And the cookie is 0644 and
		# readable machine-wide (see the comment above), so security was already equivalent to xhost +local:;
		# this just makes that existing trade-off take effect reliably.
		# -auth stays: clients with the cookie still connect with credentials as usual.
		exec "$XBIN" :0 vt7 -auth "$XAUTH" -nolisten tcp -noreset -ac -logfile "$XLOG"
	EOF

	install -D -m 0644 /dev/stdin /etc/systemd/system/nervus-x11.service <<-EOF
		[Unit]
		Description=Nervus X11 display server
		# The window manager and nervud's components all connect to it, so it must be ready first
		Before=nervus-window-manager.service nervud.service
		After=systemd-user-sessions.service

		# [No start rate limit].
		#
		# It used to fail permanently after 5 starts in 60 seconds. For a device that should show a desktop from boot,
		# giving up means a permanently black screen and someone ssh-ing in to rescue it by hand, which is exactly how it broke on real hardware.
		# The reasons X can't start during boot (driver not loaded yet, an old display manager still holding DRM) are nearly always temporary
		# and worth retrying indefinitely.
		#
		# Conversely, the rate limit was there to stop "a crash loop grabbing the VT and pinning the screen";
		# vt7 solves that (the console stays on vt1, so text is visible even if X crashes).
		StartLimitIntervalSec=0

		[Service]
		Type=simple
		ExecStart=/usr/libexec/nervus/start-x11
		Restart=always
		# 5 s rather than 2 s: with unlimited retries, a bit slower floods neither the log nor the CPU
		RestartSec=5s
		# Its own /run directory, independent of nervud's RuntimeDirectory=nervus
		RuntimeDirectory=nervus-x11
		RuntimeDirectoryMode=0755
		# Xorg touches the GPU and input devices directly, so it can't be sandboxed
		StandardOutput=journal
		StandardError=journal
		SyslogIdentifier=nervus-x11

		[Install]
		WantedBy=multi-user.target
		EOF
		ok "nervus-x11.service (vt7 + Xwrapper allowed)"

	# Openbox makes ordinary apps behave like Android's single foreground window:
	# no title bar, maximised to the area left of the system bar, only the top one visible at a time.
	install -D -m 0644 /dev/stdin /etc/nervus/openbox/rc.xml <<-'EOF'
		<?xml version="1.0" encoding="UTF-8"?>
		<openbox_config xmlns="http://openbox.org/3.4/rc">
		  <focus>
		    <focusNew>yes</focusNew>
		    <focusLast>yes</focusLast>
		    <followMouse>no</followMouse>
		    <underMouse>no</underMouse>
		    <raiseOnFocus>no</raiseOnFocus>
		  </focus>
		  <placement>
		    <policy>Smart</policy>
		    <center>yes</center>
		  </placement>
		  <theme>
		    <name>Clearlooks</name>
		    <keepBorder>no</keepBorder>
		    <animateIconify>no</animateIconify>
		  </theme>
		  <desktops>
		    <number>1</number>
		    <firstdesk>1</firstdesk>
		    <popupTime>0</popupTime>
		  </desktops>
		  <margins>
		    <top>0</top>
		    <left>0</left>
		    <right>80</right>
		    <bottom>0</bottom>
		  </margins>
		  <applications>
		    <application class="*" type="normal">
		      <decor>no</decor>
		      <maximized>yes</maximized>
		    </application>
		    <application class="*" title="Nervus Navigation">
		      <decor>no</decor>
		      <maximized>no</maximized>
		      <position force="yes">
		        <x>-0</x>
		        <y>0</y>
		        <monitor>1</monitor>
		      </position>
		      <size>
		        <width>80</width>
		        <height>100%</height>
		      </size>
		      <desktop>all</desktop>
		      <layer>above</layer>
		      <focus>no</focus>
		      <skip_taskbar>yes</skip_taskbar>
		      <skip_pager>yes</skip_pager>
		    </application>
		  </applications>
		  <keyboard>
		    <chainQuitKey>C-g</chainQuitKey>
		    <keybind key="A-Tab">
		      <action name="NextWindow"/>
		    </keybind>
		  </keyboard>
		</openbox_config>
	EOF

	install -D -m 0755 /dev/stdin /usr/libexec/nervus/start-window-manager <<-'EOF'
		#!/usr/bin/env bash
		set -euo pipefail
		export DISPLAY=:0
		export XAUTHORITY=/run/nervus-x11/xauth
		for _ in $(seq 40); do
			[[ -S /tmp/.X11-unix/X0 ]] && break
			sleep 0.25
		done
		[[ -S /tmp/.X11-unix/X0 ]] || { echo "X11 socket did not appear" >&2; exit 75; }
		exec /usr/bin/openbox --config-file /etc/nervus/openbox/rc.xml
	EOF

	install -D -m 0644 /dev/stdin /etc/systemd/system/nervus-window-manager.service <<-'EOF'
		[Unit]
		Description=Nervus single-window manager
		Requires=nervus-x11.service
		After=nervus-x11.service
		Before=nervud.service

		[Service]
		Type=simple
		ExecStart=/usr/libexec/nervus/start-window-manager
		Restart=always
		RestartSec=2s
		StandardOutput=journal
		StandardError=journal
		SyslogIdentifier=nervus-window-manager

		[Install]
		WantedBy=multi-user.target
	EOF
	ok "nervus-window-manager.service (one window at a time + 80px system bar on the right)"
	fi

# ================================================================= systemd
step "Installing systemd units"
install -D -m 0644 -o 0 -g 0 "$SRC/systemd/nervud.service" /etc/systemd/system/nervud.service

# DISPLAY must be in nervud's own environment: when the kernel injects DISPLAY into type=app components,
# it takes nervud's environment variables (displayEnv in service/supervise.go)
mkdir -p /etc/systemd/system/nervud.service.d
{
	printf '[Unit]\n'
		(( START_OWN_X )) && printf 'After=nervus-x11.service nervus-window-manager.service\nWants=nervus-x11.service nervus-window-manager.service\n'
	printf '\n[Service]\n'
	printf 'Environment=DISPLAY=%s\n' "$DISPLAY_NUM"
	[[ -n "$XAUTH_FILE" ]] && printf 'Environment=XAUTHORITY=%s\n' "$XAUTH_FILE"
} > /etc/systemd/system/nervud.service.d/10-display.conf
ok "nervud.service + display environment drop-in (DISPLAY=$DISPLAY_NUM)"

systemctl daemon-reload
	if (( START_OWN_X )); then
		systemctl enable nervus-x11.service nervus-window-manager.service >/dev/null 2>&1
	fi
systemctl enable nervud.service >/dev/null 2>&1
ok "enabled at boot"

# ================================================================= start
step "Starting"
if (( START_OWN_X )); then
	# reset-failed: the previous install may have pushed it into failed (say the old display manager still held the display).
	# systemd refuses restart on a unit that's failed and at its start rate limit
	systemctl reset-failed nervus-x11.service >/dev/null 2>&1 || true
	rm -f /var/log/nervus-x11.log
	systemctl restart nervus-x11.service || warn "nervus-x11 failed to start; see journalctl -u nervus-x11"

	# [Wait for the X socket to actually appear before going on].
	#
	# The unit is Type=simple, so systemd reports active right after fork, when Xorg may still be waiting for
	# the GPU driver or about to exit. Restarting nervud without waiting would start the desktop on a display
	# that doesn't exist yet, it would fail, and the installer would report that all is well.
	x_ready=0
	for _ in $(seq 40); do
		[[ -S /tmp/.X11-unix/X0 ]] && { x_ready=1; break; }
		sleep 0.5
	done
	if (( x_ready )); then
		ok "X display ready (/tmp/.X11-unix/X0)"
	else
		warn "the X display still isn't up after 20 seconds"
		info "${c_dim}after installing, run sudo ./scripts/fix-display.sh; it prints Xorg's real error${c_reset}"
	fi

	systemctl reset-failed nervus-window-manager.service >/dev/null 2>&1 || true
	systemctl restart nervus-window-manager.service \
		|| warn "the window manager failed to start; see journalctl -u nervus-window-manager"
	for _ in $(seq 20); do
		DISPLAY=:0 XAUTHORITY="$XAUTH_FILE" wmctrl -m >/dev/null 2>&1 && break
		sleep 0.25
	done
	if DISPLAY=:0 XAUTHORITY="$XAUTH_FILE" wmctrl -m 2>/dev/null | grep -q 'Name: Openbox'; then
		ok "Openbox has taken over windows (single foreground mode)"
	else
		warn "Openbox isn't responding; apps can show, but one-window mode and the navigation bar may not work"
	fi
	fi

# The self-check must look at the logs [since this nervud start].
#
# It used to hard-code --since '-2min': if nervud was already running from earlier (a reinstall, or restart not really restarting
# while waiting on a dependency), the load messages fell outside the window and the check reported a false "not seen in the log".
NERVUD_SINCE="$(date '+%Y-%m-%d %H:%M:%S')"
systemctl restart nervud.service
sleep 3

if systemctl is-active --quiet nervud.service; then
	ok "nervud is running"
else
	warn "nervud failed to start"
	journalctl -u nervud -n 30 --no-pager | sed 's/^/    /'
	die "installed, but nervud won't start; check the log above first"
fi

# ================================================================= self-check
step "Post-install self-check"

fail=0
nervud_log() { journalctl -u nervud --since "$NERVUD_SINCE" --no-pager 2>/dev/null; }

check() { # check <description> <log regex expected to appear>
	if nervud_log | grep -qE "$2"; then
		ok "$1"
	else
		warn "$1: not seen in the log"
		fail=1
	fi
}

if nervud_log | grep -q 'preflight.*SKIPPED'; then
	warn "preflight was skipped (a development switch; it shouldn't appear in production)"
fi

check "system packages loaded" 'loaded system package'

if nervud_log | grep -q 'trust=platform'; then
	ok "trust levels in effect (trust=platform)"
else
	warn "every package is trust=ordinary: the trust root wasn't verified"
	info "${c_dim}on a production image this means trust levels are broken for the whole machine${c_reset}"
	fail=1
fi

# Report partial downgrades too. Seen on real hardware: system services signed platform-release were platform,
# while the three system apps signed platform-systemapp were all ordinary; only checking "is there any platform"
# misses that. The warn for the downgrade carries the err, the only clue to the root cause
if nervud_log | grep -q 'signature not verified'; then
	warn "some system packages failed signature verification and were downgraded to ordinary:"
	nervud_log | grep 'signature not verified' | sed 's/^/      /'
	fail=1
fi

echo
info "installed packages:"
nervusctl list 2>/dev/null | sed 's/^/      /' || warn "nervusctl list failed"

echo
info "component units:"
systemctl list-units 'nervus-*' --no-legend --no-pager 2>/dev/null | sed 's/^/      /' || true

echo
if systemctl is-active --quiet 'nervus-nervus.launcher-launcherd.service' 2>/dev/null; then
	ok "launcherd is running"
	sleep 3
	if systemctl is-active --quiet 'nervus-nervus.launcher-desktop.service' 2>/dev/null; then
		ok "desktop started"
	else
		warn "desktop didn't start"
		info "look here: journalctl -u nervus-nervus.launcher-desktop.service -n 50"
		info "${c_dim}three common causes: can't reach X11 / JRE lacks java.desktop / wrong skiko architecture${c_reset}"
		fail=1
	fi
	if systemctl is-active --quiet 'nervus-nervus.launcher-navigation.service' 2>/dev/null; then
		ok "navigation started"
	else
		warn "navigation didn't start (the reserved strip on the right will be black)"
		info "look here: journalctl -u nervus-nervus.launcher-navigation.service -n 50"
		fail=1
	fi
else
	info "launcher (nervus.launcher) not installed; skipping the desktop check"
fi

echo
if (( fail )); then
	printf '%sInstall finished, with the warnings above%s\n' "$c_bold$c_yellow" "$c_reset"
else
	printf '%sInstall finished; everything looks good%s\n' "$c_bold$c_green" "$c_reset"
fi

cat <<EOF

  Common commands
    nervusctl list                             list installed packages
    journalctl -u nervud -f                    follow the kernel log
    journalctl -u 'nervus-<pkg>-<comp>' -n 50  one component's log
    systemctl list-units 'nervus-*'            all components
EOF
[[ -x /usr/local/sbin/nervus-restore-desktop ]] && \
	printf '    sudo nervus-restore-desktop                restore the original desktop environment\n'
echo
