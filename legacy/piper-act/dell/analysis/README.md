# analysis/ — investigation scripts

One-off scripts written while debugging recording and real-arm execution (2026-09-12 to 09-16). They produced the
evidence in `docs/` and the Mac↔Dell handoff log. They're kept as a record and for reuse, not as maintained tools.
Most hard-code Dell paths (`D:\Piper-CAN-Teleop`, `C:\Users\zhy10\Shared\Piper Arm`). Several write snapshot JPGs
next to themselves; these are gitignored.

Anything marked *arm* opens the CAN bus. Unless noted, it is **read-only**: no torque, no motion.

## Dataset audits (`datasets/piper_pick_place`)

| Script | Purpose |
|---|---|
| `verify_ds.py`, `verify2.py` | Prove a portal-recorded dataset (single camera / two cameras) is trainable by ACT: features, `action[t] = state[t+1]`, video decode. |
| `verify_real.py` | Audit real episodes: motion range, gripper sequence, video health, action shift, traces. |
| `analyse50.py` | Phase analysis of all 50 demos from the Cartesian traces: grasp/release timing, hold width, search swing, transport, lift. |
| `gripseq.py` | Gripper open/close sequence for a hard-coded subset: the flagged episodes plus three reference ones. |
| `idle.py` | Frames sitting still before motion starts, per episode. |
| `dupcheck.py` | Were any episodes captured while the camera had collapsed to ~1 fps? |
| `inspect_ds.py`, `train_frames.py` | Dataset metadata, and a contact sheet of wrist frames plus the median first state. |
| `align.py`, `fkcheck.py` | Trace sidecars line up with dataset frames; our FK matches the arm's onboard end pose (*arm*). |

## Policy and execution

| Script | Purpose |
|---|---|
| `openloop_eval.py` | Open-loop error of a checkpoint on recorded demos through the portal's own `Policy` class, versus a "hold still" baseline and a BGR-fed control. |
| `start_freeze.py` | Does the model plan to leave the rest pose? Training start frames versus a live frame. Run `live_preflight.py` first; it writes the `preflight_live.jpg` this script reads. |
| `live_preflight.py` | Live arm + camera → policy, printing what it **would** command (*arm*, nothing is sent). |
| `bench.py`, `bench2.py` | ACT CPU inference latency versus torch threads, CPU affinity and priority. |
| `run_log.py`, `run_diag.py`, `grip_diag2.py`, `grip_ts.py` | Real-run `steps.jsonl` analysis: tracking, plan-switch jitter, gripper flicker and vibration, loop timing, per-step time series. Rows with no command sent (stale holds, human correction) are skipped. |
| `intervention_diag.py` | Which rollout packages are READY and contain human-correction segments. |

## Hardware probes

| Script | Purpose |
|---|---|
| `hw_probe.py`, `status_probe.py` | CAN frame rate, ctrl_mode/status freshness, joint feedback age, camera FOURCC/fps (*arm*). |
| `probe_enable.py`, `probe_ctrl.py` | Pinpointed the unified portal's `'NoneType' object is not callable` on Enable (*arm*). |
| `isolate_rx.py` | Which step of the shim's open sequence kills RX (*arm*). |
| `cam_bandwidth.py`, `cam_light.py`, `c270_deep.py` | Logi C270 1 fps / black-frame investigation: USB bandwidth versus exposure. |

## Portal tests

These enable motors and run a policy, so each one **refuses to continue unless the portal reports `--mock-arm`**.

| Script | Purpose |
|---|---|
| `test_act_portal.py` | Full smoke test of the unified portal (mock portal on `:8790`): policy flow, correction, mark, and recorder flow. |
| `test_correct.py` | The explicit correction trigger, against a mock policy portal on `--port 8792`. |
| `drive_portal.py PORT dry\|ready` | Drives a portal over HTTP like the page does. `dry` runs the policy (mock only); `ready` only connects, loads and runs Check scene, so it is safe on the real portal. |
| `test_gripper.py` | **Offline, no portal needed.** Command clamp, gripper grasp latch and plan ensembling against simulated grippers. It prints rather than asserts. At the current default `gripper_squeeze` (2) the latch does not engage in its "plan 3 units wider than the cube" scenario; at the earlier default (5) it does. |
