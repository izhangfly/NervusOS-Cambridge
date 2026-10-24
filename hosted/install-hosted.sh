#!/usr/bin/env bash
# Install NervusOS on an Ubuntu desktop in hosted mode.
#
#   sudo hosted/install-hosted.sh [release.tar.gz]
#
# Hosted mode means NervusOS is something you open and quit, like an app:
#   * Ubuntu, GDM and the Ubuntu desktop are left exactly as they are.
#   * nervud (the NervusOS kernel) is installed as a system service but is NOT
#     started at boot. "NervusOS" in the app grid starts it on a fullscreen
#     display of its own; "Quit NervusOS", Ctrl+Alt+Q or the NervusOS power
#     button stops it again.
#   * The PiPER arm driver (hardware layer) starts and stops with NervusOS.
#
# Run scripts/build.sh first. Safe to re-run; it updates everything in place.
set -euo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
HOSTED="$REPO/hosted"
TARBALL="${1:-$REPO/out/nervus-os-linux-x86_64.tar.gz}"
STORE_SRC="$REPO/out/store"

step() { printf '\n\033[1;36m==> %s\033[0m\n' "$*"; }
ok() { printf '    \033[32mok\033[0m %s\n' "$*"; }
warn() { printf '    \033[33m!\033[0m %s\n' "$*" >&2; }
die() { printf '\n\033[1;31minstall-hosted: %s\033[0m\n' "$*" >&2; exit 1; }

[[ $EUID -eq 0 ]] || die "run with sudo: sudo $0 $*"
DESKTOP_USER="${NERVUS_USER:-${SUDO_USER:-}}"
[[ -n "$DESKTOP_USER" && "$DESKTOP_USER" != root ]] || die "run it with sudo from the desktop user's account (or set NERVUS_USER)"
id "$DESKTOP_USER" >/dev/null 2>&1 || die "no such user: $DESKTOP_USER"
DESKTOP_UID="$(id -u "$DESKTOP_USER")"
DESKTOP_HOME="$(getent passwd "$DESKTOP_USER" | cut -d: -f6)"
[[ -f "$TARBALL" ]] || die "release not found: $TARBALL (run scripts/build.sh)"
# Installing stops NervusOS. If it was open, it is opened again at the end, so a reinstall over SSH doesn't
# leave the desktop closed.
WAS_RUNNING=0
systemctl is-active --quiet nervud.service && WAS_RUNNING=1

# ------------------------------------------------------------------ packages
step "Packages"
PKGS=(xserver-xephyr openbox wmctrl xdotool xauth x11-xserver-utils x11-utils mesa-utils
      can-utils iproute2 python3-venv libnotify-bin v4l-utils ffmpeg)
