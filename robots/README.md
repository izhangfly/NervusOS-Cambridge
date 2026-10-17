# robots/ — hardware layer for NervusOS

> **Historical.** This folder is the robot lab from before NervusOS moved to the Dell: two Unitree GO2 dogs,
> the RDK S100 board, the NERO and PiPER arms driven from a Mac. The dogs and the RDK are no longer available
> (September 2026), so nothing here is maintained; the PiPER code that is maintained lives in `hardware/piper/`.
> Board passwords, robot serial numbers and MAC addresses have been replaced with placeholders.

Everything that talks to physical robots lives here. This is the **single
consolidated tree**; an earlier duplicate at `~/agilex/` is superseded and can be
deleted.

| Robot | SDK | Transport | Status |
|---|---|---|---|
| Unitree GO2 Air | `go2_webrtc_connect` | WebRTC (LocalSTA) | scripted, untested on hardware |
| AgileX PiPER (6-DOF) | `pyAgxArm` | CAN 1 Mbps | scripted incl. J6 zero-set; untested on hardware |
| AgileX NERO (7-DOF) | `pyAgxArm` | CAN 1 Mbps | scripted + DimOS adapter written |

## Layout

```
robots/
├── .venv/                  Python 3.11 (gitignored)
├── vendor/                 7 upstream clones (gitignored, ~185 MB)
├── scripts/
│   ├── arm_connect.py           connect/probe NERO or PiPER — READ-ONLY by default
│   ├── arm_set_zero_j6.py       set PiPER J6 zero from macOS (GUI-free, interactive)
│   ├── go2_connect.py           connect/probe GO2 (--discover, move, tricks)
│   └── go2_leader_follower.py   two-dog keyboard teleop; follower trails the leader
└── dimos_ext/
    ├── install.sh          symlink extensions into a dimos checkout
    └── nero/               NERO adapter for DimOS (not supported upstream)
```

## Quick start

```bash
cd ~/NervusOS/robots

# Arms — safe probe, no motion
./.venv/bin/python scripts/arm_connect.py nero
./.venv/bin/python scripts/arm_connect.py piper --list

# Dog — probe, then a 1 s capped nudge (target by serial; DHCP moves the IP)
./.venv/bin/python scripts/go2_connect.py --serial B42D1000AIRXXXXX
./.venv/bin/python scripts/go2_connect.py --serial B42D1000AIRXXXXX --move forward
./.venv/bin/python scripts/go2_connect.py --serial B42D1000AIRXXXXX --damp
```

## Two-dog leader–follower teleop

`go2_leader_follower.py` drives the **leader** (GO2 Pro, `B42D2000PROXXXXX`)
live from the keyboard; the **follower** (the original Air, `B42D1000AIRXXXXX`)
replays the leader's exact command history `--delay` seconds later — same
velocities, same tricks, same order — tracing the same route a few seconds
behind. Arrows move (Shift = fast), number keys fire tricks, `g` damps both,
`q`/Esc quits (stops + damps).

```bash
cd ~/NervusOS/robots
./.venv/bin/python scripts/go2_connect.py --discover     # both dogs on the LAN?
./.venv/bin/python scripts/go2_leader_follower.py        # defaults to this rig
./.venv/bin/python scripts/go2_leader_follower.py --delay 0   # lockstep, no trailing
```

It is **command-replay, not spatial path-tracking**: two Air/Pro units on Wi-Fi
share no localization frame, so the follower reproduces the leader's dead-reckoned
`(vx,vy,vyaw)` stream from an aligned start. Line the follower up behind the
leader, same heading. Drift accumulates over long runs — fine for a demo. This
is the WebRTC stand-in for "DimOS keyboard teleop"; DimOS's own `unitree_go2`
adapter is DDS (wired `192.168.123.x` / EDU) and cannot drive two Wi-Fi dogs.

## PiPER J6 zero — from macOS, no Windows GUI

A new PiPER **must** have its J6 zero set after the flange is installed
(manual §3.3). AgileX only documents this through the Windows-only ArmRobotTool
GUI. `arm_set_zero_j6.py` does the same thing over CAN with pyAgxArm —
`disable(6) → align by hand → calibrate_joint(6) → enable(6)` — and only reports
success when the arm confirms `is_set_zero_successfully`.

```bash
./.venv/bin/python scripts/arm_connect.py piper      # 1. confirm it answers
./.venv/bin/python scripts/arm_set_zero_j6.py        # 2. interactive J6 zero
```

It is a required, **reversible** calibration (re-run any time), not a hardware
modification — no cut wires, no added parts. The GUI's "slave arm mode" step is a
teach-pendant concept and does not apply when driving over CAN. Physical prep,
alignment (flange groove up, three-point line with the rear screw hole + teach
button), and safety are all in the script's docstring.

## Facts that cost time to establish

**One SDK drives both arms.** `pyAgxArm` covers `nero`, `piper`, `piper_h/l/x`
plus gripper and Revo2 hand. `robot.joint_nums` absorbs the 7-vs-6 DOF
difference. The older per-arm `piper_sdk` still works but is not the path here.

