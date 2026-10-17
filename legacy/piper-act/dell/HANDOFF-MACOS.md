# PiPER on macOS — WASD/arrow Cartesian teleop

**Read this before touching `piper_mac_can.py` or `piper_cartesian_ctl.py`.**
Status as of **2026-09-03**.

Goal: drive the AgileX PiPER arm's end-effector translation *and* rotation from
WASD/arrow keys on the MacBook, with the arm plugged straight into the laptop via
a candleLight USB-CAN stick. No RDK, no ROS, no DimOS in the loop.

---

## TL;DR status

| Piece | State |
|---|---|
| CAN over libusb on macOS, single-threaded | **Works.** Reproducible, never fails |
| Raw CAN transmit | **Works.** `SEND OK` on `0x472` |
| Teleop UI, key map, leashed jog, deadman | **Works**, proven in `--mock` |
| `piper_sdk` through the shim | **BROKEN.** No RX, sends fail |
| Arm physically moving | **Never achieved** |

The arm is healthy and has never been the problem. It floods the bus with correct
feedback the moment it is powered. Everything unresolved is on the macOS↔USB↔SDK
path.

---

## Hardware / environment

- Arm: AgileX PiPER, 6 DoF + gripper, CAN @ **1 Mbit/s**.
- Adapter: **candleLight USB-CAN**, bytewerk, USB `1d50:606f`,
  serial `<redacted>`. A `gs_usb`-class device.
- Host: MacBook, macOS (Darwin 24.1.0). **macOS has no SocketCAN** — this is the
  root of all the difficulty.
- libusb 1.0.30 via Homebrew (`/opt/homebrew/lib/libusb-1.0.dylib`).
- Python: `/Users/ianzhang/agilex/.venv/bin/python` (uv-managed, no `pip` binary —
  use `uv pip install --python .venv/bin/python ...`).
  Installed for this work: `pyusb`, `gs_usb`, and `piper_sdk` as an editable
  install of `/Users/ianzhang/agilex/piper_sdk`.

> `piper_sdk` was previously importable only as a namespace package from `cwd`,
> so it broke whenever a script ran from another directory. It is now properly
> installed; don't "fix" that by adding `sys.path` hacks.

---

## Files (all in `~/NervusOS/robots/scripts/`)

| File | What it is |
|---|---|
| `piper_can_check.py` | **Start here, always.** Single-threaded CAN sniff. Proves stick + arm + bus. Known-good reference |
| `piper_mac_can.py` | The shim. Redirects `piper_sdk`'s hardcoded `socketcan` onto gs_usb. **Where the open bug lives** |
| `piper_cartesian_ctl.py` | The deliverable: web teleop panel, WASD/arrows, leashed Cartesian jog. `--mock` runs it with no hardware |
| `piper_arm_state.py` | Reads real arm state via the SDK. Refuses to print unless RX is genuinely live |
| `piper_bus_probe.py` | Drives the shim's Bus alone, no SDK. Prints a verdict splitting shim-bug from SDK-threading-bug |
| `piper_webctl.py` | Pre-existing per-joint slider panel (RDK-era). Untouched, no Cartesian jog |

Note `robots/scripts/` is **untracked** in the NervusOS repo (`git status` shows
`?? robots/scripts/`). That is why the RDK's Cartesian version never synced
anywhere and had to be rebuilt.

---

## How to run

```bash
cd ~/NervusOS/robots/scripts
V=/Users/ianzhang/agilex/.venv/bin/python

$V piper_can_check.py       # expect "ARM IS ALIVE" + ~9300 frames/4s
$V piper_arm_state.py       # expect "RX live" + real joint angles   <-- CURRENTLY FAILS
$V piper_cartesian_ctl.py   # http://127.0.0.1:8770
$V piper_cartesian_ctl.py --mock   # full UI, no hardware. This works today
```

In the panel: press **ENABLE** first (motors start disabled by design), then hold
keys. Binds to loopback only — arm control is never exposed to a network.

### Key map (verified against the real arm on the RDK, earlier work)

```
W / S   ±X  forward / back      (arrows Up/Down duplicate)
A / D   ±Y  left / right        (arrows Left/Right duplicate)
Q / E   ±Z  up / down
U / O   roll     I / K  pitch     J / L  yaw
[ / ]   gripper close / open
```

Rotations degenerate near pitch = 90° (gimbal lock). That is a pose limit, not a bug.

---

## THE OPEN BUG

**Symptom.** Through `piper_sdk` + the shim: every joint and pose reads `0.0`,
`piper_arm_state.py` reports `NO RX`, and every command fails with
`SendCanMessage(SEND_MESSAGE_FAILED (100017))`. Running it also leaves the
adapter **wedged** — still enumerating, but refusing control transfers
(`[Errno 60] Operation timed out`, or `The device has no langid`). Only a
**physical unplug/replug** clears that.

**The contrast that defines the bug:** `piper_can_check.py` sniffs the same bus,
seconds later, and gets 9318 frames every time. Same adapter, same arm, same
libusb. It is single-threaded and uses `set_bitrate()` + `start(0)`.

### Ruled out — do not re-investigate

- **The arm, its power, the CAN wiring, termination.** The bus floods with correct
  PiPER IDs (`0x2A1`-`0x2A8` pose, `0x251`-`0x256` joints) whenever powered.
