# RDK S100 Handoff — start here

Branch point for continuing work on the **D-Robotics RDK S100** (on-robot compute).
Everything below is verified state as of **2026-07-24**, not assumption.

---

## 0. The one blocker that stops everything

**V2BOX (the China-path proxy) must be disabled before ANY LAN work.**

It is a TUN-mode VPN that takes the default route and makes every LAN target
unreachable — dogs, RDK, multicast discovery — with `[Errno 65] No route to host`.
`scutil --nc stop "V2BOX"` does **not** hold: its on-demand rule relaunches it
within ~60 s. It must be *disabled*, not stopped:

```bash
sudo networksetup -setnetworkserviceenabled "V2BOX" off   # needs the macOS LOGIN password
route -n get default | grep interface                     # MUST print: interface: en0
```

If `sudo` keeps rejecting the password, use the GUI instead — no sudo needed:
**System Settings → VPN → V2BOX → Off**, and disable **Connect On Demand**.

Verify with `networksetup -listallnetworkservices | grep -i v2box` — an
**asterisk `*` means disabled**. No asterisk = still enabled = still blocked.

Re-enable when done: `sudo networksetup -setnetworkserviceenabled "V2BOX" on`

---

## 1. RDK S100 facts

| Item | Value |
|---|---|
| LAN IP | `10.72.1.186` (on `ADVX-Players`, same `/20` as the Mac) |
| SSH user | `sunrise` |
| Password | changed from the default `sunrise` (not recorded here) |
| OS | Ubuntu 22.04, arm64 |
| Wi-Fi | **None onboard** — needs a USB Wi-Fi dongle to join `ADVX-Players` |
| Ethernet | **Two onboard RJ45**: `U43` = eth0 (DHCP/user), `U45` = eth1 (static `192.168.127.10`, management port) |
| J16 Type-C | **Flash/debug only** (CH340 UART @ 921600 + firmware download). **Not** a network port. |

**Direct-laptop link (no venue network needed):** Mac (via a USB-C→Ethernet
adapter) ↔ RDK **U45**, set the Mac side to `192.168.127.100/24`, then
`ssh sunrise@192.168.127.10`.

My Mac's SSH public key was to be added to `~/.ssh/authorized_keys` on the RDK —
**verify this happened**, otherwise SSH will prompt for a password:

```
ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAICN/9G2/Ik5JBuhW08VYcxDAQo8MKkAP+nbwa4y1X5+A claude-epq-deploy
```

---

## 2. TODO on the RDK (nothing below has been run yet)

### 2a. Connect and recon
```bash
ssh sunrise@10.72.1.186
uname -m && cat /etc/os-release | head -2
ip -br link show            # look for can0
```

### 2b. CAN bus for the AgileX PiPER
The arm's USB-CAN dongle is **Linux-only firmware** (gs_usb/SocketCAN) — this is
precisely why it could not work on macOS. On the RDK it is `can0`:
```bash
sudo ip link set can0 up type can bitrate 1000000     # PiPER is CAN2.0B @ 1 Mbps
ip -details -statistics link show can0
sudo apt install -y can-utils && candump can0          # verify traffic
```

### 2c. Arm SDKs
```bash
sudo apt update && sudo apt install -y git python3-pip python3-venv can-utils
mkdir -p ~/NervusOS/robots/vendor && cd ~/NervusOS/robots/vendor
git clone https://github.com/agilexrobotics/pyAgxArm.git      # unified: PiPER + NERO
git clone https://github.com/agilexrobotics/piper_sdk.git
python3 -m venv ~/NervusOS/robots/.venv
~/NervusOS/robots/.venv/bin/pip install -e pyAgxArm python-can
```

### 2d. Node + Claude Code
```bash
curl -fsSL https://deb.nodesource.com/setup_22.x | sudo -E bash -
sudo apt install -y nodejs
sudo npm install -g @anthropic-ai/claude-code
claude   # first run walks through auth
```

