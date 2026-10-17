# HANDOFF — robot SDK setup (NERO / PiPER / GO2)

> Written 2026-07-23 by a Claude Code session that was stopped mid-task to avoid
> conflicting with a parallel session. Everything below is **already done on disk**
> unless marked ⏳ or ❌. Read this before touching `robots/`.

Everything lives under `/Users/ianzhang/NervusOS/robots/`. **Nothing outside that
directory was created or modified.** No files in `nervus-os/`, `nervus-console/`,
`docs/`, `firmware/`, or `.gitignore` were touched.

---

## 1. Key findings (these overturned my initial assumptions — read them)

1. **`pyAgxArm` is the right SDK for BOTH arms.** It is AgileX's unified Python SDK
   covering `nero`, `piper`, `piper_h`, `piper_l`, `piper_x`, plus the AgxGripper and
   Revo2 dexterous hand. It supersedes the older per-arm SDKs. The NERO manual (§9)
   points at it: <https://github.com/agilexrobotics/pyAgxArm>

2. **macOS is officially supported.** `pyAgxArm` lists `macOS (Darwin)` in its
   Environment section and every demo has a `Darwin` branch. So the arms CAN be
   driven natively from this Mac — no Linux VM, no RK3588 required for bring-up.

3. **The macOS transport is `slcan` over a serial device**, because:
   - macOS has no SocketCAN (Linux-only).
   - `agx_cando` (the Windows backend) is Windows-only — `setup.py` hard-refuses
     non-`win32`, and it binds `cando.dll` via `ctypes.WinDLL`. Do not try to install
     `python-can-agx-cando` on this machine.
   - Per-platform matrix straight from `pyAgxArm/README.md`:

     | OS | `interface=` | `channel=` |
     |---|---|---|
     | Linux (RK3588 target) | `socketcan` | `can0` |
     | Windows | `agx_cando` | `"0"` |
     | **macOS (this machine)** | **`slcan`** | **`/dev/ttyACM0`** ← see caveat |

   - ⚠️ **Caveat I could not verify without hardware:** AgileX documents the macOS
     channel as `/dev/ttyACM0`, which is *Linux* device naming. On macOS the adapter
     will actually enumerate as `/dev/tty.usbmodem*` (or `/dev/cu.usbmodem*`). The
     channel string almost certainly needs changing to the real macOS path. Also
     unverified: whether the bundled USB-CAN module ships slcan/Lawicel firmware or
     candleLight/gs_usb firmware. If slcan fails, `gs_usb` is also an available
     backend on this box and is worth trying.
   - macOS also needs serial permission first: `sudo chmod 777 <device>`.

4. **`piper_sdk` is NOT socketcan-only** (I said it was at first; that was wrong).
   v0.6.1 exposes `CreateCanBus(can_name=..., bustype=..., judge_flag=False)` and
   ships `demo/V2/piper_set_can.py` using `bustype="slcan"`. It still works — but
   `pyAgxArm` is the better choice since it covers both arms with one API.

5. **No hardware was connected during this session.** No USB-CAN adapter on USB, no
   arm on the network (Mac was on WiFi `10.72.12.25` only). So *nothing involving a
   real arm has been tested* — only imports, API introspection, and env setup.

---

## 2. What is on disk

```
robots/
├── HANDOFF.md          ← this file
├── .venv/              Python 3.11.4 venv (framework python, arm64)
└── vendor/             upstream clones, --depth 1, NOT gitignored yet ❌
    ├── pyAgxArm/               ✅ primary SDK, both arms
    ├── piper_sdk/              ✅ v0.6.1, legacy/reference
    ├── piper_ros/              ✅ ROS2 stack (108 MB; contains the URDF/description)
    ├── python-can-agx-cando/   ✅ cloned, Windows-only — reference only, do NOT install
    ├── unitree_sdk2_python/    ✅ cloned, NOT installed (see §4)
    ├── go2_webrtc_connect/     ✅ installed (finished after I wrote §3 — see below)
    └── go2_ros2_sdk/           ✅ cloned, not installed
```

`piper_description` does **not** exist as a standalone repo (404) — the URDF lives
inside `piper_ros/src`.

### Installed into `.venv` and verified importable

| Package | Version | Status |
|---|---|---|
| python-can[serial] | 4.6.1 | ✅ |
| pyserial | 3.5 | ✅ (required by the slcan backend) |
| typing-extensions | 4.16.0 | ✅ |
| pyAgxArm | 1.0.0 | ✅ editable, from `vendor/pyAgxArm` |
| piper_sdk | 0.6.1 | ✅ editable, from `vendor/piper_sdk` |
| unitree_webrtc_connect | 2.1.2 | ✅ editable, from `vendor/go2_webrtc_connect` |
| aiortc / av / opencv-python / pyaudio / wasmtime | — | ✅ pulled in by the above |

