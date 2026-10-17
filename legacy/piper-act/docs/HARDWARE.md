# PiPER Arm — Windows Setup & Operating Notes

> **LIVE UPDATES:** `UPDATE.md` in this same folder is the running Mac↔Dell
> handoff channel — anything newer there (additions, corrections, deletions)
> overrides this document. Skim it before relying on anything below.

AgileX PiPER 6-DOF arm + gripper, driven from **Windows 10** over a candleLight
USB-CAN stick, with an **Orbbec Dabai DC1** camera for vision-to-motion work.

Everything below was established by measurement on this machine. The sections
marked **⚠ hard-won** cost hours to find and are easy to re-break.

---

## 1. Hardware

| Part | Detail |
|---|---|
| Arm | AgileX PiPER, 6-DOF + parallel gripper, CAN 2.0B @ **1 Mbit/s** |
| CAN adapter | candleLight USB-CAN (bytewerk), USB `1d50:606f`, serial `003100244648571720303731` |
| Camera | Orbbec **Dabai DC1**, USB `2BC5:0557` |
| Host | Windows 10 Home, project at `D:\Piper-CAN-Teleop` |

Units on the wire are **0.001 mm** and **0.001 deg** throughout.

---

## 2. Windows install

```powershell
D:\Piper-CAN-Teleop\venv\Scripts\python.exe    # the project interpreter
```

Installed: `python-can`, `gs_usb`, `pyusb`, `libusb`, `piper_sdk` (vendored at
`D:\Piper-CAN-Teleop\piper_sdk`), plus `opencv-python`, `numpy`, `pillow`,
`openai`, `anthropic`, `pygrabber` for the vision work.

### ⚠ hard-won: USB driver must be libusb0