### 2e. VS Code — use **Remote-SSH from the Mac**, not the GUI on the board
Running the VS Code GUI on the S100 over HDMI is heavy and clunky. Instead:
install VS Code on the **Mac** + the **Remote-SSH** extension → connect to
`sunrise@10.72.1.186`. VS Code auto-installs its arm64 server on the RDK, and the
integrated terminal is a shell **on the board** — run `claude` there and it has
full access to the RDK's files, CAN, and BPU.

### 2f. Then port the arm work over
`scripts/arm_set_zero_j6.py` (already written, macOS/Linux/Windows aware) will
auto-select `socketcan`/`can0` on Linux. The PiPER **must** have its J6 zero set
after the flange is installed (manual §3.3).

---

## 3. State of the robots

| Robot | Serial | Status |
|---|---|---|
| GO2 Air | `B42D1000AIRXXXXX` | provisioned, **has the working lidar**, was at `10.72.13.201` |
| GO2 Pro | `B42D2000PROXXXXX` | provisioned, **lidar broken**, was at `10.72.13.244` |
| GO2 (new) | `B42D2000NEWXXXXX` = `Go2_XXXXX` | provisioned via **Unitree app**; CLI BLE provisioning impossible (see below) |
| AgileX PiPER | — | not yet wired; needs RDK + `can0` |
| AgileX NERO | — | DimOS adapter written & protocol-verified, untested on hardware |

Only **one dog is powered on at a time** right now (the new one). `--discover`
will show exactly one entry — that is expected, not a fault.

### Two proven firmware gotchas
1. **`dimos go2tool connect-wifi` cannot provision the new dog.** Proven: BLE
   connects, GATT is healthy (`ffe0` service, `ffe1` notify @handle 12, `ffe2`
   write @handle 17), the handshake write is ACK'd — and the robot sends **zero**
   notifications. It rejects the hardcoded AES key + `b"unitree"` payload in
   `dimos/robot/unitree/go2/cli/ble.py`. Not fixable by retry/power-cycle.
   → **Provision newer-firmware dogs with the Unitree phone app.**
2. **Go2 firmware ≥ 1.1.15 needs a per-device AES-128 key for WebRTC**
   (`con_notify` returns `data2=3`). Symptom is a connect failure mentioning
   data2=3. Fetch once per robot with your Unitree account:
   ```bash
   cd ~/NervusOS/robots/vendor/go2_webrtc_connect
   ~/NervusOS/robots/.venv/bin/python examples/fetch_aes_key.py \
       --email YOUR_UNITREE_EMAIL --password YOUR_PASSWORD --sn B42D2000NEWXXXXX
   ```
   Then either `export UNITREE_AES_128_KEY=<32-hex>` or pass `--aes-key <32-hex>`
   — `scripts/go2_connect.py` now supports both, and DimOS reads the same value
   from its `unitree_aes_128_key` global config.

---

## 4. Tooling already built (all in `~/NervusOS/robots/`)

| File | Purpose |
|---|---|
| `scripts/go2_connect.py` | connect/probe a GO2; `--discover`, `--print-ip`, `--serial`, `--ap`, `--move`, `--trick`, `--lidar`, `--damp`, `--aes-key` |
| `scripts/go2_ble_probe.py` | dump a dog's BLE GATT table (diagnoses provisioning failures) |
| `scripts/arm_connect.py` | connect/probe NERO or PiPER — **read-only**, no motion |
| `scripts/arm_set_zero_j6.py` | PiPER J6 zero-set over CAN, GUI-free, interactive |
| `dimos_ext/nero/` | NERO adapter implementing DimOS `ManipulatorAdapter` |
| `README.md` | full reference + known blockers |

