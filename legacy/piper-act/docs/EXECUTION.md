# EXECUTION — running the trained ACT policy on the PiPER (Dell)

How to take a checkpoint from `policies/`, run it on the real arm from one local web page, and evaluate it.
Written 2026-09-14 (Mac). Everything marked **measured** was run, not assumed; everything marked **verify on
the Dell** could not be tested without the arm and says so.

> Read order: this file → `TRAINING INSTRUCTIONS.md` §9–§10 (evaluation, diagnosis) → `README.md` for any hardware
> question. New facts go in `UPDATE.md`.

> **[DELL] 2026-09-14: verified on the real arm, and the portal was changed.** Details and evidence are in UPDATE.md
> `[DELL] 2026-09-14`. What changes how you use it:
> - **Enable works straight from drag-teach.** Leave the teach light on. Enable holds the pose measured *after*
>   enabling and switches the arm to CAN control (README §4). With the light off the arm goes to STANDBY and stops
>   streaming, and the portal then refuses to command anything.
> - **New "Scene check" (no motion).** It runs the model on the live view and says whether it would move. At the rest
>   pose the demos' own frames score about 57; a daylight frame of the changed room scored 2–8 ("would sit still").
>   **Match the recording light and scene before judging a checkpoint.**
> - **Dell inference is 600–700 ms sustained with the arm connected**, not 200–350 ms (§8). Inference now runs in its
>   own process.
> - **New `gripper_squeeze` parameter (default 5).** Drag-taught grasps record the cube's width as the target, which
>   means zero grip force (§7).
> - `crossfade` default is now 4.
> - `/api/shutdown` no longer hard-kills on Windows.
> - Fixed: the Space key could restart a run.
> - Fixed: feedback that never arrived counted as fresh.
>
> **[DELL] 2026-09-14 19:30: second round, after the first real grasp attempts** (UPDATE `[DELL] 2026-09-14 19:40`):
> - **`step_limit_x` now limits the change of the *command***, no longer the distance from the *measured* position.
>   Measuring from the lagging gripper had pinned its command to measured+20 (slow, half-open, open/close flicker).
> - New `lead_limit_x` 4: how far a joint command may run ahead of the arm.
> - New `ensemble` 2: joints average the two newest overlapping plans; the gripper takes the newest plan.
> - New gripper grasp latch, `gripper_release_steps` 8: once the fingers stop on the cube, stale "open" or "close to 0"
>   plans are ignored until "open" is asked 8 steps in a row.
> - `gripper_effort` default 1000 → **500** (1000 crushed a printed cube).
> - New `gripper_open_raw` 95000: "fully open" really opens about 95 mm instead of the 68 mm calibration cap. The grasp
>   range is unchanged.
> - New `record_rollouts` and `intervene_on_teach`: see `AUTOMATED POST-TRAINING.md`. Pressing the drag-teach button
>   mid-run now records a hand correction instead of stopping.
> - Dry runs log to `%TEMP%`, never to `deploy\runs`.
> - `policies/CURRENT.json` is preselected in the checkpoint list.

---

## 0. TL;DR

1. Syncthing delivers the models to `C:\Users\zhy10\Shared\Piper Arm\policies\act_pick_place_v1_<step>\`
   and the program to `...\Piper Arm\deploy\`.
2. Double-click **`deploy\PiPER Policy (dry run).bat`** once: simulated arm and camera, no hardware. Walk the whole
   sequence to prove the software works on the Dell and to **read the inference latency** it reports.
3. Close the recorder and teleop servers (they own the CAN adapter), press the drag-teach button until its light is
   **off**, then double-click **`deploy\PiPER Policy.bat`**. The page opens at `http://127.0.0.1:8791`.
4. On the page: **Connect → Load policy → Enable motors → Start pose → Run** (Space). Stop with Space/Esc. Mark
   **Success/Fail** and the case. Hand on the physical e-stop, every run.

