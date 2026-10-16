#!/usr/bin/env bash
# Checks that install-nervus.sh and fix-display.sh write the same display configuration.
#
#   ./check-display-sync.sh
#
# # Why this exists
#
# Both scripts embed the same six files (Xwrapper.config / Xorg launcher and unit /
# Openbox config, window-manager launcher and unit). They drifted once on real hardware: install-nervus.sh missed a fix,
# so "after install the screen is black, but running fix-display.sh by hand fixes it". Each script looked right on its own;
# the difference hid in the embedded content, and no one spots that by eye across dozens of heredoc lines.
#
# Only executable content is compared, ignoring comments and blank lines: the two copies are commented differently
# (the installer's is more thorough, since that's the one people actually read).
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")"

c_reset=$'\033[0m'; c_red=$'\033[31m'; c_green=$'\033[32m'

# block <file> <target path>
# Extracts the content between `install -D ... <target path> <<-'EOF'` and EOF, strips the leading TABs that <<-
# would strip, then drops comments and blank lines.
#
# [It must anchor on install -D]: matching the path alone also hits the same path mentioned in the header comment,
# and then extracts from the comment all the way into the script body. The first version did exactly that
block() {
	awk -v path="$2" '
		index($0, "install -D") && index($0, path) && index($0, "<<-") { f = 1; next }
		f && /^\t*EOF$/ { exit }
		f
	' "$1" \
		| sed 's/^\t*//' \
		| grep -v '^\s*#' \
		| grep -v '^\s*$'
}

fail=0
check() { # check <name> <target path>
	local name="$1" pat="$2" a b
	a="$(block install-nervus.sh "$pat")"
	b="$(block fix-display.sh "$pat")"
	if [[ -z "$a" || -z "$b" ]]; then
		printf '  %s✗%s %s: nothing extracted from one of the scripts (did the heredoc style change?)\n' "$c_red" "$c_reset" "$name"
		fail=1
		return
	fi
	if [[ "$a" == "$b" ]]; then
		printf '  %s✓%s %s\n' "$c_green" "$c_reset" "$name"
	else
		printf '  %s✗%s %s differs:\n' "$c_red" "$c_reset" "$name"
		diff <(printf '%s\n' "$a") <(printf '%s\n' "$b") | sed 's/^/      /'
		fail=1
	fi
}

echo "Comparing the display configuration embedded in install-nervus.sh and fix-display.sh:"
check "Xwrapper.config"     '/etc/X11/Xwrapper.config'
check "start-x11"           '/usr/libexec/nervus/start-x11'
check "nervus-x11.service"  '/etc/systemd/system/nervus-x11.service'
check "Openbox rc.xml"       '/etc/nervus/openbox/rc.xml'
check "start-window-manager" '/usr/libexec/nervus/start-window-manager'
check "window-manager unit"  '/etc/systemd/system/nervus-window-manager.service'

echo
if (( fail )); then
	printf '%sThe two scripts would write different display configurations. Fix that before releasing.%s\n' "$c_red" "$c_reset" >&2
	exit 1
fi
printf '%sIn sync.%s\n' "$c_green" "$c_reset"
