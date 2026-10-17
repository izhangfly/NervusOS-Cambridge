# PiPER calibration & safe-motion procedure

Run this **every time the arm is moved, re-mounted, or its surroundings change.**
Rated joint limits describe the arm in free space — they know nothing about your
table. Calibration is what teaches it where it actually is.

Incident that motivated this (2026-07-24): a J2 sweep to 156° drove the forearm
into the table. Every joint was inside its rated limit. Rated limits are not
safety limits.

---

## Order of operations

| # | Step | Script | Moves? |
|---|---|---|---|
| 0 | Bring up CAN | `sudo ip link set can0 up type can bitrate 1000000` | no |
| 1 | J6 zero point | `piper_j6_zero.py --commit` | no |
| 2 | Verify zero pose | `piper_verify_zero.py --commit` | **yes** |
| 3 | **Workspace calibration** | `piper_workspace_calib.py --commit` | **yes** |
| 4 | Range exercise | `piper_range_test.py --commit` | **yes** |
| 5 | Interactive control | `piper_webctl.py` | **yes** |

Steps 1–2 are per-arm (only after the flange is installed). **Steps 3 onward are
per-location** and must be redone after any move.

---

## Step 1 — J6 zero (once per arm, after flange install)

Physically rotate the flange so its groove points straight up, in line with the
rear screw hole and the teach button (a three-point line, AgileX figure 2). Then:

```bash
cd ~/NervusOS/robots
./.venv/bin/python scripts/piper_j6_zero.py --commit
```

Success = `zero_config_success_flag = 0x01` **and** J6 reading ~0.000°.
No Windows host software needed; this is CAN `0x475` with `set_zero=0xAE`.

## Step 2 — verification 2

```bash
./.venv/bin/python scripts/piper_verify_zero.py --commit
```

Then look at the arm: flange groove still straight up, gripper square, no joint
straining. Numbers alone do not prove this.

## Step 3 — Workspace calibration (THE IMPORTANT ONE)

```bash
./.venv/bin/python scripts/piper_workspace_calib.py --commit --location "lab bench, north wall"
```

Walks each joint outward in 3° steps at 8% speed, watching motor current and
tracking error. On resistance it stops, backs off 6°, and records that as the
safe limit **here**. Writes `workspace_limits.json`.

Re-calibrate one joint after changing something nearby:

```bash
./.venv/bin/python scripts/piper_workspace_calib.py --commit --joint 2
```

Single-joint runs merge into the existing file rather than replacing it.

Inspect current calibration:

```bash
./.venv/bin/python scripts/piper_workspace_calib.py --show
```

### `workspace_limits.json`

```json
{
  "schema": "piper-workspace-limits/v1",
  "calibrated_at": "2026-07-24T21:15:00",
  "location": "lab bench, north wall",
  "joints": {
    "2": {
      "safe_min_deg": 6.0,  "safe_max_deg": 78.0,
      "rated_min_deg": 0.0, "rated_max_deg": 195.0,
      "max_stop_reason": "current 1180 > 900"
    }
  }
}
```

Every motion script reads this file and clamps to `safe_*` when present. Delete
the file to fall back to rated limits — **only safe with a clear workspace.**

## Step 4 — Range exercise

```bash
./.venv/bin/python scripts/piper_range_test.py --commit
```

Now bounded by calibrated limits. `--ignore-workspace` restores the old
rated-limit behaviour (this is what hit the table — don't).

## Step 5 — Web control

```bash
./.venv/bin/python scripts/piper_webctl.py          # on the board
ssh -L 8770:localhost:8770 rdk-persist              # from a laptop
```

Or open `http://localhost:8770` directly on the board's own screen.
Loopback-bound on purpose — never expose robot control to the internet.

---

## Limits of this approach

**Per-joint calibration cannot catch multi-joint collisions.** J2 and J3 may each
be individually safe while their *combination* puts the wrist under the table.
Calibration moves one joint at a time, so it never sees those poses.

The real fix is forward kinematics: `pyAgxArm` ships MDH FK
(`tests/test_mdh_fk.py`), so link positions can be computed for any candidate
pose and rejected if any link would fall below a declared table plane. That is
`piper_fk_guard.py` — the next piece to build.

Until then, treat calibrated per-joint limits as **necessary but not sufficient**,
and keep a hand near the e-stop for any multi-joint motion.

## Emergency

```bash
pkill -f piper_                                              # stops streaming => arm stops
./.venv/bin/python scripts/piper_range_test.py --estop       # CAN 0x150
```

`JointCtrl` is fire-and-forget and must be streamed at ~20 Hz. **Stop streaming
and the arm stops** — killing the script is a valid emergency stop, and is what
actually halted the arm during the table incident.

Clear latched faults before moving again:

```bash
./.venv/bin/python -c "
from piper_sdk import C_PiperInterface_V2 as P; import time
p=P('can0'); p.ConnectPort(); time.sleep(1)
p.JointConfig(joint_num=7, clear_err=0xAE); print('errors cleared')"
```

## Self-check capability

Verified against firmware `S-V1.9-0` on 2026-07-24. `GetArmLowSpdInfoMsgs()`
returns `motor_1..motor_6`, each with:

| field | type | notes |
|---|---|---|
| `bus_current` | int | 0 at idle |
| `vol` | int | bus voltage |
| `motor_temp`, `foc_temp` | int | °C |
| `foc_status_code` | int | `64` == healthy/enabled on this unit |
| `foc_status` | object | named booleans, below |

`foc_status` booleans: `collision_status`, `stall_status`, `driver_error_status`,
`driver_overcurrent`, `driver_overheating`, `motor_overheating`,
`voltage_too_low`, `driver_enable_status`.

**The arm has its own `collision_status` and `stall_status` flags** — so it does
self-detect contact and stalls at the motor level. `piper_workspace_calib.py`
uses these as its primary blocked-detector.

> **Trap:** `driver_enable_status` is `True` during NORMAL operation — it is not
> a fault. And `foc_status` is an object, so `if foc_status:` is *always* true.
> Testing the object instead of its individual flags made the first calibration
> run report every joint as blocked at the first 3° step, and it wrote an
> inverted `min=+6, max=-6` limits file. Always test the named booleans.

**What it still cannot do:** the arm has no model of your table or surroundings.
`collision_status` fires only *after* contact. It is a good backstop, not a
substitute for knowing where obstacles are — which is why step 3 probes slowly
and why FK guarding is still needed for multi-joint poses.
