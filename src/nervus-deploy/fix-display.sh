#!/usr/bin/env bash
# Repairs a machine that [already has Nervus OS installed but shows a black screen].
#
#   sudo ./fix-display.sh
#
# It doesn't touch packages or the kernel and needs no rebuild; it only rewrites the display session layer:
#   /etc/X11/Xwrapper.config      allow non-console callers
#   /usr/libexec/nervus/start-x11 use vt7, bypass Xorg.wrap, write the X log to a fixed place
#   /etc/systemd/system/nervus-x11.service  add crash rate limiting
#   /etc/nervus/openbox/rc.xml              one window at a time + the system bar on the right
#   nervus-window-manager.service           take over windows before nervud
#
# Diagnose first, then act: if it still won't start afterwards, it prints Xorg's own error lines.
#
# ⚠️ The six heredocs below (Xwrapper.config / start-x11 / nervus-x11.service /
#    Openbox rc.xml / start-window-manager / window-manager unit) [must match the
#    corresponding content in install-nervus.sh word for word], comments aside.
#    They drifted once, and the symptom was "black screen after install, but running this script fixes it",
#    while both scripts looked right, because the difference wasn't what was written but the order things happened in.
#    After changing either side, check with ./check-display-sync.sh.
set -euo pipefail

c_reset=$'\033[0m'; c_bold=$'\033[1m'; c_dim=$'\033[2m'
c_red=$'\033[31m'; c_green=$'\033[32m'; c_yellow=$'\033[33m'; c_blue=$'\033[36m'

step() { printf '\n%s==> %s%s\n' "$c_bold$c_blue" "$*" "$c_reset"; }
info() { printf '    %s\n' "$*"; }
ok()   { printf '    %s✓%s %s\n' "$c_green" "$c_reset" "$*"; }
warn() { printf '    %s!%s %s\n' "$c_yellow" "$c_reset" "$*" >&2; }
die()  { printf '\n%s✗ %s%s\n' "$c_red$c_bold" "$*" "$c_reset" >&2; exit 1; }

[[ $EUID -eq 0 ]] || die "must run with sudo: sudo $0"

XLOG=/var/log/nervus-x11.log

# ================================================================= diagnose
step "① Current state"

info "nervus-x11 state: $(systemctl is-active nervus-x11.service 2>/dev/null || echo not installed)"
info "window manager state: $(systemctl is-active nervus-window-manager.service 2>/dev/null || echo not installed)"
info "nervud     state: $(systemctl is-active nervud.service 2>/dev/null || echo not installed)"
info "default target: $(systemctl get-default 2>/dev/null || echo unknown)"

if compgen -G '/dev/dri/card*' >/dev/null; then
	ok "KMS devices: $(echo /dev/dri/card* | tr ' ' ',')"
	HAS_DRI=1
else
	warn "no card device under /dev/dri: the kernel's GPU driver isn't loaded"
	info "${c_dim}In this state Xorg is bound to fail with no screens found; changing the vt won't help${c_reset}"
	HAS_DRI=0
fi
if compgen -G '/dev/dri/renderD*' >/dev/null; then
	ok "DRM render node: $(echo /dev/dri/renderD* | tr ' ' ',')"
else
	warn "no renderD* under /dev/dri: OpenGL can't use a DRM render node"
fi
for drm_node in /sys/class/drm/card* /sys/class/drm/renderD*; do
	[[ -e "$drm_node" ]] || continue
	driver_path="$(readlink -f "$drm_node/device/driver" 2>/dev/null || true)"
	[[ -n "$driver_path" ]] && info "$(basename "$drm_node") driver: $(basename "$driver_path")"
done

XBIN=""
for c in /usr/lib/xorg/Xorg /usr/bin/Xorg /usr/bin/X; do
	[[ -x "$c" ]] && { XBIN="$c"; break; }
done
if [[ -n "$XBIN" ]]; then
	ok "X binary: $XBIN"
else
	warn "Xorg not found. Install it first: apt-get install -y xserver-xorg-core xserver-xorg-video-all xserver-xorg-input-all"
fi
for tool in openbox wmctrl xdotool; do
	command -v "$tool" >/dev/null 2>&1 \
		|| warn "$tool is missing. Install it first: apt-get install -y openbox wmctrl xdotool"
done
command -v xrandr >/dev/null 2>&1 \
	|| warn "xrandr is missing. Install it first: apt-get install -y x11-xserver-utils"