### Working GO2 command sequence
```bash
# after V2BOX is disabled and route is en0
cd ~/NervusOS/robots
./.venv/bin/python scripts/go2_connect.py --discover
export ROBOT_IP=$(./.venv/bin/python scripts/go2_connect.py --serial B42D2000NEWXXXXX --print-ip)
echo "ROBOT_IP=$ROBOT_IP"     # must be non-empty
cd ~/dimos && source .venv/bin/activate && dimos run unitree-go2
```
`ROBOT_IP` only exists in the shell that exported it. **Never** use `<placeholder>`
syntax — bash reads `<` as a redirect and silently leaves the var unset, which
surfaces as `AssertionError: IP address must be provided`.

### Multiple robots (future)
DimOS already supports it via a comma-separated `ROBOT_IPS`:
```bash
ROBOT_IPS=10.72.x.x,10.72.y.y dimos run unitree-go2-multi
ROBOT_IPS=10.72.x.x,10.72.y.y dimos run unitree-go2-fleet
ROBOT_IPS=10.72.x.x,10.72.y.y dimos run unitree-go2-multi-teleop
```
The arm is orthogonal — CAN-local on the RDK, not on the network.

---

## 5. Documentation links

### D-Robotics RDK S100
- Docs home — https://d-robotics.github.io/rdk_doc/en/rdk_s/RDK
- Hardware / connector map (U43, U45, J16) — https://d-robotics.github.io/rdk_doc/en/rdk_s/Quick_start/hardware_introduction/rdk_s100/
- Remote login (SSH) — https://d-robotics.github.io/rdk_doc/en/rdk_s/Quick_start/remote_login/
- GitHub org — https://github.com/D-Robotics

### AgileX (PiPER / NERO)
- pyAgxArm (unified SDK, use this) — https://github.com/agilexrobotics/pyAgxArm
- piper_sdk — https://github.com/agilexrobotics/piper_sdk
- piper_ros — https://github.com/agilexrobotics/piper_ros
- Support / docs portal — https://agilexsupport.yuque.com
- PiPER manual: AgileX's *PiPER Standard user manual* (PDF) (J6 zero = §3.3, wiring A→G = §3.2.2, CAN pinout = §3.2.3, e-stop = §7.3)

### Unitree GO2
- GO2 robot docs — https://unitree-go2-robot.github.io/
- go2_webrtc_connect (vendored; WebRTC + AES key CLI) — https://github.com/legion1581/go2_webrtc_connect
- unitree_sdk2_python (DDS path; needs EDU/wired) — https://github.com/unitreerobotics/unitree_sdk2_python
- go2_ros2_sdk — https://github.com/abizovnuralem/go2_ros2_sdk

### DimOS
- Repo — https://github.com/dimensionalOS/dimos
- Docs folder — https://github.com/dimensionalOS/dimos/tree/main/docs
- Hosted docs — https://dimensionalos.mintlify.site/introduction

### Toolchain
- Claude Code — https://docs.claude.com/en/docs/claude-code
- VS Code Remote-SSH — https://code.visualstudio.com/docs/remote/ssh
- Node.js via NodeSource — https://github.com/nodesource/distributions

---

## 6. Safety constraints (carry forward)

- **Do not modify borrowed AgileX hardware** — no relays, no
  cut wires; use its own e-stop. Setting the J6 zero is *not* a modification: it
  is AgileX's own required, reversible calibration (manual §3.3).
- PiPER: 24 V (never >26 V), payload ≤ 1.5 kg, e-stop = pull the aviation plug (§7.3).
- GO2 `BackFlip` / `HandStand` / `FrontPounce` are genuinely dangerous indoors —
  gate them behind the permission model before third-party apps touch this layer.
- `Move` is fire-and-forget: it must be streamed ~10 Hz; stop streaming and the
  robot stops. That is the safety property the scripts rely on.

## 7. Not done yet
- Nothing has touched the RDK — no SSH session has succeeded (V2BOX blocked every attempt).
- No arm has been connected or driven; NERO adapter untested on hardware.
- No NervusOS Provider/arbiter integration — these are standalone scripts.
- `~/agilex/` is a superseded duplicate of this tree and can be deleted.
