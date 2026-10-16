#!/usr/bin/env bash
# Removes Nervus OS from this machine completely.
#
#   sudo ./uninstall-nervus.sh
set -euo pipefail

c_reset=$'\033[0m'; c_bold=$'\033[1m'; c_dim=$'\033[2m'
c_red=$'\033[31m'; c_green=$'\033[32m'; c_yellow=$'\033[33m'; c_blue=$'\033[36m'

step() { printf '\n%s==> %s%s\n' "$c_bold$c_blue" "$*" "$c_reset"; }
info() { printf '    %s\n' "$*"; }
ok()   { printf '    %s✓%s %s\n' "$c_green" "$c_reset" "$*"; }
warn() { printf '    %s!%s %s\n' "$c_yellow" "$c_reset" "$*" >&2; }
die()  { printf '\n%s✗ %s%s\n' "$c_red$c_bold" "$*" "$c_reset" >&2; exit 1; }

ASSUME_YES="${ASSUME_YES:-0}"

confirm() {
	local prompt="$1" default="${2:-y}" reply
	[[ "$ASSUME_YES" == 1 ]] && { [[ "$default" == y ]]; return; }
	if [[ -r /dev/tty ]]; then
		read -r -p "    $prompt [$( [[ $default == y ]] && echo 'Y/n' || echo 'y/N' )]: " reply </dev/tty
	else
		warn "no interactive terminal; using the default answer: $default"
		reply="$default"
	fi
	reply="${reply:-$default}"
	[[ "$reply" =~ ^[Yy] ]]
}

remove_path() {
	local p="$1"
	if [[ -e "$p" || -L "$p" ]]; then
		rm -rf -- "$p"
		ok "removed $p"
	else
		info "not present, skipped $p"
	fi
}

[[ $EUID -eq 0 ]] || die "must run with sudo: sudo $0"
[[ $# -eq 0 ]] || die "takes no arguments; just run: sudo $0"

step "Confirm full removal"
cat <<'EOF'
    This stops the Nervus services and deletes the Nervus files written by install-nervus.sh / fix-display.sh:
      /usr/libexec/nervus
      /usr/bin/nervusctl
      /usr/lib/nervus
      /usr/share/nervus
      /var/lib/nervus
      /etc/nervus
      /etc/systemd/system/nervud.service*
      /etc/systemd/system/nervus-x11.service
      /etc/systemd/system/nervus-window-manager.service
      /etc/systemd/journald.conf.d/10-nervus.conf
      /usr/local/sbin/nervus-restore-desktop
      /run/nervus
      /run/nervus-x11
EOF
confirm "Continue with removal?" y || die "aborted"

step "Restore the desktop takeover"
if [[ -x /usr/local/sbin/nervus-restore-desktop ]]; then
	/usr/local/sbin/nervus-restore-desktop || warn "the desktop restore script failed; continuing to remove Nervus"
else
	info "no nervus-restore-desktop, skipped"
fi

step "Stop and disable the Nervus services"
if command -v systemctl >/dev/null 2>&1; then
	for u in nervud.service nervus-window-manager.service nervus-x11.service; do
		systemctl disable --now "$u" >/dev/null 2>&1 || true
	done
	for u in $(systemctl list-units --all --plain --no-legend 'nervus-*' 2>/dev/null | awk '{print $1}'); do
		systemctl stop "$u" 2>/dev/null || true
	done
	systemctl reset-failed nervud.service nervus-window-manager.service nervus-x11.service >/dev/null 2>&1 || true
	ok "Nervus services stopped"
else
	warn "systemctl not found, skipping service stop"
fi

step "Remove systemd configuration"
remove_path /etc/systemd/system/nervud.service.d
remove_path /etc/systemd/system/nervud.service
remove_path /etc/systemd/system/nervus-x11.service
remove_path /etc/systemd/system/nervus-window-manager.service
# The journald drop-in is ours, so it goes too.
#
# [/var/log/journal is not deleted]: it holds this machine's entire system log, not just Nervus's.
# Uninstalling one application shouldn't wipe the evidence ops rely on. To clear it, systemd has
# journalctl --vacuum-size.
remove_path /etc/systemd/journald.conf.d/10-nervus.conf
if command -v systemctl >/dev/null 2>&1; then
	systemctl restart systemd-journald >/dev/null 2>&1 || true
	systemctl daemon-reload
	systemctl reset-failed nervud.service nervus-window-manager.service nervus-x11.service >/dev/null 2>&1 || true
	ok "systemd reloaded"
fi

step "Remove Nervus files and data"
remove_path /usr/libexec/nervus
remove_path /usr/bin/nervusctl
remove_path /usr/lib/nervus
remove_path /usr/share/nervus
remove_path /var/lib/nervus
remove_path /etc/nervus
remove_path /usr/local/sbin/nervus-restore-desktop
remove_path /var/log/nervus-x11.log
remove_path /run/nervus
remove_path /run/nervus-x11

step "Remove the Xwrapper configuration"
if [[ -f /etc/X11/Xwrapper.config ]]; then
	if grep -qE 'Written by (install-nervus|fix-display)\.sh' /etc/X11/Xwrapper.config 2>/dev/null; then
		remove_path /etc/X11/Xwrapper.config
	else
		warn "keeping /etc/X11/Xwrapper.config: it has no Nervus marker"
	fi
else
	info "not present, skipped /etc/X11/Xwrapper.config"
fi

echo
printf '%sRemoval complete%s\n' "$c_bold$c_green" "$c_reset"
echo
