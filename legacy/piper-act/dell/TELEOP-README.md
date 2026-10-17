# PiPER WASD/Arrow Teleop — Windows Handoff

Drive an AgileX **PiPER** arm's end-effector — translation **and** rotation — from
WASD/arrow keys on a PC, with the arm plugged straight into the machine through a
**candleLight USB-CAN stick**. No ROS, no DimOS, no robot OS in the loop; just
`piper_sdk` over CAN.

This folder is a self-contained handoff of everything built and learned in the
macOS debugging session, packaged so development can continue on **Windows**.
The arm has never been the problem — see **Status**.

---

## Status (be honest about this)

| Piece | State |
|---|---|
| CAN over libusb (single-threaded) | **Works.** Reproducible, ~9300 frames/4s |
| Raw CAN transmit | **Works** (`SEND OK` on `0x472`) |
| Teleop UI, key map, leashed jog, deadman | **Works**, proven in `--mock` (no hardware) |
| `piper_sdk` through the shim | **BROKEN on macOS** — no RX, sends fail |
| Arm physically moving | **Never achieved on macOS** |

**The key caveat:** the one thing still broken was broken *on macOS*, and the
symptom is strongly tied to how macOS/libusb handles a device touched from two
threads. **It may not reproduce on Windows at all.** Windows is actually a better
platform for this: it has no SocketCAN either, but libusb's Windows backend
(WinUSB) handles concurrent access differently, and there is no kernel-driver
detach problem. Treat this as "the hard part is probably already done, verify it
on Windows."

If the SDK path turns out to be broken on Windows too, the bug is fully
characterized below with a script that isolates it in one run.

---

## Files

```
Piper-CAN-Teleop/
├── README.md                  this file — start here
├── HANDOFF-MACOS.md           full macOS debugging history (rules-out, traps, rationale)
├── requirements.txt           pip deps (python-can, pyusb, gs_usb, libusb)
├── setup-windows.bat          one-shot venv + driver-install checklist
├── scripts/
│   ├── piper_can_check.py     START HERE — single-threaded CAN sniff, proves stick+arm+bus
│   ├── piper_bus_probe.py     drives the shim's Bus alone; prints a verdict splitting the bug
│   ├── piper_arm_state.py     real arm state via the SDK; refuses to print unless RX is live
│   ├── piper_cartesian_ctl.py THE DELIVERABLE — web teleop panel (WASD/arrows)
│   ├── piper_mac_can.py       the shim: SDK's hardcoded socketcan → gs_usb (portable; name is stale)
│   └── piper_webctl.py        pre-existing joint-slider panel (reference only, RDK-era)
└── piper_sdk/                 vendored SDK source (install from here, not pip)
```

The scripts import `piper_mac_can` from their own directory, so keep `scripts/`
intact as a unit.

---

## Windows setup

**Prerequisites (once):**

1. **Python 3.12**, 64-bit, from python.org — tick **"Add Python to PATH"**.
2. **candleLight WinUSB driver** via [Zadig](https://zadig.akeo.ie):
   `Options → List All Devices → candleLight USB to CAN adapter` (USB `1D50:606F`)
   → driver = **WinUSB** → **Replace Driver**. Without this Windows binds the
   stick to a generic driver and libusb cannot open it.
3. Power the arm on and plug in the stick.

**Install (from this folder):**

```bat
setup-windows.bat
```

or by hand:

```bat
python -m venv venv
venv\Scripts\python -m pip install --upgrade pip
venv\Scripts\python -m pip install -r requirements.txt
venv\Scripts\python -m pip install -e piper_sdk
```

`requirements.txt` includes the `libusb` package, which bundles
`libusb-1.0.dll` — pyusb's libusb1 backend needs it on Windows and won't start
without it.

---

## Run it — in this order

```bat
cd Piper-CAN-Teleop

REM 1. Prove the link
venv\Scripts\python scripts\piper_can_check.py
REM    expect: "ARM IS ALIVE" + ~9300 frames/4s (IDs 0x251-0x256, 0x2A1-0x2A8)

REM 2. Prove the SDK path
venv\Scripts\python scripts\piper_arm_state.py
REM    expect: "RX live", real joint angles, "motors enabled: [False x6]"

REM 3. Control it
venv\Scripts\python scripts\piper_cartesian_ctl.py
REM    open http://127.0.0.1:8770
```

In the panel: press **ENABLE** first (motors start disabled), then hold keys.
Binds to `127.0.0.1` only — arm control is never exposed to a network.

No hardware handy? `venv\Scripts\python scripts\piper_cartesian_ctl.py --mock`
runs the full UI with a simulated arm.

---

## Key map (verified against the real arm in earlier RDK work)

```
W / S   ±X  forward / back      (arrows Up/Down duplicate)
A / D   ±Y  left / right        (arrows Left/Right duplicate)
Q / E   ±Z  up / down
U / O   roll     I / K  pitch     J / L  yaw
[ / ]   gripper close / open
```

Rotations degenerate near pitch = 90° (gimbal lock) — a pose limit, not a bug.

---

## How the shim works (and why it exists)

`piper_sdk` hardcodes `bustype="socketcan"` in `C_PiperInterface_V2.__init__`.
SocketCAN is Linux-only, so on Windows and macOS the SDK cannot open a bus by
itself. `piper_mac_can.py` fixes that by monkeypatching `can.interface.Bus` to
route any bus request onto the candleLight.

Two routes are possible on Windows:

1. **The shim (recommended).** `import piper_mac_can; piper_mac_can.install()`,
   then construct the SDK as usual. It hand-rolls a `can.BusABC` that:
   - touches the device from **exactly one I/O thread** (queues in/out),
   - **filters echo frames** (the adapter echoes our own sends; only
     `echo_id == GS_USB_NONE_ECHO_ID` frames go upstream, otherwise the SDK would
     parse its own commands as arm feedback),
   - shuts down cleanly (`atexit` + join before stop), and
   - **never calls `usb.reset()`** (see traps).

2. **python-can's native gs_usb backend** (worth trying if the shim misbehaves —
   it is the documented candleLight path on Windows):
   ```python
   from piper_sdk import C_PiperInterface_V2
   p = C_PiperInterface_V2(can_auto_init=False)
   p.CreateCanBus(can_name=0, bustype="gs_usb", expected_bitrate=1000000, judge_flag=False)
   p.ConnectPort(piper_init=False)
   ```
   (Channel `0` = first candleLight device; the gs_usb backend takes an index.)