command -v glxinfo >/dev/null 2>&1 \
	|| warn "glxinfo is missing. Install it first: apt-get install -y mesa-utils"

# When the kernel injects DISPLAY/XAUTHORITY into app components, it takes them from nervud's own environment
# (displayEnv in service/supervise.go). Without them, components carry no credentials and
# fail with "Authorization required, but no authorization protocol specified"
NERVUD_ENV="$(systemctl show nervud.service -p Environment --value 2>/dev/null || true)"
if [[ "$NERVUD_ENV" == *DISPLAY=* ]]; then
	ok "nervud environment: $NERVUD_ENV"
	[[ "$NERVUD_ENV" == *XAUTHORITY=* ]] || warn "no XAUTHORITY in nervud's environment: components can't get the X cookie"
else
	warn "no DISPLAY in nervud's environment; check /etc/systemd/system/nervud.service.d/10-display.conf"
fi

echo
info "recent nervus-x11 log:"
journalctl -u nervus-x11 -n 15 --no-pager 2>/dev/null | sed 's/^/      /' || info "      (none)"

# ================================================================= act
step "② Replace the display session configuration"

# On Debian/Ubuntu, /usr/bin/Xorg is often Xorg.wrap (xserver-xorg-legacy).
# It checks callers against allowed_users, which defaults to console, and processes started by systemd have no console,
# so it exits with "Only console users are allowed to run the X server"
install -D -m 0644 /dev/stdin /etc/X11/Xwrapper.config <<-'EOF'
	# Written by fix-display.sh: X is started as root by nervus-x11.service,
	# outside any login session, so it can't be limited to console users
	allowed_users=anybody
	needs_root_rights=yes
EOF
ok "/etc/X11/Xwrapper.config"

install -D -m 0755 /dev/stdin /usr/libexec/nervus/start-x11 <<-'EOF'
	#!/usr/bin/env bash
	# Start Xorg and prepare an auth cookie every app UID can read.
	set -euo pipefail
	# Not in /run/nervus: that's nervud's RuntimeDirectory, which systemd deletes as soon as nervud stops
	XAUTH=/run/nervus-x11/xauth
	XLOG=/var/log/nervus-x11.log

	install -d -m 0755 /run/nervus-x11
	rm -f "$XAUTH"; touch "$XAUTH"; chmod 0644 "$XAUTH"
	COOKIE="$(mcookie 2>/dev/null || openssl rand -hex 16)"
	xauth -f "$XAUTH" add :0 MIT-MAGIC-COOKIE-1 "$COOKIE"

	# Find the real binary directly, bypassing the /usr/bin/Xorg setuid wrapper
	XBIN=""
	for c in /usr/lib/xorg/Xorg /usr/bin/Xorg /usr/bin/X; do
		[[ -x "$c" ]] && { XBIN="$c"; break; }
	done
	[[ -n "$XBIN" ]] || { echo "Can't find the Xorg executable; it needs xserver-xorg-core" >&2; exit 78; }

	# Wait up to 30 seconds for a KMS device. At boot the DRM driver is often a module udev loads a little later,
	# and starting Xorg before it means an immediate exit with "no screens found"
	for _ in $(seq 60); do
		compgen -G '/dev/dri/card*' >/dev/null && break
		sleep 0.5
	done
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
	# [-ac: drop access control for local clients] If any link of the cookie chain (nervud has XAUTHORITY → the kernel binds
	# the cookie file into the namespace → the component reads it) breaks, the symptom is always the same
	# "Authorization required, but no authorization protocol specified".
	# -nolisten tcp already turns off network listening, so only local processes can connect, and the cookie is
	# 0644 and readable machine-wide anyway, so this is as secure as xhost +local:.
	exec "$XBIN" :0 vt7 -auth "$XAUTH" -nolisten tcp -noreset -ac -logfile "$XLOG"
EOF
ok "/usr/libexec/nervus/start-x11 (vt7)"

install -D -m 0644 /dev/stdin /etc/systemd/system/nervus-x11.service <<-'EOF'
	[Unit]
	Description=Nervus X11 display server
	Before=nervus-window-manager.service nervud.service
	After=systemd-user-sessions.service

	# No start rate limit: for a device that should show a desktop from boot, giving up means a permanently black screen.
	# The reasons it fails to start at boot (driver not loaded yet, an old display manager still holding DRM) are nearly always temporary.
	# The worry about "a crash loop grabbing the VT and pinning the screen" is solved by vt7 (the console stays on vt1)
	StartLimitIntervalSec=0

	[Service]
	Type=simple
	ExecStart=/usr/libexec/nervus/start-x11
	Restart=always
	RestartSec=5s
	# Its own /run directory, independent of nervud's RuntimeDirectory=nervus
	RuntimeDirectory=nervus-x11
	RuntimeDirectoryMode=0755
	StandardOutput=journal
	StandardError=journal
	SyslogIdentifier=nervus-x11

	[Install]
	WantedBy=multi-user.target