missing=()
for p in "${PKGS[@]}"; do dpkg -s "$p" >/dev/null 2>&1 || missing+=("$p"); done
if (( ${#missing[@]} )); then
	DEBIAN_FRONTEND=noninteractive apt-get install -y -qq "${missing[@]}" >/dev/null
	ok "installed ${missing[*]}"
else
	ok "all present"
fi

# ------------------------------------------------------------------ core
step "NervusOS core (upstream installer, desktop left alone)"
# ASSUME_YES=1 takes every default. The one that matters is "replace the existing
# desktop environment?", whose default is NO: GDM stays enabled and the default
# target stays graphical.target. setsid detaches us from the terminal so the
# script can't prompt either way.
ASSUME_YES=1 setsid -w bash "$REPO/src/nervus-deploy/install-nervus.sh" "$TARBALL" </dev/null \
	>/var/log/nervus-install.log 2>&1 || {
	tail -30 /var/log/nervus-install.log >&2
	die "core install failed; full log in /var/log/nervus-install.log"
}
ok "kernel, system services, system apps, JRE and trust root installed (log: /var/log/nervus-install.log)"
systemctl is-enabled --quiet gdm3.service 2>/dev/null && ok "gdm3 still enabled" || warn "gdm3 is not enabled; check the Ubuntu login screen"
[[ "$(systemctl get-default)" == graphical.target ]] && ok "default target still graphical.target" || warn "default target is $(systemctl get-default)"

# ------------------------------------------------------------------ hosted overlay
step "Hosted mode"
# The upstream installer enabled and started nervud on the existing display (:0).
# In hosted mode nervus-session owns its lifetime instead.
systemctl stop nervud.service 2>/dev/null || true
systemctl disable nervud.service >/dev/null 2>&1 || true
ok "nervud not started at boot"

install -D -m 0644 "$HOSTED/systemd/nervud-hosted.conf" /etc/systemd/system/nervud.service.d/10-display.conf
ok "nervud drop-in: DISPLAY=:7, hosted power hook"

install -D -m 0755 "$HOSTED/libexec/hosted-power" /usr/libexec/nervus/hosted-power
install -D -m 0755 "$HOSTED/bin/nervus-session" /usr/local/bin/nervus-session
install -D -m 0644 "$HOSTED/openbox/rc.xml" /etc/nervus/openbox/rc.xml
install -d -m 0755 /etc/nervus
echo "hosted" >/etc/nervus/hosted
ok "nervus-session, power hook, Openbox layout"

sed "s/@USER@/$DESKTOP_USER/g" "$HOSTED/tmpfiles/nervus-hosted.conf.in" >/etc/tmpfiles.d/nervus-hosted.conf
systemd-tmpfiles --create /etc/tmpfiles.d/nervus-hosted.conf
ok "/run/nervus-x11 owned by $DESKTOP_USER"

sed "s/@USER@/$DESKTOP_USER/g" "$HOSTED/polkit/50-nervus-hosted.rules.in" >/etc/polkit-1/rules.d/50-nervus-hosted.rules
chmod 0644 /etc/polkit-1/rules.d/50-nervus-hosted.rules
ok "polkit: $DESKTOP_USER may start/stop nervud, the arm driver and the ACT engine"

# ------------------------------------------------------------------ hardware layer
step "Hardware layer: PiPER arm on can0"
install -D -m 0755 "$HOSTED/libexec/can-up" /usr/libexec/nervus/can-up
install -D -m 0644 "$HOSTED/udev/90-nervus-can.rules" /etc/udev/rules.d/90-nervus-can.rules
install -D -m 0755 "$HOSTED/libexec/camera-defaults" /usr/libexec/nervus/camera-defaults
install -D -m 0644 "$HOSTED/udev/91-nervus-cameras.rules" /etc/udev/rules.d/91-nervus-cameras.rules
udevadm control --reload 2>/dev/null || true
# Cameras already plugged in get the defaults now; new ones get them from udev.
for v in /dev/v4l/by-id/*-video-index0; do [[ -e "$v" ]] && /usr/libexec/nervus/camera-defaults "$(readlink -f "$v")"; done
ok "camera defaults: mains anti-flicker, fixed frame rate"
install -D -m 0644 "$HOSTED/systemd/nervus-can.service" /etc/systemd/system/nervus-can.service
install -D -m 0644 "$HOSTED/systemd/nervus-piper.service" /etc/systemd/system/nervus-piper.service

id nervus-piper >/dev/null 2>&1 || useradd --system --no-create-home --home-dir /opt/nervus/piper \
	--shell /usr/sbin/nologin nervus-piper
install -d -m 0755 /opt/nervus/piper
install -m 0644 "$REPO/hardware/piper/piper_webctl.py" /opt/nervus/piper/piper_webctl.py
install -m 0644 "$REPO/hardware/piper/panel.html" /opt/nervus/piper/panel.html
install -m 0644 "$REPO/hardware/piper/README.md" /opt/nervus/piper/README.md
if [[ ! -x /opt/nervus/piper/venv/bin/python ]]; then
	python3 -m venv /opt/nervus/piper/venv
fi
/opt/nervus/piper/venv/bin/pip install -q --upgrade pip >/dev/null
/opt/nervus/piper/venv/bin/pip install -q -r "$REPO/hardware/piper/requirements.txt"
ok "driver venv: $(/opt/nervus/piper/venv/bin/python -c 'import piper_sdk,can;print("piper_sdk", getattr(piper_sdk,"__version__","?"), "python-can", can.__version__)')"
systemctl daemon-reload
systemctl enable --now nervus-can.service >/dev/null 2>&1 || warn "nervus-can.service failed (is the CAN adapter plugged in?)"
ip -br link show can0 2>/dev/null | sed 's/^/    /' || warn "can0 not present"

# ------------------------------------------------------------------ ACT engine
step "ACT engine (record, run, post-train)"
install -d -m 0755 /opt/nervus/act
rm -rf /opt/nervus/act/nervus_act
cp -r "$REPO/act/engine/nervus_act" /opt/nervus/act/nervus_act
find /opt/nervus/act/nervus_act -name __pycache__ -prune -exec rm -rf {} +
install -m 0644 "$REPO/act/README.md" /opt/nervus/act/README.md 2>/dev/null || true
if [[ ! -x /opt/nervus/act/venv/bin/python ]]; then
	python3 -m venv /opt/nervus/act/venv
	/opt/nervus/act/venv/bin/pip install -q --upgrade pip >/dev/null
	echo "    installing torch (CPU) and LeRobot: a few minutes the first time"
	/opt/nervus/act/venv/bin/pip install -q --index-url https://download.pytorch.org/whl/cpu torch==2.10.0 torchvision==0.25.0
fi
/opt/nervus/act/venv/bin/pip install -q -r "$REPO/act/engine/requirements.txt"
ok "engine venv: $(/opt/nervus/act/venv/bin/python -c 'import torch,lerobot,cv2;print("torch",torch.__version__,"lerobot",lerobot.__version__,"opencv",cv2.__version__)')"
install -d -o "$DESKTOP_USER" -g "$DESKTOP_USER" -m 0755 "$DESKTOP_HOME/Shared" "$DESKTOP_HOME/Shared/NervusACT"
sed "s/@USER@/$DESKTOP_USER/g; s#@HOME@#$DESKTOP_HOME#g" "$HOSTED/systemd/nervus-act.service.in" >/etc/systemd/system/nervus-act.service
systemctl daemon-reload
ok "nervus-act.service: runs with NervusOS, as $DESKTOP_USER, on 127.0.0.1:8795"

# ------------------------------------------------------------------ LLM Arm
step "LLM Arm service (plain-language arm control)"
install -d -m 0755 /opt/nervus/llm-arm
rm -rf /opt/nervus/llm-arm/nervus_llmarm
cp -r "$REPO/llm-arm/engine/nervus_llmarm" /opt/nervus/llm-arm/nervus_llmarm
find /opt/nervus/llm-arm -name __pycache__ -prune -exec rm -rf {} +
install -m 0644 "$REPO/llm-arm/README.md" /opt/nervus/llm-arm/README.md 2>/dev/null || true
install -d -o "$DESKTOP_USER" -g "$DESKTOP_USER" -m 0700 "$DESKTOP_HOME/.config/nervus"
sed "s/@USER@/$DESKTOP_USER/g; s#@HOME@#$DESKTOP_HOME#g" "$HOSTED/systemd/nervus-llm-arm.service.in" >/etc/systemd/system/nervus-llm-arm.service
systemctl daemon-reload
ok "nervus-llm-arm.service: runs with NervusOS, as $DESKTOP_USER, on 127.0.0.1:8796"
for k in deepseek openai; do
	[[ -f "$DESKTOP_HOME/.config/nervus/$k.key" ]] || warn "no $k key yet: install -m 600 /dev/stdin ~/.config/nervus/$k.key"
done

# ------------------------------------------------------------------ store
step "App Store catalog"
if [[ -f "$STORE_SRC/catalog.ini" ]]; then
	rm -rf /usr/share/nervus/store
	install -d -m 0755 /usr/share/nervus/store/packages
	install -m 0644 "$STORE_SRC/catalog.ini" /usr/share/nervus/store/catalog.ini
	install -m 0644 "$STORE_SRC"/packages/*.nspkg /usr/share/nervus/store/packages/
	ok "$(ls /usr/share/nervus/store/packages | wc -l) packages in /usr/share/nervus/store"
else
	warn "no out/store (run scripts/build.sh apps); the store will be empty"
fi

# ------------------------------------------------------------------ desktop integration
step "Ubuntu desktop integration"
install -D -m 0644 "$HOSTED/icons/nervusos.svg" /usr/share/icons/hicolor/scalable/apps/nervusos.svg
gtk-update-icon-cache -q /usr/share/icons/hicolor 2>/dev/null || true
install -D -m 0644 "$HOSTED/applications/nervusos.desktop" /usr/share/applications/nervusos.desktop
install -D -m 0644 "$HOSTED/applications/nervusos-quit.desktop" /usr/share/applications/nervusos-quit.desktop
# Apps that open straight from the Ubuntu desktop: nervus-open starts NervusOS if needed, then the app.
APP_LAUNCHERS=(nervus-handteleop nervus-llmarm)
install -D -m 0755 "$HOSTED/bin/nervus-open" /usr/local/bin/nervus-open
for a in "${APP_LAUNCHERS[@]}"; do
	install -D -m 0644 "$HOSTED/icons/$a.svg" "/usr/share/icons/hicolor/scalable/apps/$a.svg"
	install -D -m 0644 "$HOSTED/applications/$a.desktop" "/usr/share/applications/$a.desktop"
done
gtk-update-icon-cache -q /usr/share/icons/hicolor 2>/dev/null || true
# nervus-open needs nervud's admin socket (root only) for one thing: starting an installed app.
printf '%s ALL=(root) NOPASSWD: /usr/bin/nervusctl launch *\n' "$DESKTOP_USER" >/etc/sudoers.d/50-nervus-open
chmod 0440 /etc/sudoers.d/50-nervus-open
visudo -cq -f /etc/sudoers.d/50-nervus-open || { rm -f /etc/sudoers.d/50-nervus-open; warn "sudoers rule for nervus-open rejected"; }
update-desktop-database -q /usr/share/applications 2>/dev/null || true
ok "NervusOS and Quit NervusOS in the app grid"

as_user() {
	sudo -u "$DESKTOP_USER" env DBUS_SESSION_BUS_ADDRESS="unix:path=/run/user/$DESKTOP_UID/bus" \
		XDG_RUNTIME_DIR="/run/user/$DESKTOP_UID" HOME="$DESKTOP_HOME" "$@"
}
if [[ -S "/run/user/$DESKTOP_UID/bus" ]]; then
	# Pin NervusOS to the dock
	favs="$(as_user gsettings get org.gnome.shell favorite-apps 2>/dev/null || echo "[]")"
	if [[ "$favs" != *nervusos.desktop* ]]; then
		newfavs="$(python3 -c 'import ast,sys; l=ast.literal_eval(sys.argv[1] if sys.argv[1]!="@as []" else "[]"); l.append("nervusos.desktop"); print(str(l))' "$favs")"
		as_user gsettings set org.gnome.shell favorite-apps "$newfavs" && ok "pinned to the dock"
	fi
	# Ctrl+Alt+N opens NervusOS, Ctrl+Alt+Q quits it (also bound inside NervusOS)
	base=/org/gnome/settings-daemon/plugins/media-keys/custom-keybindings
	list="$(as_user gsettings get org.gnome.settings-daemon.plugins.media-keys custom-keybindings 2>/dev/null || echo "@as []")"
	for kb in "nervusos-open:NervusOS:nervus-session start:<Control><Alt>n" "nervusos-quit:Quit NervusOS:nervus-session stop:<Control><Alt>q"; do
		IFS=: read -r id name cmd key <<<"$kb"
		path="$base/$id/"
		schema="org.gnome.settings-daemon.plugins.media-keys.custom-keybinding:$path"
		as_user gsettings set "$schema" name "$name"
		as_user gsettings set "$schema" command "/usr/local/bin/$cmd"
		as_user gsettings set "$schema" binding "$key"
		if [[ "$list" != *"$path"* ]]; then
			list="$(python3 -c 'import ast,sys; s=sys.argv[1].replace("@as ",""); l=ast.literal_eval(s); l.append(sys.argv[2]); print(str(l))' "$list" "$path")"
		fi
	done
	as_user gsettings set org.gnome.settings-daemon.plugins.media-keys custom-keybindings "$list"
	ok "shortcuts: Ctrl+Alt+N open, Ctrl+Alt+Q quit"
else
	warn "$DESKTOP_USER has no desktop session right now; dock pin and shortcuts skipped (re-run after logging in)"
fi

desk="$(as_user xdg-user-dir DESKTOP 2>/dev/null || echo "$DESKTOP_HOME/Desktop")"
if [[ -d "$desk" ]]; then
	install -o "$DESKTOP_USER" -g "$DESKTOP_USER" -m 0755 "$HOSTED/applications/nervusos.desktop" "$desk/nervusos.desktop"
	as_user gio set "$desk/nervusos.desktop" metadata::trusted true 2>/dev/null || true
	for a in "${APP_LAUNCHERS[@]}"; do
		install -o "$DESKTOP_USER" -g "$DESKTOP_USER" -m 0755 "$HOSTED/applications/$a.desktop" "$desk/$a.desktop"
		as_user gio set "$desk/$a.desktop" metadata::trusted true 2>/dev/null || true
	done
	ok "launcher on the desktop ($desk)"
fi

# ------------------------------------------------------------------ check
step "Check"
/usr/libexec/nervus/nervud --help >/dev/null 2>&1 || true
[[ -x /usr/libexec/nervus/nervud ]] && ok "kernel: /usr/libexec/nervus/nervud"
ls /usr/lib/nervus/system-packages | sed 's/^/    system package: /'
systemctl is-active --quiet nervud.service && warn "nervud is running" || ok "nervud stopped (opens with NervusOS)"
if (( WAS_RUNNING )); then
	if sudo -u "$DESKTOP_USER" XDG_RUNTIME_DIR="/run/user/$DESKTOP_UID" /usr/local/bin/nervus-session start >/dev/null 2>&1; then
		ok "NervusOS was open before the install: opened it again"
	else
		warn "NervusOS was open before the install and did not reopen; run: nervus-session start"
	fi
fi

cat <<EOF

NervusOS is installed in hosted mode.
  Open:   "NervusOS" in the app grid or dock, Ctrl+Alt+N, or: nervus-session start
  Quit:   Ctrl+Alt+Q, "Quit NervusOS", the NervusOS power button, or: nervus-session stop
  Status: nervus-session status
  Logs:   journalctl -u nervud -f      journalctl -u nervus-piper -f      journalctl -u nervus-act -f
  Remove: sudo hosted/uninstall-hosted.sh
EOF
