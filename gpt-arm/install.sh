#!/usr/bin/env bash
# Install GPT Arm for the desktop user on the robot host (Ubuntu 24.04): the service (a systemd user unit
# on 127.0.0.1:8800) and the desktop app. Run from the gpt-arm folder, as the desktop user (no sudo).
#   ./install.sh [--python /path/to/python]
set -euo pipefail
cd "$(dirname "$0")"
PY=/opt/nervus/act/venv/bin/python
[[ "${1:-}" == "--python" ]] && PY="$2"
"$PY" -c "import numpy, cv2" || { echo "the Python at $PY needs numpy and opencv" >&2; exit 1; }
mkdir -p ~/.config/systemd/user ~/.local/bin
[[ "$PWD" == "$HOME/gpt-arm" ]] || ln -sfn "$PWD" "$HOME/gpt-arm"
sed "s#^ExecStart=.*#ExecStart=$PY -m gptarm.server --port 8800#" desktop/gptarm.service > ~/.config/systemd/user/gptarm.service
install -Dm755 desktop/gptarm_portal.py ~/.local/bin/gptarm-portal
install -Dm644 desktop/gptarm.svg ~/.local/share/icons/hicolor/scalable/apps/gptarm.svg
install -Dm644 desktop/gptarm.desktop ~/.local/share/applications/gptarm.desktop
desk="$(xdg-user-dir DESKTOP 2>/dev/null || echo "$HOME/Desktop")"
if [[ -d "$desk" ]]; then
    install -Dm755 desktop/gptarm.desktop "$desk/gptarm.desktop"
    gio set "$desk/gptarm.desktop" metadata::trusted true 2>/dev/null || true
fi
systemctl --user daemon-reload
systemctl --user enable gptarm.service >/dev/null
echo "installed. Start: systemctl --user start gptarm.service, or open GPT Arm from the desktop."

# Optional: the wrist camera's depth (Orbbec Dabai DC1 through OpenNI2; see gptarm/depth.py).
if [[ "${GPTARM_DEPTH:-1}" == "1" ]]; then
    if [[ ! -f vendor-openni/libOpenNI2.so ]]; then
        tmp="$(mktemp -d)"
        git clone -q --depth 1 https://github.com/orbbec/ros2_astra_camera "$tmp/rac" \
            && mkdir -p vendor-openni && cp -r "$tmp/rac/astra_camera/openni2_redist/x64/." vendor-openni/ \
            && cp "$tmp/rac/astra_camera/scripts/56-orbbec-usb.rules" vendor-openni/
        rm -rf "$tmp"
    fi
    "$PY" -m pip install -q openni || echo "could not install the openni package: depth stays off"
    [[ -f /etc/udev/rules.d/56-orbbec-usb.rules ]] || echo "for depth, as root: install -m644 vendor-openni/56-orbbec-usb.rules /etc/udev/rules.d/ && udevadm control --reload-rules && udevadm trigger"
fi