- **The cable / port.** Works through the 140W USB-C cable and through hubs.
- **"A stick left in the started state wedges it."** *Disproved* — `piper_can_check.py`
  ran twice back-to-back with no replug and no `stop()` problem. I asserted this
  earlier; it was wrong.
- **`usb.reset()` as a recovery.** It makes things worse: the device returns
  enumerated but deaf and stops answering string descriptors. **Never call it.**
  Removed from the shim deliberately.
- **The SDK opening several buses.** `C_STD_CAN.Init()` guards on
  `if self.bus is not None`.
- **`bus.state`.** python-can's `BusABC.state` defaults to `ACTIVE`, so the SDK's
  `is_can_bus_ok()` passes whenever the bus object exists.
- **python-can's own `gs_usb` backend.** Separately broken on macOS: it opens and
  transmits but *never receives*, because it uses `set_timing()` +
  `start(HW_TIMESTAMP)`. This is exactly why the shim hand-rolls a `BusABC`.
  Don't "simplify" the shim back onto it.

### Leading hypotheses, in order

1. **Concurrent libusb transfers.** `piper_sdk` reads on a background `ReadCan`
   thread while commands go out from the caller's thread. Two threads on one
   libusb handle is not reliable on macOS and fits every symptom including the
   segfault. The shim now funnels all device access through a single I/O thread
   with queues — **written but never verified against hardware.**
2. **Read timeout too tight.** The I/O loop polls `dev.read(frame, 2)` — a 2 ms
   timeout in a hot loop against a bus carrying ~2300 frames/s. Could overrun the
   adapter's buffering or thrash libusb. Try 50–100 ms.
3. **Stray open handles.** `GsUsb.scan()` opens every matching device to read
   descriptors. Worth confirming the shim isn't leaving a second handle open
   alongside the one it keeps.

### Next experiments — cheapest first

Each attempt costs a physical replug, so run them in this order and stop at the
first that reproduces.

1. **Replug, then run `piper_bus_probe.py`.** It drives the shim's Bus alone —
   single-threaded, no SDK — and prints a verdict. This is the decisive split:
   - RX works and no wedge → the bug is **SDK threading**; hypothesis 1 is right
     and the single-I/O-thread rewrite needs verifying end to end.
   - Fails or wedges → the bug is **in the shim's own bus class**; go to
     hypothesis 2.
2. Raise the I/O-loop read timeout to 50 ms and repeat.
3. Log every `libusb_open` (the debug spam appears with libusb debug enabled) and
   count handles.

A fallback worth considering if this stays stuck: run the arm from a Linux host
with real SocketCAN (the RDK, a VM, or a Pi) and keep the Mac as the UI client
over HTTP. `piper_cartesian_ctl.py` already takes `--iface can0` and binds
elsewhere with `--bind`, so it runs unmodified there. This sidesteps the entire
macOS/libusb problem.

---

## Design notes worth preserving

**Leashed closed-loop jog.** Each 20 Hz tick reads the *actual* end pose and
commands `target = actual + small_lead` on held axes only (`JOG_LEAD_MM=6`,
`JOG_LEAD_DEG=5`). An open-loop integrator lets the commanded target drift past
the workspace and trip `TARGET_POS_EXCEEDS_LIMIT`, and one blocked axis then
freezes the rest. The leash cannot outrun the arm, so neither happens. This
design was proven on the real arm in the earlier RDK work — keep it.

**Safety, all deliberate:**
- Motors start **disabled**; nothing moves until ENABLE.
- 1.5 s heartbeat deadman — closing the tab or sleeping the laptop halts the arm.
- Key release clears that axis; `blur` and `visibilitychange` force-release, so a
  key held while the window loses focus cannot run away.
- E-STOP sends `EmergencyStop` (CAN `0x150`).
- Binds `127.0.0.1` by default.

**Units.** The arm speaks 0.001 mm and 0.001 deg. `EndPoseCtrl(X,Y,Z,RX,RY,RZ)`
takes those units on CAN `0x152`-`0x154`. Cartesian control needs
`MotionCtrl_2(0x01 CAN, 0x00 MOVE P, speed, 0x00)`; `0x151` carries
`move_spd_rate_ctrl` as a percentage of rated speed, floor 1 — a literal 0 stalls
motion mid-travel and reads as a blocked joint.

**Diagnostics lie if RX is dead.** With no RX the SDK returns default-initialised
objects whose fields read `STANDBY` / `NORMAL` / `motors: all False` — which looks
exactly like a healthy idle arm. I misread that as real state once. That is why
`piper_arm_state.py` proves RX is live (by watching the message timestamp advance)
before printing anything. Keep that check.

---

## Where the WASD version came from

It did not exist locally. `piper_webctl.py` here is joint-sliders only. The
`Nervus-OS` GitHub org has no copy (searched all 14 repos; `robot-arm` is a Kotlin
app). The Cartesian version described in memory lived only on the RDK, which was
unreachable (`Connection timed out during banner exchange`) — and
`robots/scripts/` is untracked, so it never synced. `piper_cartesian_ctl.py` is a
rebuild from the SDK, following the design notes above.

DimOS also ships `keyboard_teleop_piper` (pygame; WASD+QE translate, RFTGYH
rotate) in `robots/dimos/.venv/.../robot/manipulators/piper/blueprints/teleop.py`,
but it needs the whole DimOS runtime and CAN on the board.