Nothing new has to be installed on the Dell: the portal uses only what the recorder already needs (§4).

---

## 1. What training hands you

| Item | Value |
|---|---|
| Checkpoints | `act_pick_place_v1_010000` … `_100000` (every 10K steps), each 197 MB |
| Files per checkpoint | `config.json`, `model.safetensors`, `policy_preprocessor*.json/.safetensors` (input normalization), `policy_postprocessor*` (output un-normalization), `train_config.json` |
| Model | ACT, 51.6M parameters, ResNet-18 wrist-image encoder + transformer, CVAE (off at inference) |
| Inputs | wrist RGB image 480×640 + 7 joint positions (`joint1…joint6, gripper`, plugin-normalized) |
| Output | **a chunk of 30 future joint-position targets** (2.0 s at 15 fps) |
| Final training loss | ~0.07 (from 6.8); see the training portal for the curves |

The normalization statistics live inside the checkpoint, so a checkpoint folder is self-contained. **Loss does not
pick the checkpoint — the robot does** (§9).

---

## 2. How a trained policy actually runs

```
every 1/15 s  ┌─────────── control loop (15 Hz) ───────────┐
              │ read 7 joints  +  newest wrist frame (RGB)  │
              │ every `replan_every` steps:                 │
              │   hand (joints, image) to the policy thread │──┐
              │ take this step's target from the plan       │  │ policy thread (CPU)
              │ clamp: demonstrated range, per-step limit   │  │  normalize → ACT forward → 30 targets
              │ JointCtrl + GripperCtrl                     │  │  un-normalize → plan[s .. s+29]
              └─────────────────────────────────────────────┘◄─┘
```

- **What the numbers mean.** Training used `action[t] = state[t+1]`: "the joint position to be at next frame". So
  each output is an absolute joint target, sent with `JointCtrl` (MOVE J) at 15 Hz — the same rate as the data.
- **Why a separate policy thread.** One ACT forward pass is too slow for a 66 ms control period on a laptop CPU
  (§8). A synchronous loop would freeze the arm for a few frames at every re-plan. Instead the portal asks for a new
  plan in the background and keeps executing the old one. When the new plan arrives, the targets for frames that
  already passed are **skipped** ("stale actions skipped" on the page) and the rest replace the old plan, blended
  over `crossfade` steps. This is the same idea as LeRobot's own async inference server.
- **Name mapping (a real trap).** The dataset calls the joints `joint1 … gripper`; the plugin reports `joint1.pos
  … gripper.pos`. The stock LeRobot runner fails on this with `KeyError: 'joint1'` (**measured**). The portal maps
  the names itself.
- **Colour order (a silent trap).** The dataset frames are **RGB** (**measured**: the red card is red in the
  training video, 173,636 strongly-red pixels vs 0 blue). OpenCV captures BGR; the portal converts. Feeding BGR
  would show the policy a blue card and it would fail without any error.

---

## 3. Randomness and seeds — there are none at execution time

**ACT inference is deterministic.** Same image + same joints → the exact same 30 targets, every time.

| Where randomness exists | Training | Execution |
|---|---|---|
| CVAE latent `z` | sampled from the encoder (`modeling_act.py:449`) | **replaced by zeros** (`modeling_act.py:454`) |
| Dropout (0.1) | on | **off** (`model.eval()`) |
| Image augmentation (colour/sharpness jitter) | on, random per frame | **not applied** |
| Data shuffling, `seed=1000` | yes | not used |

**Measured on checkpoint 090000:** the same input with `torch.manual_seed(1234)` and then `(2468)` → maximum
difference **0.0**. There is no seed to set or tune.

Run-to-run differences on the real robot therefore come only from the physical world: where the block is, light,
camera noise, exactly when you press Run, and how the arm tracks. That is why evaluation needs **repeated trials
with the block placed deliberately** (§9), not a seed.

---

## 4. Dell infrastructure

