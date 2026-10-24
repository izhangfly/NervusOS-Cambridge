#!/usr/bin/env bash
# Remove NervusOS hosted mode from this Ubuntu machine.
#
#   sudo hosted/uninstall-hosted.sh          remove hosted mode, keep the NervusOS core
#   sudo hosted/uninstall-hosted.sh --all    also run the upstream uninstaller
#
# Ubuntu itself was never modified, so there's nothing to restore there.
set -euo pipefail
REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
[[ $EUID -eq 0 ]] || { echo "run with sudo" >&2; exit 1; }
DESKTOP_USER="${NERVUS_USER:-${SUDO_USER:-}}"

if [[ -n "$DESKTOP_USER" ]]; then
	sudo -u "$DESKTOP_USER" XDG_RUNTIME_DIR="/run/user/$(id -u "$DESKTOP_USER")" /usr/local/bin/nervus-session stop 2>/dev/null || true
fi
systemctl stop nervud.service nervus-piper.service 2>/dev/null || true
systemctl disable --now nervus-can.service 2>/dev/null || true

rm -f /etc/systemd/system/nervus-piper.service /etc/systemd/system/nervus-can.service \
	/etc/systemd/system/nervud.service.d/10-display.conf \
	/etc/udev/rules.d/90-nervus-can.rules /etc/tmpfiles.d/nervus-hosted.conf \
	/etc/polkit-1/rules.d/50-nervus-hosted.rules /etc/nervus/hosted /etc/nervus/openbox/rc.xml \
	/usr/libexec/nervus/hosted-power /usr/libexec/nervus/can-up /usr/local/bin/nervus-session \
	/usr/share/applications/nervusos.desktop /usr/share/applications/nervusos-quit.desktop \
	/usr/local/bin/nervus-open /etc/sudoers.d/50-nervus-open \
	/usr/share/applications/nervus-handteleop.desktop /usr/share/applications/nervus-llmarm.desktop \
	/usr/share/applications/nervus-act.desktop /usr/local/bin/nervus-standalone \
	/usr/share/icons/hicolor/scalable/apps/nervusos.svg
rm -rf /usr/share/nervus/store /opt/nervus/piper /opt/nervus/apps /run/nervus-x11
id nervus-piper >/dev/null 2>&1 && userdel nervus-piper 2>/dev/null || true
systemctl daemon-reload
udevadm control --reload 2>/dev/null || true
echo "hosted mode removed"

if [[ "${1:-}" == "--all" ]]; then
	ASSUME_YES=1 setsid -w bash "$REPO/src/nervus-deploy/uninstall-nervus.sh" </dev/null
fi
