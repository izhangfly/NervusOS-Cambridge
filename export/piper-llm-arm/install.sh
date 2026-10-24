#!/usr/bin/env bash
# Install PiPER LLM Arm as a desktop app on Ubuntu 24.04:  sudo ./install.sh
REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$REPO/scripts/install-common.sh"

install_driver

step "LLM Arm service (127.0.0.1:8796)"
install -d -m 0755 /opt/nervus/llm-arm
rm -rf /opt/nervus/llm-arm/nervus_llmarm
cp -r "$REPO/service/engine/nervus_llmarm" /opt/nervus/llm-arm/
install -m 0644 "$REPO/service/README.md" /opt/nervus/llm-arm/README.md
[[ -x /opt/nervus/llm-arm/venv/bin/python ]] || python3 -m venv /opt/nervus/llm-arm/venv
/opt/nervus/llm-arm/venv/bin/pip install -q opencv-python-headless numpy
install -d -o "$DESKTOP_USER" -g "$DESKTOP_USER" -m 0700 "$DESKTOP_HOME/.config/nervus"
sed -e "s/@USER@/$DESKTOP_USER/g; s#@HOME@#$DESKTOP_HOME#g" -e '/^PartOf=nervud.service/d' \
	-e 's#/opt/nervus/act/venv/bin/python#/opt/nervus/llm-arm/venv/bin/python#' \
	"$REPO/service/nervus-llm-arm.service.in" >/etc/systemd/system/nervus-llm-arm.service
systemctl daemon-reload
for k in deepseek openai; do
	[[ -f "$DESKTOP_HOME/.config/nervus/$k.key" ]] || warn "no $k key: install -m 600 /dev/stdin ~/.config/nervus/$k.key"
done
ok "started when the app opens"

build_app llm-arm llm-arm
install_launcher nervus-llmarm
printf '\nDone. Put an API key in ~/.config/nervus (see README), then open "LLM Arm" from the desktop.\n'