Bind the candleLight to **libusb0 (libusb-win32)** with [Zadig](https://zadig.akeo.ie/).
**WinUSB does not work** — it cannot open this composite device (CAN +
firmware-upgrade interfaces) and `libusb_open` returns `ERROR_ACCESS_DENIED`
([libusb #1131](https://github.com/libusb/libusb/issues/1131)). libusbK also works.

### ⚠ hard-won: libusb-1.0.dll must be on PATH

The `libusb` pip package ships the DLL under
`site-packages\libusb\_platform\windows\x86_64\`, but pyusb's
`ctypes.util.find_library` only searches PATH. It has been copied to the
Python312 install dir and `venv\Scripts`.

### libusb0 quirks worth knowing

- **No `clear_halt`** — pyusb raises `AttributeError`.
- Bulk-transfer timeouts surface as a bare **`OSError` (WinError 10060)**, *not*
  `usb.core.USBError`. Any `except usb.core.USBError` will miss them.

---

## 3. The CAN shim — `scripts/piper_mac_can.py`

`piper_sdk` hardcodes `bustype="socketcan"`, which does not exist on Windows.
The shim rewrites `can.interface.Bus` onto the candleLight. **Import and call
`install()` before constructing `C_PiperInterface_V2`.**

```python
import piper_mac_can; piper_mac_can.install()
piper = C_PiperInterface_V2("candle", judge_flag=False)
```

Design points, all forced by measurement:

- **One I/O thread only.** python-can's native `GsUsbBus` issues concurrent
  send/recv on a single libusb0 handle, which deadlocks and throws access
  violations while jogging. All traffic goes through one thread plus queues.
- **The I/O thread can never die.** Both directions catch `(OSError,
  usb.core.USBError)`; a failed write is retried then dropped.
- **Writes bypass `GsUsb.send()`** to pass an explicit 30 ms timeout. pyusb's
  1000 ms default would freeze the 20 Hz control loop on one stalled write.
- **RX drains in bursts of 64**, not one frame per lap — see below.

### ⚠ hard-won: the "goes deaf" bug was never RX

Two compounding faults produced every "second connection is dead" symptom:

1. **A bulk-OUT timeout escaped and killed the I/O thread.** The `except
   OSError` wrapped only `read()`; `dev.send()` was guarded by `except
   queue.Empty` alone. One timeout killed the thread and the bus stayed dead.

2. **The adapter's TX endpoint stalls when its buffer pool is left full.** The
   candleLight shares one small pool between received frames and the TX echoes
   it owes the host. A process exiting mid-flood leaves it full; the firmware
   then refuses *all writes* while **RX keeps streaming perfectly** — so the arm
   reports its pose but silently ignores every command. Measured directly:
   `TX 0/5, RX 2382 frames/s`.
   **The cure is to read the backlog**, which hands the buffers back. Reusing
   the handle does not help; `usb.reset()` does not help. This matches the
   documented legacy-firmware USB buffer overflows on busy buses
   ([python-can #2016](https://github.com/hardbyte/python-can/issues/2016)).

So `_open()` drains 0.4 s, then TX-probes with a harmless `0x4AF` firmware query,
re-opening up to 4× until writes succeed. A session therefore always starts
healthy no matter how the last one ended.

**Debunked — do not revisit:** `PiperInit()`'s query sends are not the cause
(`ConnectPort(piper_init=False)` only *looked* like a fix because it sends
nothing at connect); `usb.reset()` is not the wedge cause; the 100 ms RX timeout
was never the issue.

### Health check

`scripts/piper_can_check.py` — a healthy powered arm floods the bus at
**~2300 frames/s** with `0x2A1-0x2AC` pose and `0x251-0x256` joint feedback,
with no input needed. Silence means wiring or power, not software.

A healthy SDK connect shows `tx=13, txfail=0, heal=0, rx≈5850` per 2 s
(`scripts/piper_sdk_probe.py`).

---

## 4. Arm behaviour

### Enable sequence ⚠ hard-won

```python
piper.EnableArm(7, 0x02)
time.sleep(1.2)
piper.MotionCtrl_2(0x01, 0x00, speed, 0x00)   # CAN control + MOVE P
```

**Never send `EmergencyStop(0x02)` "resume" first.** It leaves the motors
disabled and a following `EnableArm` never re-enables them (0/6 motors).

### ⚠ hard-won: joint-zero is a kinematic singularity

Do **not** treat `JointCtrl(0,0,0,0,0,0)` as home. Measured there
(end pose `[56, 0, 213, 0, 85, 0]`): **only Z can be jogged**; X, Y, RX and RZ
all return `TARGET_POS_EXCEEDS_LIMIT` immediately. The arm stands straight up
with its joints aligned, and the wrist Euler angles land at **RY ≈ 85°** — one
degree off gimbal lock, where RX/RZ are degenerate.

**Use the READY pose instead:**

```
joints (0, 45, -60, 0, 45, 0) deg  ->  end pose [138, 0, 418, 180, 65, 180]
```

Measured **6/6 axes jogging cleanly**: X ±50 mm, Y ±36 mm, Z ±47 mm, RX ±16°,
RY +23°, RZ fine. Two other elbow-bent poses also gave 6/6, so the principle is
*elbow bent, wrist clear of ±90° Euler* — not this exact pose.
`scripts/piper_ready_pose.py` re-runs the survey.

Getting there uses MOVE J (`ModeCtrl(0x01,0x01,speed,0x00)` then `JointCtrl`),
which needs no IK and therefore never faults.

### Fault taxonomy — two different things ⚠

Status byte 1 of the feedback frame:

| Code | Meaning | Handling |
|---|---|---|
| `0x02` | NO_SOLUTION | **soft** |
| `0x03` | SINGULARITY_POINT | **soft** |
| `0x04` | TARGET_POS_EXCEEDS_LIMIT (the firmware's "target **angle** exceeds limit": a *joint angle* limit, not the spatial box) | **soft** |
| `0x01, 0x05-0x0A` | e-stop, joint comm, brake, collision, joint status, other | **hard** |

**Soft** means "that particular setpoint was unreachable" — the arm is healthy
and it clears itself in **~1.3 s** once you stop pushing. The correct response is
to stop the jog and let the operator steer away. Homing on a soft fault throws
away the operator's position for no reason, and is the single most annoying
possible behaviour. Only **hard** faults earn the joint-space recovery.

**Byte 6 is a per-joint bitfield** (`err_status.joint_N_angle_limit`), so the UI
can name the exact joint that blocked the move rather than saying "limit".

### Drag-teach button ⚠ hard-won (corrected 2026-09-11)

The round button between J5/J6 is the **drag-teach** button — solid green =
recording, flashing = playback. When it latches teaching mode
(`ctrl_mode=TEACHING_MODE`), the arm silently ignores Cartesian/joint motion.
**Recovery:** the normal enable sequence — `EnableArm(7,0x02)` +
`MotionCtrl_2(0x01,0x00,speed,0x00)` — **does** flip `ctrl_mode` 0x02 → 0x01
(CAN_CTRL) and restores control; the teleop's ENABLE button therefore recovers
from teaching mode. (An earlier note here claimed "no CAN command can exit it"
— that was disproven on the arm; `scripts/piper_enable_check.py` shows the
enable sequence pulling it out.) The physical button click (light off) also
works.

### Speed ⚠ hard-won

`EndPoseCtrl` is **point-to-point MOVE P**, and every new setpoint restarts the
planner's acceleration ramp. Two consequences:

1. **Keep the commanded target about one control period ahead.** Measured on Z:
   a 0.35 s lookahead crawled at **2 mm/s**; 0.05 s gave **88 mm/s**. Further
   ahead is *slower*, which is deeply counter-intuitive.
2. **Acceleration was the real cap.** `END_MAX_LINEAR_ACC` is in 0.001 m/s², so
   the old value of `250` meant 0.25 m/s² — too gentle to reach even half the
   requested speed before braking. That is why 50% and 100% felt identical.

Current values (`END_MAX_LINEAR_VEL 900`, `ANGULAR_VEL 1800`, `LINEAR_ACC 1200`,
`ANGULAR_ACC 2000`) with a one-period lookahead give a monotonic slider:

| Slider | Measured |
|---|---|
| 15 % | 27 mm/s |
| 50 % | 58 mm/s |
| 100 % | 71 mm/s |

**Going faster than ~75 mm/s** needs a different control mode: streamed MOVE P
is the limit. The protocol exposes **`MOVE_CPV` (0x05, continuous path
velocity, firmware ≥ V1.6.5)**, which is the documented route to genuine
velocity control — untested here.

---

## 5. Teleop server — `scripts/piper_cartesian_ctl.py`

```powershell
D:\Piper-CAN-Teleop\venv\Scripts\python.exe D:\Piper-CAN-Teleop\scripts\piper_cartesian_ctl.py
# then open http://127.0.0.1:8770
```

WASD/arrow Cartesian jogging, speed slider, gripper, HOME (→ READY), E-stop, and
a live call log. A **heartbeat deadman** stops the jog if the browser stops
talking (`HEARTBEAT_TIMEOUT` 1.5 s).

API: `GET /api/state`; `POST /api/{heartbeat,jog,jogstop,gripper,enable,recover,speed,estop}`.
Axis names are lowercase: `x y z rx ry rz`.

### ⚠ hard-won: the "server hang" was a deadlock

`log_call()` takes `state_lock`, and `_loop()` called it *from inside*
`with state_lock:`. `state_lock` was a plain non-reentrant `threading.Lock`, so
the loop thread blocked forever while holding it and **every HTTP request hung**
(process pinned ~86% CPU, `/api/state` timing out). Fixed by making it an
`RLock`; `/api/state` also no longer holds the lock while writing to the socket.
This — not port conflicts — was the long-standing hang.

---

## 6. Camera — Orbbec Dabai DC1

### ⚠ RGB works over UVC; depth needs OrbbecSDK v1

The Dabai family speaks Orbbec's **legacy OpenNI protocol**:

- The **RGB** module enumerates as a normal UVC camera (`...&MI_00`, shown as
  "Dabai DC1"). Works today with plain OpenCV — measured **640×480, ~12.5 fps**
  (exposure-limited in dim light; brighter scenes give more).
- **Depth** is a separate interface (the `VID_2BC5&PID_0657` node, currently in
  an **Error** state for want of a driver) and requires
  **[OrbbecSDK v1](https://github.com/orbbec/OrbbecSDK)** — the `main` branch,
  which keeps OpenNI support and ships the Windows driver in its `driver` folder.
  **SDK v2 dropped OpenNI devices**, and Dabai DC1 is not in the v2 device table.

RGB alone is what OpenVLA / SmolVLA / π₀ / ACT / Diffusion Policy actually
consume, so depth is optional for a first VLA run.

**Always open the camera by name, never by index** — index 0 is the laptop's
"Integrated Webcam" and the Dabai is index 1, but that order shifts whenever USB
devices change, and silently training on the wrong camera is a miserable bug.

---

## 7. Vision-to-motion — `D:\Piper-CAN-Teleop\vla\`

| File | Purpose |
|---|---|
| `camera.py` | Dabai DC1 capture, by name; JPEG/base64 for vision models |
| `arm.py` | Safe action API: `enable / ready / move_to / move_delta / set_gripper`, workspace-clamped |
| `gpt_driver.py` | Direct driving by **GPT-6 Astra** (`gpt-6-astra`) via tool calling |

```powershell
setx OPENAI_API_KEY "sk-..."      # once, then reopen the shell
cd D:\Piper-CAN-Teleop\vla
..\venv\Scripts\python.exe gpt_driver.py --no-arm "describe what you see"
..\venv\Scripts\python.exe gpt_driver.py "pick up the red block"
```

`arm.py` clamps every target to a conservative workspace box
(X 60–480, Y ±320, Z 80–600 mm) and caps a single step at 60 mm / 30°, because a
language model will cheerfully ask for a pose through the table.

### Where this fits

**VLM-in-the-loop** (what `gpt_driver.py` does) needs no GPU, dataset or
training, and is the fastest path to language-driven manipulation. It is *not*
fast or precise — every step is a round-trip to a large model, so expect seconds
per action.

For closed-loop 10–50 Hz control you want a trained policy. The PiPER is already
supported in the LeRobot ecosystem:

- [`WeGo-Robotics/lerobot_robot_piper`](https://github.com/WeGo-Robotics/lerobot_robot_piper) —
  `piper_follower` / `piper_leader` plugin for teleoperation, dataset recording
  and policy deployment
- [`innovator-zero/piper-aio`](https://github.com/innovator-zero/piper-aio) —
  full imitation-learning stack: bring-up → teleop collection → replay → LeRobot
  conversion → inference
- [huggingface/lerobot#645](https://github.com/huggingface/lerobot/pull/645), [#1335](https://github.com/huggingface/lerobot/issues/1335) — upstream integration

⚠ **Those plugins assume Linux SocketCAN.** On this machine the CAN layer is the
shim in §3, so porting means pointing their bus construction at
`piper_mac_can.install()` rather than `can0` — the SDK calls above it are
unchanged.

Model options, roughly in order of effort:

| Model | Note |
|---|---|
| [SmolVLA](https://huggingface.co/docs/lerobot) | Tightest LeRobot integration, smallest; best first VLA |
| [π₀ / openpi](https://huggingface.co/docs/lerobot/pi0) | Strong generalist, heavier |
| [OpenVLA](https://github.com/openvla/openvla) | 7B, Open X-Embodiment, solid research baseline |
| ACT / Diffusion Policy | Narrow but precise action heads; common paired with a VLA front end |

A common 2026 pattern is two models: a foundation VLA as the language→task front
end, and a narrow imitation policy as the precise action head.

---

## 8. Diagnostics in `scripts\`

| Script | What it answers |
|---|---|
| `piper_can_check.py` | Is the adapter alive and is the arm on the bus? |
| `piper_sdk_probe.py` | Does the SDK connect, and is TX healthy? (`txfail`, `heal`) |
| `piper_tx_recover.py` | Which software recovery un-stalls a wedged TX endpoint |
| `piper_motion_repeat.py` | Back-to-back motion across separate processes |
| `piper_ready_pose.py` | Which joint pose gives the most usable Cartesian axes |
| `piper_axis_survey.py` | Which of the 6 axes can be jogged from a given pose |
| `camera_probe.py` | What the cameras can deliver |

---

## 9. If something breaks

1. **Arm reports pose but ignores commands** → stalled TX endpoint. Restart the
   process; `_open()` self-heals. If not, replug the CAN stick.
2. **`piper_can_check` shows 0 frames** → arm power or CAN wiring, not software.
3. **Adapter "enumerates but will not answer"** (serial read fails, "no langid")
   → genuinely wedged; only a physical replug clears it.
4. **Arm ignores everything, light is solid green** → drag-teach latched. Press
   the physical button.
5. **Only Z jogs** → you are at joint-zero or another singular pose. Press HOME.
6. **Teleop unresponsive, high CPU** → check for a re-introduced `state_lock`
   deadlock (§5).
