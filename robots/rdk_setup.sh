#!/usr/bin/env bash
# RDK S100 provisioning — covers RDK_HANDOFF.md sections 2b, 2c, 2d.
# Run ON the RDK as user `sunrise`. Idempotent: safe to re-run.
#
#   bash ~/rdk_setup.sh            # everything
#   bash ~/rdk_setup.sh can        # only the CAN bring-up
#   bash ~/rdk_setup.sh sdk        # only the arm SDKs
#   bash ~/rdk_setup.sh node       # only Node + Claude Code

set -uo pipefail

ROOT="$HOME/NervusOS/robots"
VENV="$ROOT/.venv"
VENDOR="$ROOT/vendor"
STEP="${1:-all}"

log()  { printf '\n\033[1;36m==> %s\033[0m\n' "$*"; }
ok()   { printf '\033[1;32m  [ok]\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33m  [warn]\033[0m %s\n' "$*"; }
die()  { printf '\033[1;31m  [FAIL]\033[0m %s\n' "$*"; exit 1; }

# Keep one sudo timestamp for the whole run so it prompts at most once.
log "sudo credentials (the sunrise user's password)"
sudo -v || die "sudo failed"
( while true; do sudo -n true; sleep 50; kill -0 "$$" 2>/dev/null || exit; done ) 2>/dev/null &
SUDO_KEEPALIVE=$!
trap 'kill $SUDO_KEEPALIVE 2>/dev/null' EXIT

# ---------------------------------------------------------------- 2a recon
log "Recon"
echo "  arch:   $(uname -m)"
echo "  kernel: $(uname -r)"
grep -E '^(NAME|VERSION)=' /etc/os-release | sed 's/^/  /'
echo "  net links:"
ip -br link show | sed 's/^/    /'

# ------------------------------------------------------------- packages
if [[ "$STEP" == "all" || "$STEP" == "apt" || "$STEP" == "can" || "$STEP" == "sdk" ]]; then
  log "APT packages"
  sudo apt-get update -y || warn "apt update had errors, continuing"
  sudo apt-get install -y --no-install-recommends \
      git python3-pip python3-venv can-utils curl ca-certificates build-essential \
    || die "apt install failed"
  ok "git, python3-venv, can-utils, curl, build-essential"
fi

# ------------------------------------------------------------------ 2b CAN
if [[ "$STEP" == "all" || "$STEP" == "can" ]]; then
  log "CAN bus (PiPER = CAN2.0B @ 1 Mbps)"
  if ip -br link show can0 >/dev/null 2>&1; then
    # Reconfiguring bitrate requires the link to be down first.
    sudo ip link set can0 down 2>/dev/null
    if sudo ip link set can0 up type can bitrate 1000000; then
      ok "can0 up @ 1000000 bps"
      ip -details -statistics link show can0 | sed 's/^/    /'
    else
      warn "could not bring can0 up — is the USB-CAN dongle plugged in?"
    fi

    # Persist across reboots via systemd-networkd-independent unit.
    log "Persisting can0 across reboots"
    sudo tee /etc/systemd/system/can0.service >/dev/null <<'UNIT'
[Unit]
Description=Bring up can0 at 1 Mbps (AgileX PiPER)
After=network.target

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/sbin/ip link set can0 up type can bitrate 1000000
ExecStop=/sbin/ip link set can0 down

[Install]
WantedBy=multi-user.target
UNIT
    sudo systemctl daemon-reload
    sudo systemctl enable can0.service >/dev/null 2>&1 && ok "can0.service enabled"
  else
    warn "no can0 interface found."
    echo "    USB-CAN candidates on the bus:"
    lsusb 2>/dev/null | grep -iE 'canable|gs_usb|8dev|candle|1d50|Geschwister' | sed 's/^/      /' \
      || echo "      (none matched — plug in the AgileX USB-CAN dongle)"
    echo "    gs_usb module:"
    lsmod 2>/dev/null | grep -E '^gs_usb|^can' | sed 's/^/      /' || echo "      (not loaded; try: sudo modprobe gs_usb)"
  fi