The shim's name says "mac" but the Darwin-specific kernel-driver patch is gated
behind `if platform.system() != "Darwin"`, so it is a no-op on Windows. The
single-I/O-thread design and echo filtering are valuable on every OS.

---

## The open bug (from macOS) and why Windows may sidestep it

**Symptom (macOS):** through the SDK, every joint/pose reads `0.0`, `SEND_MESSAGE_FAILED`
on every command, and the process segfaults on exit — which wedges the adapter
until a physical replug. Meanwhile `piper_can_check.py` (single-threaded) sniffs
the same bus seconds later and gets 9318 frames.

**Root cause (leading, unconfirmed):** `piper_sdk` reads on a background thread
while commands go out from the caller's thread. Two threads issuing concurrent
libusb transfers on one handle does not work on macOS.

**Why Windows likely differs:** libusb's Windows/WinUSB backend serializes access
differently and has no kernel-driver-detach step, so the same two-thread pattern
commonly works there. That is why the recommendation is to just try it.

**If it's broken on Windows too**, run the decisive split first:

```bat
venv\Scripts\python scripts\piper_bus_probe.py
venv\Scripts\python scripts\piper_can_check.py   REM check the adapter survived
```

- `piper_bus_probe.py` gets RX and the adapter survives → the shim's Bus is fine;
  the bug is SDK threading. Verify the single-I/O-thread rewrite end-to-end.
- no RX, or the adapter wedges → the bug is in the shim's Bus class. Raise the
  `_io_loop` read timeout (currently **2 ms**; try 50–100 ms) and re-test.

See `HANDOFF-MACOS.md` for the full list of hypotheses already ruled out — do not
re-investigate the arm, wiring, cable, `usb.reset()`, SDK multi-open, or `bus.state`.

---

## Safety design (deliberate — do not "simplify" away)

- Motors start **disabled**; nothing moves until ENABLE.
- **1.5 s heartbeat deadman** — closing the tab or sleeping the PC halts the arm.
- Key release clears that axis; window `blur`/`visibilitychange` force-release, so
  a key held while the window loses focus cannot run away.
- **E-STOP** sends `EmergencyStop` (CAN `0x150`).
- Binds `127.0.0.1` by default.

**Leashed closed-loop jog.** Each 20 Hz tick reads the *actual* end pose and
commands `target = actual + small_lead` on held axes only (`JOG_LEAD_MM=6`,
`JOG_LEAD_DEG=5`). An open-loop integrator lets the commanded target drift past
the workspace and trip `TARGET_POS_EXCEEDS_LIMIT`, and one blocked axis then
freezes the rest. The leash cannot outrun the arm, so neither happens. Keep it.

---

## Units and CAN IDs

- The arm speaks **0.001 mm** and **0.001 deg**.
- `EndPoseCtrl(X,Y,Z,RX,RY,RZ)` → CAN `0x152`-`0x154` (Euler angles).
- Cartesian control needs `MotionCtrl_2(0x01 CAN, 0x00 MOVE P, speed, 0x00)`.
- `0x151` = `move_spd_rate_ctrl`, a percentage of rated speed, **floor 1** — a
  literal 0 stalls motion mid-travel and reads as a blocked joint.
- Enable = `EnableArm(7, 0x02)` on `0x471`; gripper = `GripperCtrl(...)` on `0x159`.

Reference demos in `piper_sdk/piper_sdk/demo/V2/` — `piper_ctrl_moveP.py`,
`piper_ctrl_end_pose.py`, `piper_ctrl_enable.py`, and `piper_set_can.py`.

---

## Traps that cost hours (read once, remember forever)

1. **Never call `usb.reset()` on the candleLight.** It returns enumerated but
   deaf, stops answering string descriptors (`"The device has no langid"`), and
   only a physical replug clears it. Removed from the shim deliberately.
2. **With no RX, the SDK returns default objects that look like a healthy idle
   arm** — `STANDBY`, `NORMAL`, `motors: all False`. `piper_arm_state.py` exists
   precisely to stop you trusting those: it proves RX is live (watching the
   message timestamp advance) before printing anything.
3. **Every software experiment on the wedge costs a physical replug.** Read
   `HANDOFF-MACOS.md`'s "ruled out" list before re-running anything.
4. `piper_sdk` must be installed as a package (`pip install -e piper_sdk`), not
   imported from a working directory, or it breaks the moment a script runs from
   another folder.
5. Keep a hand near **E-STOP** on the first physical move under any new code path.

---

## Where the WASD version came from

It did not exist in the codebase or on GitHub. The local `piper_webctl.py` is
joint-sliders only; the `Nervus-OS` org (all 14 repos searched) has no copy. The
Cartesian version lived only on a board that was unreachable, so
`piper_cartesian_ctl.py` was rebuilt from `piper_sdk`, following the safety and
leashed-jog design above.