EOF
ok "nervus-x11.service"

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
ok "nervus-window-manager.service"

# When the kernel injects DISPLAY/XAUTHORITY into type=app components it uses nervud's own environment.
# The same drop-in also makes sure the window manager is ready before any app.
install -D -m 0644 /dev/stdin /etc/systemd/system/nervud.service.d/10-display.conf <<-'EOF'
	[Unit]
	After=nervus-x11.service nervus-window-manager.service
	Wants=nervus-x11.service nervus-window-manager.service

	[Service]
	Environment=DISPLAY=:0
	Environment=XAUTHORITY=/run/nervus-x11/xauth
EOF
ok "nervud display environment + window manager dependency"

systemctl daemon-reload
systemctl enable nervus-x11.service nervus-window-manager.service >/dev/null 2>&1 || true

# ================================================================= restart
step "③ Restart the display session"

systemctl reset-failed nervus-x11.service 2>/dev/null || true
rm -f "$XLOG"
systemctl restart nervus-x11.service || true
sleep 3

if systemctl is-active --quiet nervus-x11.service; then
	ok "nervus-x11 running"
	X_OK=1
else
	warn "nervus-x11 still isn't up ($(systemctl is-active nervus-x11.service 2>/dev/null))"
	X_OK=0
fi

systemctl reset-failed nervus-window-manager.service 2>/dev/null || true
systemctl restart nervus-window-manager.service || true
sleep 1
if DISPLAY=:0 XAUTHORITY=/run/nervus-x11/xauth wmctrl -m 2>/dev/null | grep -q 'Name: Openbox'; then
	ok "Openbox has taken over windows (single foreground mode)"
	WM_OK=1
else
	warn "Openbox isn't responding"
	WM_OK=0
fi

echo
info "${c_bold}Current X11 resolution and renderer:${c_reset}"
XRANDR_OUTPUT="$(DISPLAY=:0 XAUTHORITY=/run/nervus-x11/xauth xrandr --current 2>&1 || true)"
if grep -q '^Screen ' <<<"$XRANDR_OUTPUT"; then
	info "$(grep -m1 '^Screen ' <<<"$XRANDR_OUTPUT")"
	while IFS= read -r line; do
		info "output: $line"
	done < <(
		awk '
			/ connected/ {
				output=$1
				geometry=""
				for (i=2; i<=NF; i++) {
					if ($i ~ /^[0-9]+x[0-9]+\+[0-9]+\+[0-9]+/) {
						geometry=$i
						break
					}
				}
				print output " " (geometry == "" ? "connected but inactive" : geometry)
			}
		' <<<"$XRANDR_OUTPUT"
	)
else
	warn "xrandr can't read :0: $(tail -1 <<<"$XRANDR_OUTPUT")"
fi

GLX_OUTPUT="$(DISPLAY=:0 XAUTHORITY=/run/nervus-x11/xauth glxinfo -B 2>&1 || true)"
GL_RENDERER="$(sed -n 's/^[[:space:]]*OpenGL renderer string:[[:space:]]*//p' <<<"$GLX_OUTPUT" | head -1)"
GL_DIRECT="$(sed -n 's/^[[:space:]]*direct rendering:[[:space:]]*//p' <<<"$GLX_OUTPUT" | head -1)"
if [[ -n "$GL_RENDERER" ]]; then
	info "GLX direct rendering: ${GL_DIRECT:-unknown}"
	if [[ "${GL_RENDERER,,}" =~ llvmpipe|softpipe|swrast ]]; then
		warn "OpenGL renderer: $GL_RENDERER (software rendering; the GPU isn't being used)"
	else
		ok "OpenGL renderer: $GL_RENDERER"
	fi
else
	warn "glxinfo can't create an OpenGL context: $(tail -1 <<<"$GLX_OUTPUT")"
fi

if [[ -S /tmp/.X11-unix/X0 ]]; then
	ok "X socket ready: /tmp/.X11-unix/X0"
else
	warn "no /tmp/.X11-unix/X0: the X server didn't really start"
	X_OK=0