fi

# ------------------------------------------------------------- 2c arm SDKs
if [[ "$STEP" == "all" || "$STEP" == "sdk" ]]; then
  log "Arm SDKs"
  mkdir -p "$VENDOR"
  cd "$VENDOR" || die "cannot cd $VENDOR"

  clone_or_pull() {
    local url="$1" dir="$2"
    if [[ -d "$dir/.git" ]]; then
      ok "$dir already cloned — pulling"
      git -C "$dir" pull --ff-only 2>&1 | tail -1 | sed 's/^/    /'
    else
      git clone --depth 1 "$url" "$dir" 2>&1 | tail -1 | sed 's/^/    /' \
        && ok "cloned $dir" || warn "clone failed: $url"
    fi
  }
  clone_or_pull https://github.com/agilexrobotics/pyAgxArm.git  pyAgxArm
  clone_or_pull https://github.com/agilexrobotics/piper_sdk.git piper_sdk

  log "Python venv"
  [[ -d "$VENV" ]] || python3 -m venv "$VENV" || die "venv creation failed"
  "$VENV/bin/pip" install --upgrade pip -q 2>&1 | tail -2
  if [[ -d "$VENDOR/pyAgxArm" ]]; then
    "$VENV/bin/pip" install -e "$VENDOR/pyAgxArm" python-can 2>&1 | tail -3 | sed 's/^/    /' \
      && ok "pyAgxArm + python-can installed"
  else
    "$VENV/bin/pip" install python-can 2>&1 | tail -2 | sed 's/^/    /'
    warn "pyAgxArm missing; installed python-can only"
  fi
  "$VENV/bin/python" -c 'import can; print("  python-can", can.__version__)' 2>/dev/null \
    || warn "python-can import failed"
fi

# --------------------------------------------------- 2d Node + Claude Code
if [[ "$STEP" == "all" || "$STEP" == "node" ]]; then
  log "Node.js 22 + Claude Code"
  if command -v node >/dev/null 2>&1 && [[ "$(node -v)" == v2[2-9]* ]]; then
    ok "node $(node -v) already present"
  else
    curl -fsSL https://deb.nodesource.com/setup_22.x | sudo -E bash - \
      || warn "nodesource setup failed"
    sudo apt-get install -y nodejs || die "nodejs install failed"
    ok "node $(node -v 2>/dev/null)"
  fi

  # Install global npm packages without sudo-owned root in ~/.npm
  if command -v claude >/dev/null 2>&1; then
    ok "claude already installed: $(claude --version 2>/dev/null | head -1)"
  else
    sudo npm install -g @anthropic-ai/claude-code 2>&1 | tail -3 | sed 's/^/    /'
    command -v claude >/dev/null 2>&1 \
      && ok "claude installed: $(claude --version 2>/dev/null | head -1)" \
      || warn "claude not on PATH"
  fi
fi

# ------------------------------------------------------------------ summary
log "SUMMARY"
printf '  %-22s %s\n' "arch"        "$(uname -m)"
printf '  %-22s %s\n' "can0"        "$(ip -br link show can0 2>/dev/null | awk '{print $1, $2}' || echo 'ABSENT')"
printf '  %-22s %s\n' "python-can"  "$("$VENV/bin/python" -c 'import can;print(can.__version__)' 2>/dev/null || echo 'no')"
printf '  %-22s %s\n' "pyAgxArm"    "$([[ -d $VENDOR/pyAgxArm ]] && echo present || echo missing)"
printf '  %-22s %s\n' "piper_sdk"   "$([[ -d $VENDOR/piper_sdk ]] && echo present || echo missing)"
printf '  %-22s %s\n' "node"        "$(node -v 2>/dev/null || echo 'no')"
printf '  %-22s %s\n' "claude"      "$(command -v claude >/dev/null 2>&1 && claude --version 2>/dev/null | head -1 || echo 'no')"
echo
echo "  Next:  candump can0            # verify PiPER traffic"
echo "         claude                  # first run walks through auth"
