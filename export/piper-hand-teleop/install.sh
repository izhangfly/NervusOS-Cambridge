#!/usr/bin/env bash
# Install PiPER Hand Teleop as a desktop app on Ubuntu 24.04:  sudo ./install.sh
REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$REPO/scripts/install-common.sh"

install_driver
DEBIAN_FRONTEND=noninteractive apt-get install -y -qq ffmpeg v4l-utils >/dev/null

if [[ "$(cat /sys/class/dmi/id/product_name 2>/dev/null)" == *"Surface Pro 9"* ]]; then
	step "Surface Pro 9 front camera -> /dev/video60 \"Surface Camera\""
	[[ -x /usr/local/bin/cam ]] || warn "libcamera isn't built yet: run surface-camera/build-libcamera.sh as yourself, then re-run this"
	DEBIAN_FRONTEND=noninteractive apt-get install -y -qq v4l2loopback-dkms i2c-tools gstreamer1.0-tools \
		gstreamer1.0-plugins-base gstreamer1.0-plugins-good >/dev/null
	install -D -m 0644 "$REPO/surface-camera/v4l2loopback.conf" /etc/modprobe.d/nervus-v4l2loopback.conf
	printf 'v4l2loopback\ni2c-dev\n' >/etc/modules-load.d/nervus-surface-camera.conf
	install -D -m 0644 "$REPO/surface-camera/90-nervus-surface.rules" /etc/udev/rules.d/90-nervus-surface.rules
	install -D -m 0755 "$REPO/surface-camera/surface-camera" /usr/libexec/nervus/surface-camera
	install -D -m 0644 "$REPO/surface-camera/nervus-surface-camera.service" /etc/systemd/system/nervus-surface-camera.service
	usermod -aG video "$DESKTOP_USER"
	udevadm control --reload 2>/dev/null || true
	systemctl daemon-reload
	ok "started on demand when Hand Teleop opens"
fi

build_app hand-teleop hand-teleop
install_launcher nervus-handteleop
printf '\nDone. Open "Hand Teleop" from the desktop (log out and in once if you were just added to the video group).\n'
