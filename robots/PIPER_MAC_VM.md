# Piper Cartesian control on the Mac via a UTM Linux VM

The Piper's bundled USB-CAN dongle is a **bytewerk STM32F072 candleLight** (gs_usb) —
Linux-only, can't be driven from macOS and can't be reflashed to SLCAN (F072 has no
slcan build). See `piper-macos-can-dongle-limit` memory. Solution: run a **Linux VM**
whose kernel has the native `gs_usb` driver, pass the dongle through, bring up `can0`,
run the webctl there, and forward `:8770` to the Mac's `localhost`.

**Non-negotiable requirement:** USB passthrough works **only on UTM's QEMU backend**
(Apple Virtualization can't do it). So the VM must be built with **Emulate → ARM64**,
never "Virtualize". (ARM64-on-ARM64 QEMU still uses the HVF accelerator = near-native.)

Status: UTM 4.7.5 installed. ISO downloading to `~/Downloads/ubuntu-24.04.4-live-server-arm64.iso`.

---

## Phase A — Create the VM (UTM GUI)

1. Open **UTM** → **+ Create a New Virtual Machine**.
2. Choose **Emulate** ⚠️ (NOT "Virtualize" — that kills USB passthrough).
3. **Operating System: Linux**. Do NOT tick "Use Apple Virtualization".
4. **Boot ISO Image → Browse** → `~/Downloads/ubuntu-24.04.4-live-server-arm64.iso`.
5. Architecture: **ARM64 (aarch64)**. System: leave default (`virt`). Continue.
6. **Memory: 4096 MB**, **CPU Cores: 4**. Continue.
7. **Storage: 32 GB**. Continue.
8. Shared Directory: skip (we use SSH/scp). Continue.
9. Name it **piper-linux** → Save. Do NOT start it yet.

## Phase B — Configure USB + network BEFORE first boot (UTM GUI)

Select `piper-linux` → **Edit / settings** (the sliders icon):

- **QEMU** section → confirm the accelerator is HVF (default). Leave UEFI boot on.
- **USB** (or "Sharing") → set **USB Support = USB 3.0**, and enable
  **"Share USB devices"** / USB Sharing. (This only exists on the QEMU backend —
  if you don't see it, the VM was made with Virtualize; delete and redo Phase A.)
- **Network** → Network Mode = **Shared Network** (NAT). Then **Port Forward → New**,
  add two rules (Protocol TCP):
  | Host Address | Host Port | Guest Address | Guest Port |
  |---|---|---|---|
  | 127.0.0.1 | 2222 | (blank) | 22 |
  | 127.0.0.1 | 8770 | (blank) | 8770 |
  (2222 = SSH so I can drive the guest from the Mac; 8770 = the webctl at
  `http://localhost:8770`.)
- Save.

## Phase C — Install Ubuntu Server (in the VM window)

Start `piper-linux`. In the installer:
- Language / keyboard: defaults.
- Network: leave DHCP (the NAT gives it an address automatically).
- Storage: **Use an entire disk** (the 32 GB virtual disk) → Done → Continue (confirm).
- Profile: name/username (e.g. `ian`), server name `piper-linux`, set a password.
- **☑ Install OpenSSH server** — REQUIRED (this + the :2222 forward is how I get in).
- Featured snaps: skip all.
- Let it install → **Reboot Now**. When it says to remove the install medium:
  UTM toolbar → the CD/disc icon → **Clear** / eject the ISO, then hit Enter.

Verify from the **Mac** once it reboots:
```bash
ssh -p 2222 <user>@localhost 'uname -a; echo OK'
```

## Phase D — Pass the dongle through & bring up can0

1. Make sure the candleLight is plugged into the Mac and the **arm's 24 V is on**.
2. With `piper-linux` running, click the **USB icon** in the UTM toolbar →
   select **"candleLight USB to CAN adapter"** to attach it to the VM.
   (It disappears from macOS and appears inside the guest — that's correct.)
3. In the guest (`ssh -p 2222 <user>@localhost`):
```bash
ip -br link show | grep -i can          # expect can0 (gs_usb bound it)
# if absent: sudo modprobe gs_usb; dmesg | tail -20
sudo ip link set can0 up type can bitrate 1000000    # Piper = CAN2.0B @ 1 Mbps
sudo apt update && sudo apt install -y can-utils
candump can0                             # arm powered -> streams frames = ✅ PATH PROVEN
```

## Phase E — SDK + webctl (I drive this over SSH once Phase D shows frames)

```bash
sudo apt install -y git python3-venv python3-pip
mkdir -p ~/piper && cd ~/piper
python3 -m venv .venv
git clone https://github.com/agilexrobotics/piper_sdk.git
git clone https://github.com/agilexrobotics/pyAgxArm.git
./.venv/bin/pip install -e piper_sdk -e pyAgxArm python-can
```
Then I build the Cartesian webctl (leashed closed-loop jog + virtual keyboard +
RECOVER, binding `0.0.0.0:8770`) — the same UX as the old RDK `piper_webctl.py`,
using SocketCAN `can0`. Browse **http://localhost:8770** on the Mac (via the :8770
port forward). Set the Piper's **J6 zero** first if the flange was reinstalled
(`scripts/arm_set_zero_j6.py` logic, socketcan path).

---

## Reality check / caveats
- **Real-time jitter:** CAN over VM USB passthrough can hiccup under load. The
  leashed closed-loop jog (reads actual pose each tick) tolerates this better than
  open-loop. If it's too jittery for smooth control, the fallbacks below win.
- **The dongle is single-owner:** while attached to the VM it's gone from macOS;
  detach it in the UTM USB menu to give it back.

## Better fallback if the VM is too flaky: the Dell (1 TB, Windows)
An x86 Linux box is more reliable than a Mac ARM QEMU VM for real-time CAN. Two ways:
- **WSL2 on Windows + `usbipd-win`**: `winget install dorssel.usbipd-win`, then
  `usbipd bind`/`usbipd attach` the candleLight into WSL2 Ubuntu — gs_usb/can0 works
  in WSL2. No repartitioning, keeps Windows. Fastest path on the Dell.
- **Dual-boot Ubuntu** (or a Ubuntu-on-USB live stick): native SocketCAN, most
  reliable, but repartitions/adds a boot option. Parallel to Windows (pick at boot),
  not simultaneous. WSL2 is simpler unless you want a dedicated robot box.
The Mac can't directly "install Linux onto the Dell" — that's done at the Dell.
