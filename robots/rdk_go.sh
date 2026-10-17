#!/usr/bin/env bash
# RDK S100 one-shot bootstrap.
#   1. enables sshd
#   2. opens a reverse SSH tunnel out to the Hetzner relay (gives Claude access)
#   3. installs the full stack (handoff 2b-2d)
#
# Copy to a USB stick, plug into the S100, then on the board:
#   bash /media/sunrise/<STICK>/rdk_go.sh
# Needs the sudo password of the sunrise user.

set -uo pipefail

RELAY_USER=root
RELAY_HOST=YOUR_SERVER_IP
RELAY_PORT=2222
ROOT="$HOME/NervusOS/robots"
VENV="$ROOT/.venv"
VENDOR="$ROOT/vendor"

log()  { printf '\n\033[1;36m==> %s\033[0m\n' "$*"; }
ok()   { printf '\033[1;32m  [ok]\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33m  [warn]\033[0m %s\n' "$*"; }

log "sudo (the sunrise user's password)"
sudo -v || { echo "sudo failed"; exit 1; }
( while true; do sudo -n true; sleep 50; kill -0 "$$" 2>/dev/null || exit; done ) 2>/dev/null &
trap 'kill %1 2>/dev/null' EXIT

# ---------------------------------------------------------------- 1. sshd
log "Enabling sshd"
sudo systemctl enable --now ssh 2>/dev/null || sudo systemctl enable --now sshd 2>/dev/null
systemctl is-active ssh 2>/dev/null || systemctl is-active sshd 2>/dev/null || warn "sshd not active"
ss -tlnp 2>/dev/null | grep -q ':22' && ok "sshd listening on 22" || warn "nothing on port 22"

# ------------------------------------------------------------ 2. tunnel key
log "Installing tunnel key"
mkdir -p "$HOME/.ssh" && chmod 700 "$HOME/.ssh"
# The key is made on the board, never shipped inside this script (an earlier version carried one, and a key
# without a forced command can run any command on the relay). Authorize the printed public key on the relay as:
#   restrict,port-forwarding,permitlisten="127.0.0.1:2222",command="/bin/false" ssh-ed25519 AAAA... rdk-tunnel
# `ssh -N -R` opens no session, so the forced command never runs and the tunnel still works.
[ -f "$HOME/.ssh/rdk_tunnel" ] || ssh-keygen -q -t ed25519 -N '' -C rdk-tunnel -f "$HOME/.ssh/rdk_tunnel"
chmod 600 "$HOME/.ssh/rdk_tunnel"
ok "authorize this public key on the relay (options above):"
cat "$HOME/.ssh/rdk_tunnel.pub"

log "Checking relay reachability"
if timeout 10 bash -c "cat < /dev/null > /dev/tcp/$RELAY_HOST/22" 2>/dev/null; then
  ok "relay $RELAY_HOST:22 reachable"
else
  warn "cannot reach $RELAY_HOST:22 — check the board's internet connection"
fi

# ------------------------------------------------ 3. persistent tunnel unit
log "Installing tunnel as a systemd service (survives reboot + reconnects)"
sudo tee /etc/systemd/system/rdk-tunnel.service >/dev/null <<UNIT
[Unit]
Description=Reverse SSH tunnel to relay (exposes local sshd on relay:$RELAY_PORT)
After=network-online.target
Wants=network-online.target

[Service]
User=$USER
ExecStart=/usr/bin/ssh -i $HOME/.ssh/rdk_tunnel -N \\
  -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \\
  -o ExitOnForwardFailure=yes -o ServerAliveInterval=30 -o ServerAliveCountMax=3 \\
  -R $RELAY_PORT:localhost:22 $RELAY_USER@$RELAY_HOST
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
UNIT
sudo systemctl daemon-reload
sudo systemctl enable --now rdk-tunnel.service
sleep 5
systemctl is-active --quiet rdk-tunnel.service \
  && ok "tunnel service active" \
  || { warn "tunnel failed — last log:"; sudo journalctl -u rdk-tunnel -n 15 --no-pager | sed 's/^/    /'; }

# ------------------------------------------------------------ 4. the install
log "APT packages"
sudo apt-get update -y
sudo apt-get install -y --no-install-recommends \
    git python3-pip python3-venv can-utils curl ca-certificates build-essential

log "CAN bus"
if ip -br link show can0 >/dev/null 2>&1; then
  sudo ip link set can0 down 2>/dev/null
  sudo ip link set can0 up type can bitrate 1000000 && ok "can0 up @ 1 Mbps" || warn "can0 up failed"
else
  warn "no can0 — plug in the AgileX USB-CAN dongle (then: sudo modprobe gs_usb)"
fi

log "Arm SDKs"
mkdir -p "$VENDOR" && cd "$VENDOR" || exit 1
for r in pyAgxArm piper_sdk; do
  [[ -d "$r/.git" ]] && git -C "$r" pull --ff-only >/dev/null 2>&1 \
    || git clone --depth 1 "https://github.com/agilexrobotics/$r.git" "$r" 2>&1 | tail -1
done
[[ -d "$VENV" ]] || python3 -m venv "$VENV"
"$VENV/bin/pip" install -q -U pip
"$VENV/bin/pip" install -e "$VENDOR/pyAgxArm" python-can 2>&1 | tail -2

log "Node 22 + Claude Code"
if ! command -v node >/dev/null 2>&1; then
  curl -fsSL https://deb.nodesource.com/setup_22.x | sudo -E bash -
  sudo apt-get install -y nodejs
fi
command -v claude >/dev/null 2>&1 || sudo npm install -g @anthropic-ai/claude-code

# ------------------------------------------------------------------ summary
log "SUMMARY"
printf '  %-14s %s\n' "arch"    "$(uname -m)"
printf '  %-14s %s\n' "ip"      "$(hostname -I 2>/dev/null)"
printf '  %-14s %s\n' "sshd"    "$(systemctl is-active ssh 2>/dev/null || echo no)"
printf '  %-14s %s\n' "tunnel"  "$(systemctl is-active rdk-tunnel 2>/dev/null || echo no)"
printf '  %-14s %s\n' "can0"    "$(ip -br link show can0 2>/dev/null | awk '{print $2}' || echo ABSENT)"
printf '  %-14s %s\n' "node"    "$(node -v 2>/dev/null || echo no)"
printf '  %-14s %s\n' "claude"  "$(claude --version 2>/dev/null | head -1 || echo no)"
echo
echo "  If 'tunnel' says active, Claude can now reach this board."