### Already present (from recording — verify, don't reinstall)

| Needed | Where | Check |
|---|---|---|
| `lerobot==0.4.4`, `torch` (CPU) | `D:\Piper-CAN-Teleop\venv` | `python -c "import lerobot,torch;print(lerobot.__version__,torch.__version__)"` → `0.4.4 2.10.0` |
| PiPER plugin | `D:\Piper-CAN-Teleop\lerobot_robot_piper` (editable) | `python -c "import lerobot_robot_piper"` |
| CAN shim | `D:\Piper-CAN-Teleop\scripts\piper_mac_can.py` | installed by the plugin's own `__init__` bootstrap on import; the portal only sets `PIPER_SHIM_DIR` (default above) and never calls `install()` a second time |
| `opencv-python`, `pygrabber` | venv | camera opened **by name** (`--camera-name "Dabai DC1"`) |
| `av` (PyAV) | venv | only for the dry run (reads the dataset video) |
| Dataset metadata | `Shared\Piper Arm\datasets\piper_pick_place\meta` | the portal reads the demonstrated joint range from it |

### Delivered by Syncthing (new)

```
Piper Arm\
  policies\act_pick_place_v1_010000 … _100000\     ← checkpoints (pretrained_model only)
  deploy\
    piper_policy_portal.py                          ← the execution portal (one file, no extra dependencies)
    PiPER Policy.bat                                ← real arm; copy a shortcut to the Desktop
    PiPER Policy (dry run).bat                      ← simulated arm + dataset video as camera
    runs\<time>_<checkpoint>\steps.jsonl            ← written by the portal: every step of every run
    runs\results.csv                                ← your Success/Fail marks, one row per trial
```