**pyAgxArm's high-level API is already SI** — radians, m, rad/s. That is exactly
what DimOS requires, so the NERO adapter does no unit scaling. Do not confuse
this with raw `piper_sdk`, whose CAN layer speaks millidegrees.

**macOS is supported, via `slcan`.** SocketCAN is Linux-only and `agx_cando` is
Windows-only. Per-platform matrix:

| OS | `interface=` | `channel=` |
|---|---|---|
| macOS | `slcan` | `/dev/tty.usbmodem*` |
| Linux (RK3588 / RDK S100) | `socketcan` | `can0` |
| Windows | `agx_cando` | `"0"` |

⚠️ AgileX documents the macOS channel as `/dev/ttyACM0`, which is *Linux*
naming. On macOS it enumerates as `/dev/tty.usbmodem*`; `arm_connect.py`
auto-detects this. **Your adapter's firmware decides whether macOS works at
all** — you need SLCAN firmware here, and gs_usb/candleLight for the Linux
boards. A CANable can be reflashed between the two.

**Both arms run CAN2.0B at 1 Mbps**, so they cannot share a bus. One USB-CAN
adapter per arm.

**GO2 Air needs WebRTC, not DDS.** WebRTC works on consumer units with no
jailbreak (this is the path DimOS uses); the DDS path in `unitree_sdk2_python`
generally wants a wired `192.168.123.x` link and an EDU unit.

**`Move` is fire-and-forget on both the GO2 and the TRACER** — it must be
streamed at ~10 Hz. Stop streaming and the robot stops. That is a safety
feature, not a bug, and the scripts rely on it.

## DimOS + NERO

DimOS ships adapters for `a1z, a750, openarm, openyam, piper, xarm` — **no
NERO**. `dimos_ext/nero/` fills that gap and is verified against the real
`ManipulatorAdapter` protocol:

```bash
./dimos_ext/install.sh ~/dimos      # symlinks into the dimos tree
cd ~/dimos && ./.venv/bin/python -c \
  "from dimos.hardware.manipulators.spec import ManipulatorAdapter
from dimos.hardware.manipulators.nero.adapter import NeroAdapter
print(isinstance(NeroAdapter(address='can0'), ManipulatorAdapter))"   # -> True
```

It is a **symlink**, so edits here take effect immediately and a `git pull`
inside `dimos` cannot clobber the work. `pyAgxArm` has been installed into
dimos's own venv, which the adapter needs at runtime.

Implemented: full lifecycle, joint position/velocity read+write, Cartesian
pose via `move_p`, limits (read from the arm, falling back to the conservative
web-UI table in manual §6.8.1), enable/disable, stop, error clear.
Not implemented: gripper (needs the AgxGripper effector wired through
`init_effector()`) and force/torque (NERO has no F/T sensor) — both correctly
return `None`/`False` as the protocol allows.

## Known blockers

**`dimos go2tool connect-wifi` fails on newer GO2 firmware.** Symptom: BLE scan
finds the dog, then `handshake` → bare `TimeoutError`, identically on every
retry. Confirmed cause (dog `B42D2000NEWXXXXX`): GATT is fine — service `ffe0`,
notify `ffe1` (handle 12), write `ffe2` (handle 17) all present, and the
handshake write is ACK'd — but the robot never notifies back, i.e. it rejects
the AES key / `b"unitree"` payload dimos hardcodes. Not fixable by retrying or
power-cycling. **Provision those units with the Unitree phone app**, then every
downstream step works unchanged. Diagnose with:

```bash
~/dimos/.venv/bin/python scripts/go2_ble_probe.py                  # list GO2s
~/dimos/.venv/bin/python scripts/go2_ble_probe.py <BLE-ADDRESS>    # dump GATT
```


**The VPN breaks everything networked.** There are 13 `utun` interfaces with the
default route on `utun6` (`10.72.12.25`). Consequences:
- `10.88.15.23` (the GO2) is unreachable; port scans return false positives on
  *every* port, including 31337.
- DimOS's web viewer crashes: `TimeoutError: RPC call to
  'RerunWebSocketServer/set_transport' timed out after 120.0s`, because Zenoh
  peer discovery fans out across all 13 tunnels and never converges.

**Disconnect the VPN before any hardware work.** Then:

```bash
cd ~/dimos && source .venv/bin/activate
export ROBOT_IP=10.88.15.23
dimos --rerun-open web run unitree-go2        # browser UI at :7779
```

DimOS's native viewer is the default and works; the web UI only exists with
`--rerun-open web`. If Zenoh still misbehaves on macOS, `--transport=lcm` is the
documented fallback.

## Not yet done

- Nothing here has touched real hardware. No CAN adapter has been connected and
  no arm or dog has been driven. Every claim above about *behaviour* is from
  source and documentation, not observation.
- No NervusOS Provider integration yet — these are standalone scripts, not yet
  wired to the bus/arbiter.
- The GO2 sport API includes `BackFlip`, `HandStand`, `FrontPounce` and friends.
  Before any third-party app touches this layer, those need gating behind the
  permission model; they are genuinely dangerous indoors.