fi

# ================================================================= bring up the desktop
step "④ Restart nervud and wait for the desktop"

NERVUD_SINCE="$(date '+%Y-%m-%d %H:%M:%S')"
systemctl restart nervud.service
# launcherd tries LaunchComponent every 5 s; give it two rounds
sleep 12

if systemctl is-active --quiet 'nervus-nervus.launcher-desktop.service' 2>/dev/null; then
	ok "desktop component running"
	DESK_OK=1
else
	warn "desktop component didn't start"
	DESK_OK=0
fi

if systemctl is-active --quiet 'nervus-nervus.launcher-navigation.service' 2>/dev/null; then
	ok "navigation component running"
	NAV_OK=1
else
	warn "navigation component didn't start: the reserved strip on the right of the screen will be black"
	NAV_OK=0
fi

for component in desktop navigation; do
	RENDER_LINE="$(
		journalctl -u "nervus-nervus.launcher-${component}.service" \
			--since "$NERVUD_SINCE" --no-pager 2>/dev/null \
			| grep 'renderer=' | tail -1 || true
	)"
	if [[ -n "$RENDER_LINE" ]]; then
		info "$component Skiko: ${RENDER_LINE##*renderer=}"
	else
		warn "no Skiko renderer in the $component log yet"
	fi
done

# ================================================================= verdict
step "⑤ Verdict"

if (( X_OK && WM_OK && DESK_OK && NAV_OK )); then
	printf '    %s%sThe display session is back; the desktop should be on screen%s\n' "$c_bold" "$c_green" "$c_reset"
	info "${c_dim}If the screen is still black, press Ctrl+Alt+F7 to switch to the VT X is on${c_reset}"
	exit 0
fi

printf '    %s%sNot fixed yet; here is the real error%s\n' "$c_bold" "$c_yellow" "$c_reset"

if (( ! X_OK )); then
	echo
	info "${c_bold}Xorg error lines:${c_reset}"
	if [[ -f "$XLOG" ]]; then
		grep -E '\(EE\)|\(WW\).*no screens|Fatal' "$XLOG" | tail -20 | sed 's/^/      /' \
			|| tail -20 "$XLOG" | sed 's/^/      /'
	else
		info "      ($XLOG doesn't exist, so Xorg never even started; see the journal below)"
	fi
	echo
	info "${c_bold}journal:${c_reset}"
	journalctl -u nervus-x11 --since "$NERVUD_SINCE" --no-pager 2>/dev/null | tail -20 | sed 's/^/      /'

	echo
	info "${c_bold}What to do:${c_reset}"
	if (( ! HAS_DRI )); then
		info "  · /dev/dri is empty: the board's GPU/HDMI driver isn't loaded. That's a problem outside X:"
		info "    first check HDMI has a signal and the kernel's DRM module is loaded (lsmod | grep -i drm)."
		info "    Vendor boards (RDK, Jetson and the like) usually need the vendor's own display driver package."
	fi
	info "  · no screens found: install a driver other than modesetting"
	info "        apt-get install -y xserver-xorg-video-fbdev xserver-xorg-video-modesetting"
	info "  · Cannot open /dev/tty7: use another vt (change vt7 on the last line of start-x11)"
	info "  · Just want to see the desktop and don't care about the local screen: ssh -X in from another machine"
	info "    and point nervud's DISPLAY there (/etc/systemd/system/nervud.service.d/10-display.conf)"
fi

if (( X_OK && ! DESK_OK )); then
	echo
	info "${c_bold}X is fine; the problem is the desktop process:${c_reset}"
	journalctl -u 'nervus-nervus.launcher-desktop.service' -n 40 --no-pager 2>/dev/null | sed 's/^/      /'
	echo
	info "  · UnsatisfiedLinkError / skiko: the package's skiko doesn't match the board; -Pnervus.abi was wrong at build time"
	info "  · NoClassDefFoundError: java/awt: the JRE is a -headless build without java.desktop"
	info "  · Can't connect to X11 window server: DISPLAY/XAUTHORITY didn't reach the sandbox;"
	info "    see systemctl show nervus-nervus.launcher-desktop.service -p Environment"
fi

if (( X_OK && ! NAV_OK )); then
	echo
	info "${c_bold}X is fine; the problem is the navigation process:${c_reset}"
	journalctl -u 'nervus-nervus.launcher-navigation.service' -n 40 --no-pager 2>/dev/null | sed 's/^/      /'
fi

exit 1