⚠ `deploy\runs\` is inside the synced folder, so results appear on the Mac too. That is intended.

### Before the first real run (one-time, on the Dell)

1. Run the **dry-run** launcher and complete Connect → Load → Enable → Start pose → Run → Stop → Success. Note the
   **Inference** number (§8).
2. Open `piper_policy_portal.py` → class `RealArm` and compare it with your recorder's proven connection code.
   Three things are **verify on the Dell**:
   - feedback age uses `GetArmJointMsgs().time_stamp`; if that attribute is missing the age shows 0 and the stale
     check is blind. Your recorder's `rx_count` frames/s check is better — swap it in.
   - `GetArmStatus().arm_status.ctrl_mode` is the field for drag-teach detection (0x02).
   - `EmergencyStop(0x01)` behaviour on this firmware (README §4 warns about `0x02`).
3. Keep both the recorder (`:8790`) and teleop (`:8770`) servers closed. They share the one CAN adapter
   (UPDATE 09-12 §1).

---

## 5. Run procedure (real arm)

| # | Do | What the portal does | Watch |
|---|---|---|---|
| 0 | Arm powered, drag-teach light **ON** [DELL: streaming guaranteed], e-stop within reach, scene as in training: black table, red card **left**, white block **right** of the start heading, **same lamp light as the recording session (evening), nothing new in view** | — | — |
| 1 | **Connect** | opens CAN only — **no torque, no movement** (it deliberately does *not* call `robot.connect()`, which enables and drives home) | Feedback in ms (not "NONE / STALE"), Mode DRAG-TEACH; camera ≥ 10 fps |
| 2 | Pick a checkpoint → **Load policy** | loads in a separate process, runs 3 warm-up inferences | "one inference ≈ N ms" |
| 2b [DELL] | **Check scene** (tick "repeat" while adjusting) | one inference on the live view; **sends nothing** | score ≥ 20 "plans to move". Below 8: fix light/scene first, or drag the arm up to look at the table and check again |
| 3 | **Enable motors** | `EnablePiper`, waits 1.2 s, then holds the pose **measured after enabling** and switches to CAN control (leaves drag-teach) | arm stiffens, no motion, Mode → CAN control |
| 4 | **Start pose** | moves over 4 s at 20 % speed to the median first pose of the 50 demos (rest pose, forearm forward, gripper closed) | clear path |
| 5 | Place the block | — | case A (visible once raised) or B (out of view, right) |
| 6 | **Run** / Space | 15 Hz policy control | hand on e-stop |
| 7 | **Stop** / Space / Esc | freezes at the measured pose, torque stays on | — |
| 8 | Choose case → **Success** or **Fail** (+ note) | appends to `runs\results.csv` | — |
| 9 | Start pose → next trial | — | — |

A run also stops by itself on: max duration, camera frame too old, joint feedback too old, the page closing
(heartbeat), an arm hard fault, or the arm entering drag-teach mode. The reason appears on the page and in
`summary.json`.

Finishing the session: **Start pose**, then close the console window. The portal **never** releases the motors on
exit (the plugin's `disconnect()` would park and drop). To power down, support the arm, or park it with your
existing tools.

---

## 6. The page

- **Sequence** buttons unlock in order; greyed out means a precondition is missing (the message line says which).
- **Arm / Camera / Policy / Current run** panels: live health. Red values are the ones that will block or stop a
  run.
- **Joints**: black tick = measured, orange tick = target just sent, grey band = range seen in the 50 demos. A
  target pinned at the edge of the band means the policy wants to go somewhere it was never shown.
- **Runtime parameters**: edit and press Enter; applies immediately, including mid-run.
- **Recent results**: the last 50 rows of `results.csv`.

Logs per run in `deploy\runs\<id>\`: `steps.jsonl` (per step: measured joints, raw model output, clamped target,
camera and feedback age, which observation the plan came from), `summary.json` (params, stop reason, latency),
`result.json` (your mark).

---

## 7. Parameters

### Tunable at execution time (page or `results.csv` records them)

| Parameter | Default | Range | What it does | Change it when |
|---|---|---|---|---|
| **checkpoint** | newest | 10K … 100K | which trained weights | always compare several (§9) |
| `replan_every` | 10 | 1–30 | frames between new plans (the ACT `n_action_steps` idea). Lower = reacts sooner to what the camera sees, more CPU | search (case B) fails to notice the block → lower it (5); CPU pegged / jerky → raise it (15). Below ~ latency/66 ms (e.g. 4 at 250 ms) gives no extra benefit |
| `crossfade` | 4 [DELL; was 3] | 0–10 | frames blended when a new plan takes over | visible twitch every re-plan → 5; sluggish reaction → 0–2 |
| `speed_pct` | 40 | 10–100 | firmware MOVE J speed cap. **The plugin hard-codes 30** | arm visibly lags the orange target ticks → raise (60–70); overshoot/oscillation → lower |
| `step_limit_x` | 1.5 | 0–5 (0 = off) | caps each joint's per-frame change at this × the demos' 99th percentile (joint1 3.7, joint2 6.5, joint3 6.8, joint4 4.8, joint5 11.4, joint6 4.8, gripper 16.7 units) | first runs: 1.0; motion clipped too much (moves slower than demos) → 2.0 |
| `envelope_margin` | 5 | 0–30 | targets clamped to the demonstrated joint range ± this | never raise for real runs without watching closely |
| `gripper_effort` | 1000 | 200–5000 | `GripperCtrl` force (0.001 N·m) | block slips → 1500–2500 |
| `gripper_squeeze` [DELL] | 5 | 0–30 | gripper is commanded this many units tighter than the policy's target. The drag-taught demos recorded the cube's width while it was held (raw 52,000–53,600), so the unmodified target asks for zero grip force. The policy still sees the measured width. Also applied to the hold after Stop. | cube slips during the lift → 8–12, together with a higher `gripper_effort` |
| `max_duration_s` | 90 | 5–600 | automatic stop | longest demo was 55 s |
| `stale_camera_ms` | 500 | 100–3000 | stop if the newest frame is older | — |
| `stale_arm_ms` | 300 | 100–3000 | stop if joint feedback is older | — |
| `heartbeat_s` | 2.0 | 0.5–10 | stop if the page stops talking | — |

### Fixed — must match training, do **not** change

| Thing | Value | Why |
|---|---|---|
| Control rate | **15 Hz** | one output step = 1/15 s; running faster or slower changes the motion speed the model learned |
| Camera | wrist Dabai, 640×480, **RGB** | the model has only ever seen this view and colour order |
| Joint order and normalization | plugin `joint1…joint6, gripper`, joints −100…100, gripper 0…100 | the checkpoint's normalizer assumes it |
| Gripper calibration | `0…68,000` raw (UPDATE 09-13, item 3) | changing it re-scales every gripper value |
| Scene layout | card left, block right, black table, same light | never seen anything else (TRAINING INSTRUCTIONS §9.5) |
| Chunk size | 30 (baked into the weights) | changing needs retraining |
| Temporal ensembling | not offered | needs one inference per frame (66 ms); the Dell cannot (§8) |

---

## 8. Measured performance and what to expect on the Dell

Mac, **CPU only, 4 threads** (a stand-in for the Dell's 4-core i7-8650U), checkpoint 090000:

| Measurement | Value |
|---|---|
| one ACT forward pass | 85–133 ms (max 217 ms under load) |
| queue pop without network | ~1 ms |
| dry run, 293 steps / 19.6 s | **14.9 Hz**, mean period 66.4 ms, worst 76.6 ms, 0 starved steps, 1 overrun |
| stale actions skipped per plan | 2.2 (≈ latency / 66 ms) |
| page heartbeat lost | run stopped 2.0 s later, arm held position |

**[DELL] Measured on the i7-8650U, checkpoint 100000:**

| Condition | Inference |
|---|---|
| cold laptop, nothing else running (offline replay of 68 demo frames) | median 354 ms, p90 388 ms |
| portal dry run (mock arm + mock camera), 25 s | 470–490 ms; 15.0 Hz, 0 starved, 0 overruns, ~7.5 stale steps skipped |
| real arm connected (portal process ~80 % of a core parsing ~2,330 CAN frames/s) + live camera | **~700 ms**, same in-process or in a separate process, and for 3/4/6/8 torch threads |
| warm laptop after ~1 h of load, portal stopped | 545–575 ms |

So the sustained operating point is **600–700 ms** (thermally limited 15 W CPU). Each plan executes steps ~11–21 of
the 30-step chunk, which is still inside the chunk; ACT's own default runs whole chunks open-loop. Keep the laptop on the
charger with free airflow and other programs closed.

~~Dell estimate: 200–350 ms per inference~~ (Mac estimate, superseded above) (roughly 2–3× slower than the M2 Pro's cores). That means 3–5 targets
skipped per plan and a plan age of ~⅓ s — acceptable for this task. Read the real number from the dry run:

| Dell inference | Recommendation |
|---|---|
| < 300 ms | defaults |
| 300–600 ms | `replan_every` 10–15, `crossfade` 4, close other programs |
| > 600 ms or the loop reports overruns | Plan B (§12) |

---

## 9. Evaluation — choosing the checkpoint

Loss kept falling to 100K, but for 50 demonstrations the best robot behaviour is often at an earlier checkpoint
(later ones can over-fit the exact demo trajectories).

1. Start with **050000, 080000, 100000**. Add 030000 or 070000 if the results differ a lot.
2. For each: **10 trials case A** (block visible when the arm rises) and **10 trials case B** (block out of view
   to the right). Same parameters for all checkpoints.
3. Success = cube fully on the card **and** released.
4. `runs\results.csv` then holds everything; score A and B separately — they fail for different reasons
   (TRAINING INSTRUCTIONS §10).
5. Only after picking a checkpoint, tune `replan_every` / `speed_pct` / `crossfade` on it.

Log a `[DELL] … DONE` entry in `UPDATE.md` with the table.

---

## 10. Safety

- **Physical e-stop in hand for every run.** The page's button is software and depends on CAN working.
- The portal never calls the plugin's `connect()` (enables torque **and drives home**) or `disconnect()` (parks,
  then **releases the motors**).
- Drag-teach mode ignores all commands (README §4). The portal refuses to enable or run while it is detected, and
  stops a run if it appears.
- Three independent motion limits: demonstrated joint envelope, per-step change cap, firmware speed cap.
- Soft faults (`0x02/0x03/0x04`) do not stop the run, matching README §4; hard faults do.
- First real runs: `step_limit_x` 1.0, `speed_pct` 30, block in an easy case-A position.

---

## 11. Troubleshooting

| Symptom | Likely cause | Fix |
|---|---|---|
| Arm does not move after Run, targets change on the page | still in drag-teach, or motors not enabled | press teach button (light off), Enable |
| Freezes at the start, targets barely change | 18 demos wait >2 s before moving (TRAINING INSTRUCTIONS §2) | nudge block/lighting; try another checkpoint; data fix per §10 |
| Twitch every ~0.7 s | plan switch | `crossfade` 5 |
| Orange ticks run ahead of black ticks, arm lags | speed cap | `speed_pct` 60–70 |
| Moves slower than the demos | step limit clipping | `step_limit_x` 2.0 |
| Reaches the block, misses the grasp | wrist view occluded at grasp; single camera | expected weakness; slow grasp demos / scene camera later |
| Case B: never turns right | too few search demos | lower `replan_every`; then record 10–20 search demos |
| "camera at N fps (< 5)" | CPU-starved or the FK problem again (UPDATE 09-13, item 5) | close other apps; do not enable SDK FK |
| "arm feedback N ms old" | CAN wedged / other process owns adapter | close recorder/teleop; README §9 |
| Load policy fails with a path error | Syncthing still copying | wait until Up to Date; half-copied folders are hidden |

---

## 12. Plan B — run the model on the Mac GPU

If the Dell CPU is too slow, LeRobot 0.4.4 ships `lerobot.async_inference` (`policy_server.py` +
`robot_client.py`, gRPC): the Mac runs the policy on MPS (a forward pass is a few ms) and the Dell streams
observations and executes returned chunks. It would need the robot client wired to the CAN shim and the name
mapping from §2, and a stable LAN path between the machines. Not built; only worth it if §8's table says so.

---

## 13. Status

| Item | State |
|---|---|
| Portal logic, async planner, clamps, logging, results, heartbeat stop | ✅ tested on the Mac in dry-run mode with real checkpoint 090000 |
| Inference API, determinism, name mapping, colour order | ✅ measured |
| `RealArm` against the physical PiPER | ✅ [DELL] §4 checklist done. `time_stamp` exists (the shim's `time.time()` per frame) and now reads ∞ when never received. `ctrl_mode`/`arm_status` are IntEnum (`int()` safe) and are ignored when stale. Connect, read, and the live scene check ran on the arm with no motion. **Enable / Start pose / Run on the arm: not yet run** (they need the operator at the e-stop) |
| Dell inference latency | ✅ [DELL] 600–700 ms sustained with the arm connected (§8) |
| Offline sanity of checkpoint 100000 on the Dell | ✅ [DELL] 68 demo frames through the portal's own `Policy`: mean 30-step error 1.79 vs 6.09 for "hold still" (explains 71 % of the motion); fed BGR → 2.43 (colour order matters, RGB confirmed) |
| Scene readiness | ⚠ [DELL] daylight 09-14, changed room: scene check 2–8 at the rest pose vs ~57 on the demos' frames → match the recording light before the §9 trials |
| Checkpoint choice | ⏳ user chose **100000** first; §9 trials pending |