Verified working:
```
platform : Darwin arm64      python-can: 4.6.1
ArmModel members: ['NERO', 'PIPER', 'PIPER_H', 'PIPER_L', 'PIPER_X']
can backends incl.: gs_usb, serial, slcan, socketcan, socketcand, virtual
```

---

## 3. ✅ RESOLVED — the one in-flight item finished cleanly

`pip install -e ./vendor/go2_webrtc_connect` completed with **exit code 0** after the
rest of this file was written. Nothing is left hanging; no cleanup needed. It was run
with `CFLAGS`/`LDFLAGS` pointed at brew's `portaudio` so `pyaudio` would build, and
`pyaudio 0.2.14` did build successfully.

Post-install verification passed:
```
python -c "import pyAgxArm, piper_sdk, can, aiortc"   →  arms + webrtc OK
```

Note it installs under the distribution name **`unitree_webrtc_connect`** (v2.1.2),
not `go2_webrtc_connect`. It also dragged in `numpy 2.4.6` and `opencv-python 5.0.0`,
so if anything later needs pinned older numpy, expect a conflict in this venv.

---

## 4. Not done / decisions left open

- ❌ **`vendor/` is not gitignored.** ~185 MB of upstream clones will show up in
  `git status` in a repo that is otherwise clean. Add to `.gitignore` before any
  commit:
  ```
  /robots/vendor/
  /robots/.venv/
  ```
  (Or convert `vendor/` to git submodules if you'd rather pin upstream SHAs — that's
  a real choice, not an oversight; submodules would suit a project that ships to
  RK3588 targets.)

- ❌ **`unitree_sdk2_python` not installed.** It pins `cyclonedds==0.10.2`, which is
  **not in Homebrew** and must be built from source with `CYCLONEDDS_HOME` set. My
  recommendation: don't build it on macOS at all. The DDS peer needs to sit on the
  GO2's own ethernet/AP network, which is the RK3588's job. Use `go2_webrtc_connect`
  for desktop/dev, `unitree_sdk2_python` on the RK3588.

- ❌ **No control scripts written.** I was going to add a `doctor.py` (enumerate
  serial ports + probe CAN) and one unified `arm_ctl.py` driving both arms off
  `robot.joint_nums` — the API is genuinely uniform across NERO/PiPER, so one script
  covers both. Not started.

- ❌ **No NervusOS integration.** Nothing was wired into the Go kernel, no Provider /
  adapter / Robot Profile written. Per `remember/ADVX-2026-*.md` the arm is meant to
  become a CAN Provider behind the arbiter; that work is untouched.

---

## 5. The API surface, so you don't have to re-derive it

`create_agx_arm_config(robot, comm='can', firmeware_version='default', **kwargs)`
→ `AgxArmFactory.create_arm(cfg)` → `.connect()`.
Note the upstream typo: **`firmeware_version`**, not `firmware_version`.
Firmware enums: `NeroFW.{DEFAULT,V111,V112,V120}` (NERO ≤1.10 → DEFAULT, 1.11 → V111,
1.12 → V112, ≥1.20 → V120), and `PiperFW`.

Arm methods (identical for NERO and PiPER; `robot.joint_nums` is 7 vs 6):

```
enable, disable, reset, electronic_emergency_stop, connect, disconnect, is_ok
move_j, move_l, move_p, move_c, move_js, move_mit      # MIT = dangerous, per manual
set_speed_percent, set_motion_mode, set_normal_mode
set_leader_mode, set_follower_mode                      # master/slave linkage
get_joint_angles, get_flange_pose, get_tcp_pose, fk, get_arm_status, get_fps
get_driver_states, get_motor_states, get_joints_enable_status_list
set_joint_limits_enabled, set_crash_protection_rating, set_tcp_offset
init_effector(robot.OPTIONS.EFFECTOR.AGX_GRIPPER | .REVO2)
```

Working demos to copy from: `vendor/pyAgxArm/pyAgxArm/demos/{nero,piper}/test1.py`,
plus `detect_nero_series.py` / `detect_piper_series.py`.

---

## 6. Safety notes lifted from the two manuals (worth honouring in any script)

- NERO payload ≤ 3 kg; PiPER ≤ 1.5 kg. Supply 24 V — NERO must not exceed 25 V,
  PiPER not 26 V.
- **Avoid J2/J3/J4/J5 at 0° on NERO** — the origin is a singularity. The manual is
  explicit that you cannot issue a Cartesian (point-to-point) command from the home pose; move
  J2/J4 with joint control first.
- NERO's web UI (10.90.0.150, `admin`/`123456`) is a **separate mode from CAN**. The
  state machine treats web / teach / trajectory / master-slave as mutually exclusive;
  "Please configure the correct control mode" means you're in the wrong one.
- Master-slave: bring both arms near zero *before* linking, or the follower snaps to
  the leader's pose. Never set two arms as follower simultaneously.
- If you use a pre-owned PiPER gripper on NERO, its CAN id must be changed first
  (AgileX's *NERO gripper ID change guide*) or it collides and wrecks kinematics + gravity comp.
