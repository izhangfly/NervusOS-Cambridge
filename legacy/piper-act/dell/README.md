# dell/ — the Windows laptop at the arm

Everything that runs on the Dell: recording demonstrations, running policies on the real PiPER, capturing
corrections, and the CAN plumbing underneath. Training happens on the Mac (`../tools/`).

## Layout

| Path | On the Dell | What it is |
|---|---|---|
| `scripts/piper_act_portal.py` | `D:\Piper-CAN-Teleop\scripts\` | **Unified portal** on `http://127.0.0.1:8790`: dashboard `/`, data collection `/record`, policy execution `/policy`. One process owns the CAN adapter and the cameras, so recording and running can't fight over the single candleLight. It has its own copies of the policy engine (`Policy`, `Planner`, `Controller`, `RolloutRecorder`) and recorder. |
| `piper_policy_portal.py` | `Shared\Piper Arm\deploy\` (Syncthing) | The **standalone** policy portal on `:8791`, started by `PiPER Policy.bat` / `PiPER Policy (dry run).bat`. The unified portal reuses only its page template (`PAGE`); `analysis/` imports its `RealArm`, `Camera`, `Policy` and `Controller` classes. |
| `scripts/piper_record_portal.py` | `D:\Piper-CAN-Teleop\scripts\` | Drag-teach recorder (`:8790` when run alone). The unified portal reuses its page template; it still runs standalone as a fallback. |
| `scripts/piper_mac_can.py` | `D:\Piper-CAN-Teleop\scripts\` | CAN shim for the candleLight `gs_usb` adapter on Windows and macOS. It replaces python-can's SocketCAN bus with a single-I/O-thread bus. |
| `plugin-patch/` | `D:\Piper-CAN-Teleop\lerobot_robot_piper\` | The one file changed in the LeRobot PiPER plugin, which installs the shim. Apache-2.0; see its README. |
| `scripts/*.bat` | same | `piper_act_portal.bat` (unified portal), `piper_act_tunnel.bat` (SSH reverse tunnel to the relay), `start_recorder.bat`. |
| `scripts/piper_cartesian_ctl.py` | same | The WASD Cartesian teleop (`:8770`, via the shim) this project grew out of. |
| `scripts/piper_webctl.py` | same | An earlier joint-slider panel for the RDK board (Linux SocketCAN `can0`). Reference only; it doesn't run on the Dell. |
| `scripts/piper_*.py`, `camera_*.py` | same | Hardware diagnostics. Some move the arm; see below. |
| `scripts/repair_traces.py` | same | Splits the old cumulative Cartesian trace sidecars back into one file per episode. |
| `vla/camera.py` | `D:\Piper-CAN-Teleop\vla\` | Opens a camera by **name** (DirectShow via pygrabber). The recorder and portal depend on it. |
| `vla/arm.py`, `vla/gpt_driver.py` | same | An earlier vision-language driving experiment. |
| `analysis/` | (ad-hoc) | Investigation scripts behind the debugging notes in `docs/`. See its README. |
| `TELEOP-README.md`, `HANDOFF-MACOS.md`, `setup-windows.bat`, `requirements.txt` | `D:\Piper-CAN-Teleop\` | **Historical** Mac→Windows handoff and setup for the CAN teleop (2026-09-08). Read the note on drivers below. |
| `requirements-portal.txt` | — | Versions in the Dell venv that runs the portals. |

### Which diagnostics touch the arm

These categories come from grepping each script for motion, torque and reset commands.

- **These MOVE the arm** (`JointCtrl` / `EndPoseCtrl`): `piper_accel_test`, `piper_axis_survey`, `piper_clean_enable_test`,
  `piper_exit_teach_test`, `piper_fault_recovery_test`, `piper_full_test`, `piper_home_test`, `piper_mode_diag`,
  `piper_motion_repeat`, `piper_move_diag`, `piper_nudge`, `piper_ready_pose`.
  - **These also end with `DisableArm`, which drops the arm under gravity:** `piper_accel_test`,
    `piper_clean_enable_test`, `piper_exit_teach_test`, `piper_fault_recovery_test`, `piper_full_test`,
    `piper_home_test`, `piper_mode_diag`, `piper_move_diag`.
  - `piper_home_test` and `piper_full_test` drive to joint-zero, which is a kinematic singularity. Use the READY pose
    (`piper_ready_pose`) for real work; see `docs/HARDWARE.md` §4.
  - Support the arm and keep the e-stop in reach.
- **These enable torque or switch mode without a motion target:** `piper_arm_state`, `piper_enable_check`,
  `piper_enable_loop_test` (ends with `DisableArm`), `piper_recover_test` (sends `EmergencyStop`).
- **`piper_tx_recover`** can end with a USB reset, which may leave the adapter needing a replug.
- **The rest send no motion or torque commands:** the bus, link, SDK and firmware probes, `piper_pose_probe`,
  `piper_teach_probe`, the camera scripts, and the `*_reopen_test`, `*_native_test`, `atexit`, `recvthread`, `send`
  and `sdk_noinit` tests. Some send query frames (`0x4AF` firmware, `0x472` motor limits). `piper_send_test` sends
  one raw test frame on `0x101`, which is not a PiPER command ID.

## Environment

- Python 3.12 venv at `D:\Piper-CAN-Teleop\venv`, with the versions in `requirements-portal.txt`.
- **piper_sdk 0.6.1** from AgileX (`pip install piper_sdk==0.6.1`). The Dell's vendored copy at
  `D:\Piper-CAN-Teleop\piper_sdk` was compared with the upstream `0.6.1` tag. It has no code changes: only
  upstream's later README and docstring edits (`'''` → `r'''`). So `setup-windows.bat`'s `pip install -e piper_sdk`
  can be replaced by the PyPI package.
- The PiPER plugin: upstream WeGo-Robotics commit + `plugin-patch/`, installed editable.
- **Driver:** the working Dell binds the candleLight to **libusb-win32 (libusb0)** with Zadig, and `piper_mac_can.py`
  assumes libusb0. The historical `setup-windows.bat` and `TELEOP-README.md` say WinUSB, which was the first attempt,
  later replaced. TELEOP-README's "2 ms" read timeout is also out of date: the shim uses `RX_TIMEOUT_MS = 100`.
  `docs/HARDWARE.md` is current.

## Running

```bat
:: 1. the portal (venv + plugin + patch + shim)
scripts\piper_act_portal.bat
:: 2. optional: expose it through the relay (server/relay.py) over an SSH reverse tunnel
scripts\piper_act_tunnel.bat
```

Dry run without hardware: `python scripts\piper_act_portal.py --mock-arm --mock-camera`. Mock runs write the
dataset and rollouts to `%TEMP%`, never to the synced folders. **Off the Dell**, the post-training contract defaults
to the Dell's synced folder, so also pass `--posttrain-dir <folder containing CONTRACT.json>` (`docs/CONTRACT.json`
works), or set the page parameter `record_rollouts` to 0.

## Things to know

- **Paths are absolute and Dell-specific.** The code expects the venv at `D:\Piper-CAN-Teleop\venv` and the
  Syncthing folder at `C:\Users\zhy10\Shared\Piper Arm\` (datasets, policies, posttrain, deploy). On the Dell,
  `piper_policy_portal.py` is imported from that synced `deploy\` folder; in a clone, from `dell/`.
- **The relay server's address is not in the repo.** `piper_act_tunnel.bat` reads `PIPER_ACT_SERVER` from
  `scripts\piper_act_tunnel.local.cmd`, which is gitignored:
  `set PIPER_ACT_SERVER=user@host`.
- **The portal and relay have no authentication.** Anyone who can reach the relay's port can enable the motors and
  run the arm. Keep the physical e-stop in reach, and don't expose the relay publicly without adding auth.
  `load` accepts only checkpoint names the portal lists itself.
- **Safety model:** the portal never calls the plugin's `robot.connect()` (which enables torque and drives home). It
  closes the bus only with `bus.disconnect(disable_torque=False)`; the default `True` would park the arm and then
  disable the motors. Torque comes only from **Enable**, and motion only from **Run** or **Start pose**.
- Data and checkpoints are not in git (see the root `.gitignore`). They live in the synced folder.
