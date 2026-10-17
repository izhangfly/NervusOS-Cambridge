# PiPER × ACT — Project Handoff

**Status as of 2 October 2026.** This document covers everything from the July hackathon to now: what was built,
on which machine, every decision and why, the data actually used, what went wrong, and how each problem was corrected.
It is written so someone (including future you) can pick the project up cold.

> Public copy. The server address is written as `HETZNER_IP`, and operational security details are omitted.

---

## 0. Summary

| Item | State |
|---|---|
| Task | Pick up a 5 cm white cube and place it on a 15 cm red square, from the wrist camera only |
| Robot | AgileX PiPER (6-DoF + gripper), single arm, won at a hackathon in July 2026 |
| Model | ACT (Action Chunking Transformer) via Hugging Face LeRobot 0.4.4, 51.6 M parameters, chunk 30 |
| Default policy (`CURRENT`) | `act_pick_place_pt1_100000`, promoted 15 Sep on real-arm trials |
| Real-arm success rate | `pt1_100000` 7/17 (41%), vs the original model 2/15 (13%) |
| Waiting for trials | `act_pick_place_pt2_015000` (round 2 candidate) — 0 labelled trials |
| Data | 50 hand demos + 42 accepted rollout episodes (32 hand corrections, 10 clean runs) |
| Untrained data | 8 accepted episodes (3 corrections, 5 clean runs) not yet in any round; 11 runs never labelled |
| Last arm activity | 16 Sep 2026 (one unlabelled run on `pt2_015000`) |
| Running now | Mac post-training watcher + dashboard feed (launchd); training dashboard on `HETZNER_IP:8850` |

---

## 1. What the project is

Teach one robot arm one task by imitation learning: a human demonstrates, a model learns to copy, and the model then
improves from its own mistakes when a human corrects it. ACT was chosen as step one, with SmolVLA (a 450 M
vision-language-action model) as step two once the pipeline is trusted.

### Hardware

| Part | Detail |
|---|---|
| Arm | AgileX PiPER, 6 joints + parallel gripper, CAN 2.0B at 1 Mbit/s |
| CAN adapter | candleLight USB-CAN (bytewerk, `1d50:606f`), bound to **libusb0** with Zadig on Windows |
| Wrist camera | Orbbec Dabai DC1, RGB 640×480 over UVC, ~12.5 fps (exposure-limited at night) |
| Scene camera | Logitech C270 — **not used**: 1–3 fps and black frames over USB; never fixed |
| Robot host | Dell Latitude 7390 (i7-8650U, 15 W, no GPU), Windows 10, project at `D:\Piper-CAN-Teleop` |
| Training host | MacBook Pro M2 Pro, 16 GB, Apple GPU via MPS |
| Server | Hetzner VPS (`HETZNER_IP`): training dashboard, Syncthing relay/discovery, Dell portal relay |

### Scene

Black table, 5 cm white cube, 15 cm × 15 cm red card. Card to the left of the arm, cube to the right of the start
heading. Episodes start from the rest pose (forearm forward, gripper closed).

---

## 2. Architecture

```
            DELL (Windows, at the arm)                           MAC (training)
 ┌──────────────────────────────────────────┐      ┌───────────────────────────────────────────┐
 │ CAN shim (piper_mac_can.py) + WeGo plugin │      │ LeRobot 0.4.4 + torch 2.10 (MPS)          │
 │ Recorder portal  → datasets/<name>/       │      │ train_supervisor.py (crash-resume)        │
 │ Policy portal    → deploy/runs/,          │      │ posttrain pipeline (launchd, every 5 min) │
 │                    posttrain/rollouts/    │      │   scan → convert → trigger → fine-tune    │
 │ Unified portal piper_act_portal.py :8790  │      │   → offline gate → publish → promote      │
 └──────────────────┬───────────────────────┘      └──────────────────┬────────────────────────┘
                    │        ~/Shared/Piper Arm  (Syncthing, two-way) │
                    └──────────────────────┬──────────────────────────┘
                                           │ UPDATE.md (append-only log), CONTRACT.json,
                                           │ datasets/, policies/ + CURRENT.json, posttrain/
                                 ┌─────────┴──────────────────────────────┐
                                 │ HETZNER: act-portal :8850 (dashboard)  │
                                 │          relay.py   :8790 (Dell portal)│
                                 └────────────────────────────────────────┘
```

**The split.** The Mac trains because it has the GPU. The Dell drives the arm because the CAN link and the camera
already worked there. Neither machine does the other's job.

**The glue.** One Syncthing folder (`~/Shared/Piper Arm`, folder ID `mac-dell-shared`) and one append-only file,
`UPDATE.md`, where each side posts entries tagged `ADD / CORRECT / EDIT / DELETE / DONE / ASK / NOTE`. Nobody edits
history. The post-training loop has a machine-readable rulebook, `posttrain/CONTRACT.json` (version 1), that says
which machine may write which paths.

### Ownership (never violated)

| Writer | Paths |
|---|---|
| Dell only | `posttrain/rollouts/**`, `deploy/runs/**` (incl. `results.csv`), new datasets under `datasets/` |
| Mac only | `posttrain/mac/**`, new `policies/<name>/`, `policies/CURRENT.json` |
| Both read-only | `posttrain/CONTRACT.json`, `datasets/piper_pick_place/**` |

---

## 3. Timeline

| Date (2026) | What happened |
|---|---|
| July | PiPER won at a hackathon. |
| ~7 Sep | The previous CAN host (RDK S100 board) was gone; the Dell became the robot host. |
| 10 Sep | RDT-1B investigated and abandoned the same day; ACT chosen. Mac environment built and benchmarked. Discovered the Dell already ran the arm on Windows; Ubuntu plan dropped. Dell aligned to LeRobot 0.4.4 and connected the WeGo plugin through the CAN shim. |
| 11–13 Sep | Dell: drag-teach verified as a recording method, recorder portal built, CAN "deaf adapter" root-caused, camera-starvation bug fixed. **50 demonstrations recorded on 13 Sep.** Dell wrote `TRAINING INSTRUCTIONS.md`. |
| 13→14 Sep | Mac pre-flight: dataset audit, smoke train, resume test, crash-recovery drill, batch benchmark, chunk size changed 100→30. Five audit agents died at a usage limit (23:51→02:50 Beijing). **Training v1 ran 03:09→14:42 Beijing on 14 Sep.** |
| 14 Sep | Training dashboard deployed on `HETZNER_IP:8850`. Execution portal written (Mac) and audited on the real arm (Dell, 9 fixes). First real runs froze at rest. Dell specified the post-training loop (contract v1); Mac built its half. Round 0 trained and was published. Intervention detection found broken and fixed. 5-agent review of the Mac pipeline → 6 fixes. `pt0_020000` rejected on real trials. |
| 15 Sep | Rule changes (hold-out fraction, correction trimming, roll-joint margin). **Round 1: 100k steps overnight.** `pt1_100000` promoted to CURRENT. Round 2 auto-triggered and published. Dashboard got a session archive. Dell merged its portals into one unified portal, reachable publicly via a relay. |
| 16 Sep | Last arm run (unlabelled, on `pt2_015000`). |
| 22 Sep | LinkedIn series: Day 1 posted; Days 2–13 finalised. |
| 28 Sep | Mac post-training daemon restarted (machine restart); still running. |

---

## 4. Decisions and why

| # | Decision | Why |
|---|---|---|
| D1 | **ACT, not RDT-1B / π0 / OpenVLA** | RDT-1B's T5-XXL encoder is ~45 GB (Mac had ~45 GB free; RDT's README says it won't fit a 24 GB RTX 4090), and fine-tuning requires DeepSpeed multi-GPU, which doesn't run on macOS. A foundation model buys cross-task generalisation that one task on one arm doesn't need. ACT (~52 M) trains overnight on hardware already owned, so every bug costs one loop, not a cluster booking. |
| D2 | **Keep the Dell on Windows; no Ubuntu** | CAN (libusb0 + shim) and the Dabai RGB camera already worked on Windows after real effort. Reinstalling would have traded proven paths for unproven ones; the dataset format is OS-independent. |
| D3 | **Use the WeGo `lerobot_robot_piper` plugin, not a custom robot class** | It calls `piper_sdk` and never touches `can.interface.Bus` directly, so the CAN shim applies transparently (two small patches needed; see §8). |
| D4 | **Train on the Mac, run on the Dell** | The Dell has no GPU; the Mac does ACT at ~2.4 steps/s. |
| D5 | **Kinesthetic (drag-teach) demonstrations** | The plugin's teleop needs a second "leader" arm. Drag-teach was verified to keep streaming joints at full rate (~2,325 frames/s) in teaching mode, and the gripper can be moved by hand (raw −3,200…101,500). Hand-guided motion is smoother than keyboard jogging. |
| D6 | **Action = next state: `action[t] = state[t+1]`** | Hand-dragging produces no command stream. Defining the action as the next measured position makes it a pure regression with no inverse kinematics; verified exact on all 24,570 frames. At run time the outputs are sent as joint-position targets (`JointCtrl`). |
| D7 | **Chunk size 30 (not the ACT default 100)** | ACT was designed at 50 fps (100 steps = 2 s). The data is 15 fps, so 100 steps = 6.7 s of open-loop prediction. The ACT paper's success peaks near a 2 s chunk and falls for longer ones; the author's tuning note says one chunk ≈ 1 s of motion. 30 steps at 15 fps = 2.0 s. This is the one setting that cannot be changed after training. |
| D8 | **100k steps, checkpoint every 5k (instructions said 50k / 10k)** | LeRobot's ACT default and the ALOHA recipe use ~100k for this data scale; `--steps` can be extended on resume (tested), finer checkpoints give more to compare on the robot. |
| D9 | **Batch 8, lr 1e-5 (unchanged)** | Measured batch 8/16/32 = 19.3/20.4/20.0 samples/s: the GPU was saturated, so bigger batches wouldn't move more data per hour, only change update noise. |
| D10 | **Single wrist camera for the first runs** | The C270 scene camera delivered 1–3 fps and black frames. Accepted knowingly: the wrist view is occluded at the grasp. A second camera remains the top hardware next step. |
| D11 | **Unattended training via a supervisor** | Restarts from the last *validated* checkpoint after crash/hang/NaN; checks safetensors headers against file size so a half-written save is never resumed. Proven with `kill -9` mid-run. |
| D12 | **Run inference asynchronously on the Dell** | One ACT forward pass on the Dell's CPU takes 650–800 ms, but the control loop is 15 Hz (66 ms). Plans are computed in the background; stale steps are skipped and plans are blended. |
| D13 | **Human-in-the-loop post-training (HG-DAgger + filtered behaviour cloning)** | When the policy fails, the operator takes over with drag-teach and finishes by hand; that correction starts exactly where *this* policy goes wrong. Clean autonomous successes are also kept. Failed policy segments are never used — imitation learning copies whatever it is shown. |
| D14 | **Every fine-tune trains on base + all accepted episodes** | `lerobot-train --policy.path` recomputes normalisation statistics from the dataset you give it; training on rollouts alone would shift the statistics and damage the pretrained weights. |
| D15 | **Two-stage acceptance: offline gate, then real-arm promotion** | Loss measures imitation of the demos, not task success. Gate set A (no forgetting) and set B (learned held-out corrections) decide what gets *published*; only ≥10 labelled real-arm trials beating CURRENT's last 10 decide what becomes the *default*. |
| D16 | **No seeds at evaluation** | ACT zeroes its CVAE latent and disables dropout at inference; two different seeds gave identical outputs (max diff 0.0). Trial-to-trial variation is entirely physical. |
| D17 | **Explicit "Start correction" trigger** | Detecting drag-teach from firmware status proved unreliable (§8). A button / `C` key makes the switch to a human segment deterministic. |

---

## 5. Data actually used

### 5.1 Base demonstrations — `datasets/piper_pick_place` (recorded 13 Sep)

| Property | Value |
|---|---|
| Episodes / frames | **50 / 24,570** at 15 fps, 27.3 min, 166.4 MB, LeRobot format v3.0 |
| Features | `observation.state` (7), `action` (7), `observation.images.wrist` (480×640×3, RGB) |
| Joint order / units | `joint1…joint6, gripper`; joints −100…100, gripper 0…100 (raw 0…68,000, clamped) |
| Episode length | min 20.8 s, median 29.8 s, max 55.5 s |
| Idle start > 2 s | **18 of 50** (worst: episodes 4, 7, 12, 8, 6 at 5.5–6.7 s) |
| Regrasp | 1 episode (episode 1), kept as a recovery example |
| Repeated frames | ~17% (12.5 fps camera into a 15 fps recording) |
| Gripper while holding | raw 52,000–53,600 (inside the 68,000 cap) |
| Video codec | **49 of 50 episodes AV1**, 1 H.264 (all decode fine) |
| Mac audit | `action[t]==state[t+1]` exact on all frames; stats recomputed, max relative error 8.5e-5; RGB confirmed (red card is red) |

### 5.2 Idle-trimmed base — `~/NervusOS/robots/vla/posttrain/base_trimmed` (Mac, local)

Rule: onset = first frame where any of joint1–6 moves > 1.0 from frame 0; keep from `onset − 5`.
Result: **23,138 frames (1,432 removed)**; 18 episodes lost more than 25 frames. Re-encoded H.264 at CRF 18.

### 5.3 Rollouts from the real arm — `posttrain/rollouts/` (Dell writes)

| Item | Count |
|---|---|
| Rollout packages | 62 (14–16 Sep) |
| Labelled and processed | 51 → **42 accepted**, 9 with no usable episodes (failed, no correction) |
| Accepted episodes | **42 = 32 hand corrections + 10 clean autonomous successes**, 21,578 frames |
| Unlabelled (no `READY`) | **11** — never become data until marked Success/Fail in the portal |
| Rejected | 0 currently (one wrist-roll rejection was reversed after the margin change, §8) |
| Not yet in any round | **8** (3 corrections, 5 clean runs, 6,163 frames) |

Correction episodes are trimmed of stillness at both ends (any of the 7 dims moves > 1.0, ±5 frames padding):
round 1 kept 8,797 of 10,391 rollout frames; round 2 kept 11,990 of 14,393.

---

## 6. Training runs and results

| Run | Parent | Data | Steps | Final loss | Gate A (lower better) | Gate B (lower better) | Outcome |
|---|---|---|---|---|---|---|---|
| **v1** (13–14 Sep) | — | 50 base, 24,570 frames | 100k (save 5k) | 0.067 (from 6.816) | 1.547* | — | Original model; 10 published (`v1_010000…100000`) |
| **Round 0** (14 Sep) | `v1_100000` | trimmed base, 23,138 frames | 20k | 0.059 | **1.345** vs 1.547 | — | Published `pt0_020000`; **rejected** on the arm (1/11 = 9% vs 20%) |
| **Round 1** (14–15 Sep) | `pt0_020000` | 72 eps (50 + 19 corr + 3 clean), 31,935 frames, 3 held out | 100k (save 10k) | 0.050 | **1.029** (100k) | 5.61 (90k) / 5.77 (100k) vs 6.72 | Published `pt1_090000`, `pt1_100000`; **`pt1_100000` promoted** |
| **Round 2** (15 Sep) | `pt1_100000` | 82 eps (50 + 27 corr + 5 clean), 35,128 frames, 2 held out | 20k (save 5k) | 0.053 | **1.013** (15k) | **4.70** vs 5.12 | Published `pt2_015000`; **awaiting trials** |

\* Gate A on the trimmed base, episodes 4/13/27/41/49, every 15th frame (156 samples); "hold still" baseline 6.99.

### Real-arm trials (`deploy/runs/results.csv`, one row per run)

| Policy | Success | Rate |
|---|---|---|
| `act_pick_place_v1_100000` | 2 / 15 | 13% |
| `act_pick_place_pt0_020000` | 1 / 20 | 5% |
| `act_pick_place_pt1_100000` (CURRENT) | 7 / 17 | 41% |
| `act_pick_place_pt2_015000` | 0 / 0 | — |

Promotion record: `pt1_100000` became CURRENT on 15 Sep 14:19 UTC at 3/11 (27%) vs the old model's last 10 at 2/10
(20%). It has since reached 7/17.

---

## 7. How the post-training loop works now

```
operator runs policy on the arm
   │ fails → press drag-teach AND "Start correction" (or C) → finish by hand → Stop → mark Success/Fail
   ▼
Dell writes posttrain/rollouts/<run_id>/ : episode.jsonl + wrist.mp4 + meta.json, then READY (last)
   ▼ Syncthing
Mac watcher (every 5 min): validate → split → convert to LeRobot (H.264) → ledger.json
   ▼ trigger: ≥5 new corrections or ≥10 new accepted episodes, ≥6 h since the last round (or forced)
fine-tune from CURRENT on base_trimmed + all accepted episodes (~15% of untrained corrections held out)
   ▼
offline gate: A ≤ 1.10 × CURRENT (no forgetting) AND B < CURRENT (learned held-out corrections)
   ▼ pass
publish candidate to policies/ (atomic copy) + UPDATE.md entry
   ▼ operator runs ≥10 labelled trials of the candidate
promote to CURRENT.json only if its success rate ≥ CURRENT's most recent 10
```

**Episode rules (contract v1):** each contiguous human segment ≥ 30 rows is always used; a whole run is used only if
`outcome == success` and `interventions == 0`; failed or pre-intervention policy rows are never used; a gap in `k`
splits an episode. Packages are rejected for `mock`, wrong contract version, frames ≠ rows, writer errors, or states
outside the demonstrated range (±10 units; ±30 on the roll joints 4 and 6).

**Where it runs:** `~/NervusOS/robots/vla/tools/posttrain/` (`config`, `packages`, `lerodata`, `gate`, `pipeline`),
launched by launchd agent `com.nervus.piper-posttrain`. State the Dell can see is in `posttrain/mac/`
(`STATUS.json`, `ledger.json`, `CANDIDATE.json`, `rounds/<N>.json`).

---

## 8. What went wrong, and the correction

Grouped by area. "Mine" marks mistakes made on the Mac/assistant side, so they are not repeated.

### Planning and documentation

| Problem | Root cause | Correction |
|---|---|---|
| RDT-1B plan unworkable | 45 GB encoder; DeepSpeed/CUDA-only fine-tuning | Switched to ACT on 10 Sep (D1). |
| First Dell handoff told the Dell to install Ubuntu and write its own robot class (mine) | Written from old notes before reading the Dell's own README | Deleted; rewrote `HANDOFF-VLA.md` after reading what actually worked on Windows. |
| Recommended `--policy.checkpoints_total_limit` (mine) | Flag does not exist in LeRobot 0.4.4; would have crashed on launch | Dell caught it against source; `TRAINING INSTRUCTIONS.md` supersedes those commands. |
| Recommended a folder path as `--dataset.repo_id` (mine) | Never tested | Correct form: `repo_id=local/<name>` plus `--dataset.root=<path>`. |
| Five pre-flight audit agents died overnight | Usage limit hit at 23:51; nobody watching | Audit finished by hand after 02:50; training started 03:09. Now: launch long jobs under the supervisor, not under agents. |
| Estimated Dell inference at 200–350 ms (mine) | Extrapolated from the Mac CPU | Measured 600–800 ms (thermal/power throttling to ~27% CPU performance, plus CAN parsing load). Portal re-tuned for it. |

### Hardware link (Dell)

| Problem | Root cause | Correction |
|---|---|---|
| WinUSB can't open the adapter | Composite device; access denied | Bind to libusb0 with Zadig; copy `libusb-1.0.dll` onto PATH. |
| Arm streams its pose but ignores every write ("deaf") | Adapter's shared TX/RX buffer pool left full after a process exited mid-flood; firmware refuses writes while reads continue | Shim drains 0.4 s, probes a harmless write, reopens up to 4×. |
| `struct.error` crashes, "silent bus" | Two leftover teleop processes owning the adapter; arm in STANDBY doesn't broadcast | Check for stray `python.exe`; press drag-teach so the arm streams; shim catches `struct.error`. |
| Plugin threw "CAN socket does not exist" on Windows | `JudgeCanInfo()` reads `/sys/class/net` | Shim blanks it (must import from `piper_sdk.hardware_port`). |
| SDK swallowed bus-creation failures; portal showed "connected" on a dead bus | Cached zero joints normalise to a plausible pose | Liveness judged by frames/s (`rx_count`), not by "read succeeded". |
| Hard-killing the portal wedged the adapter | Windows `os.kill` = `TerminateProcess` | Clean shutdown path; signal handlers release the adapter. |

### Data collection

| Problem | Root cause | Correction |
|---|---|---|
| Camera collapsed from 12.5 fps to 1 fps while status said OK | `EnableFkCal()` ran forward kinematics ~4,600×/s in Python, starving the camera thread; "stale" threshold of 1 s let 1 fps pass | FK computed once per recorded frame; recording refuses/aborts below 5 fps. |
| Dataset was mostly AV1, not H.264 | LeRobot defaults to libsvtav1 when a dataset is reopened without `vcodec` | Decodes fine; all Mac-built datasets now pass `vcodec="h264"` explicitly; Dell recorder patched. |
| 3D trace side-files were cumulative | Buffer cleared in only one branch | Cleared at take start; history repaired by `repair_traces.py`. |
| 18 demos idle > 2 s at the start | Operator pause before moving | Not fixed at recording; trimmed later (round 0) — see "Deployment". |
| C270 scene camera unusable | USB transfer failure (black frames, 1–3 fps) | Not fixed. Single-camera training accepted (D10). |

### Training (Mac)

| Problem | Root cause | Correction |
|---|---|---|
| Chunk 100 would have meant 6.7 s open-loop | Default assumes 50 fps | Chunk 30 (D7). |
| A 20-step fine-tune scored *worse* than its parent on the gate | Fine-tuning recomputes normalisation stats from the new dataset | Understood; rounds run long enough to re-fit (20k+). |
| Re-encoded frames failed the 3/255 fidelity check | LeRobot hard-codes CRF 30 (3.8/255 error) | Encoder CRF overridden to 18 (2/255). |
| `validate_frame` rejected the base features | `info.json` stores shapes as lists; LeRobot compares tuples | Features converted to tuples. |
| Gate numbers didn't match the Dell's (1.79 vs 1.65) | Different frame samples | Gate always compares candidate and CURRENT on identical samples; only the ratio matters. |
| Dashboard broke after round 1 cleanup | Exporter read the merged dataset, which cleanup deletes | Sessions exporter rebuilds the dataset summary from the round record and train log. |
| Dashboard feed agent crash-looped under launchd | Script daemonised itself while launchd expected a foreground process | `--daemon` removed from the plist. |

### Deployment on the real arm

| Problem | Root cause | Correction |
|---|---|---|
| `KeyError: 'joint1'` in the stock runner | Dataset names `joint1`; plugin reports `joint1.pos` | Portal maps names. |
| Risk of feeding BGR to an RGB model | OpenCV captures BGR | Portal converts; verified (BGR raises gate error 1.79 → 2.43). |
| `connect()` drives home; `disconnect()` parks then drops the arm | Plugin defaults | Portal never calls either. |
| Arm frozen at rest on the first real runs | 18 idle demos taught "stay still"; ACT has no notion of time, so once it waits it waits forever. Daylight was suspected first and ruled out (still frozen under the same evening lamp) | Short-term: start from a raised pose that sees the table (scene score 1.2 → 65). Long-term: round 0 trained on the idle-trimmed base. |
| Hovering over the red card without releasing | Model's own plan equalled the current pose (difference 0.0) in every long hover; the arm stopped 3–8 units off the demo release pose — a pose never seen | Corrections from the hover pose (5–10 recommended); not a portal bug. |
| Missing the block on the first approach | Plan computed from a camera frame ~1.0 s old (p90 1.3 s); demos move ~21 units/s, grasp spread ~8 units | Lower `speed_pct`; correct by hand instead of moving the block. Real fix: run the model on the Mac GPU (§11). |
| Gripper crawled open, flipped half-open, dropped or **crushed** a 3D-printed cube, vibrated | Step limit anchored to the measured (lagging) position; plan flips across 50; effort 1000; 15 Hz re-sends of an unchanged target | Step limit on the command itself, lead limit, 2-plan joint ensemble (not gripper), grasp lock (8 steps to release), effort 500, squeeze 2, re-send only on change. "Fully open" stretched to raw 95,000 without changing calibration. |
| Arm "shot toward the ceiling" | Out-of-distribution start (joint5 at its limit) → policy extrapolated | Start from demo-like poses; idle-trimmed retraining. |
| Page E-stop left the portal unrecoverable | Feedback went stale; no way to reopen the bus | Reconnect button. |
| Space bar re-started a run | Focused button re-clicked on key-up | Blur after click; swallow Space key-up. |
| Runs stopped with "arm feedback 310–367 ms old" | Dell throttled to ~27% CPU (15 W chip, charging, Chrome/VS Code competing) | 1 s grace for stale arm/camera; operator advice: plug in, close heavy apps. |

### The post-training loop

| Problem | Root cause | Correction |
|---|---|---|
| **Your first hand corrections were not recorded** | Intervention check ran *after* the stale-feedback stop, and firmware mode was only trusted if < 0.5 s fresh | Dell added **"Start correction" button + `C` key**, logs `ctrl_mode/arm_status/teach_status` per row, moved the check earlier. Corrections then recorded correctly (first: run 235153). |
| Same 5 newest corrections held out every round → never trained (review, HIGH) | Hold-out had no memory | Only not-yet-trained corrections can be held out. |
| With ≤5 corrections, *all* were held out | Fixed hold-out of 5 | Hold out ~15% of untrained corrections (max 5). |
| Round stuck in "building" after a crash (review, HIGH) | No resume path | Building rounds are re-run on the next tick. |
| Duplicate `results.csv` rows counted as extra trials | Portal appended a row per click | Dell: one row per run, first label wins. Mac: dedupe by `run_id`. |
| Gate A penalised the trimmed model for not idling | Set A used the untrimmed base | Set A runs on the trimmed base. |
| Still-syncing files permanently rejected | Missing file treated as invalid | Missing → pending, retried. |
| Manual and daemon ticks could run concurrently | Lock only in the daemon | `fcntl` lock in every tick. |
| Possible promotion against an untested CURRENT | Missing baseline defaulted to 0% | Refuses to promote without CURRENT trials. |
| Rotated-block correction rejected (wrist roll 57.9) | ±10 range margin on every joint | ±30 on roll joints 4 and 6. |
| Corrections re-taught idling (10.9 s still before dragging, ~3 s after) | Untrimmed rollout episodes | Leading/trailing stillness trimmed from all rollout episodes. |

---

## 9. Open issues and risks

**Security:** `server/relay.py` has no authentication, and the portal behind it can move the arm. Never expose it
publicly without an auth layer; the physical e-stop remains the last line of defence.

**Model and evaluation caveats:**

- **Gate B is weak.** On held-out corrections, "hold still" beat every round-1 checkpoint (5.41 vs 5.57–5.84) and
  beat CURRENT in round 2 (4.89 vs 5.12); only `pt2_015000` (4.70) beat it. The pass rule compares against CURRENT
  only, and B has just 2–3 episodes (61–88 samples). Consider requiring B < hold-still, and more held-out episodes.
- **Promotions rest on small samples.** `pt1_100000` won 3/11 vs 2/10 — a one-success difference. Its later 7/17 is
  more convincing. Trials also span portal fixes made between sessions, so models were not tested under identical
  conditions.
- **Lighting.** All 50 demos were recorded at night. Daylight is a distribution shift; test candidates and CURRENT
  under the same light, and record daylight corrections if it fails.

**Known limits (by design or hardware):**

- Wrist camera only: the cube disappears from view at the grasp and when it ends up behind the gripper (partial
  observability). Needs a fixed second camera.
- Rotated cubes: no demo shows a twisted grasp. A cube needs ≤45° of wrist roll (demos already span −75°…+57°), but
  a diagonal approach needs ~71 mm opening vs the 68 mm calibration cap. Needs 15–20 rotated-block corrections.
- Dell inference latency (650–800 ms) makes plans ~1 s stale.

**Housekeeping:**

- 11 unlabelled rollout packages; 8 accepted episodes waiting for round 3 (needs 2 more corrections or 2 more accepted
  runs).
- `convert_package` is not crash-atomic against `rollout_eps` (a kill mid-append + `retry-errors` could duplicate an
  episode). Do not use `retry-errors` blindly.
- `ledger.json` `first_round` is never filled (cosmetic).
- `posttrain_ctl.py` has no CLI for choosing parent/steps; custom rounds need `pipeline.start_round(...)` in Python.
- Dashboard feed re-pushes all sessions every 60 s even when nothing changes.
- Disk: `outputs/` holds 12 GB (v1's 20 checkpoints with optimizer state); `posttrain/` 4.2 GB; 36 GB free.

---

## 10. Legacy: what exists and where

### Mac (`~/NervusOS/robots/vla/`)

| Path | Purpose |
|---|---|
| `.venv/` | Python 3.11, LeRobot 0.4.4, torch 2.10.0 (MPS), torchcodec 0.10, av 15.1 |
| `tools/train_supervisor.py`, `tools/train_status.sh` | Crash-resuming trainer; one-glance status |
| `tools/run_act_piper_pick_place_v1.json` | v1 run config |
| `tools/posttrain/*.py`, `tools/posttrain_ctl.py` | Post-training loop and CLI |
| `tools/portal_export.py`, `tools/portal_sessions.py` | Dashboard exporters (sessions = current) |
| `portal/` | Dashboard source (`server.py`, `act-portal.service`, `www/`) |
| `outputs/train/act_piper_pick_place_v1/` | v1 checkpoints (12 GB) |
| `posttrain/` | `base_trimmed`, `rollout_eps`, `round0–2`, `pipeline.log` |
| `linkedin/piper-act-13-day-series.md` | LinkedIn series (Day 1 posted; `[hackathon name]` still to fill) |
| `bench_act_mps.py` | Original MPS benchmark |

launchd agents: `com.nervus.piper-posttrain` (loop, every 5 min), `com.nervus.piper-portal` (dashboard feed, every 60 s).

### Shared folder (`~/Shared/Piper Arm/`, Syncthing)

| File | Status |
|---|---|
| `README.md` | Dell's hardware reference — still authoritative for hardware |
| `TRAINING INSTRUCTIONS.md` | Authoritative for training commands (supersedes `HANDOFF-VLA.md` commands) |
| `HANDOFF-VLA.md` | Historical; its training commands are wrong (§8) |
| `EXECUTION.md` | Portal guide, annotated by the Dell; portal now superseded by the unified portal |
| `AUTOMATED POST-TRAINING.md` + `posttrain/CONTRACT.json` | Loop specification; the contract wins on conflict |
| `UPDATE.md` | Full two-machine log (34 entries) |
| `datasets/`, `policies/` (+ `CURRENT.json`), `posttrain/`, `deploy/` | Data, models, loop state, portal + `runs/results.csv` |

### Dell (`D:\Piper-CAN-Teleop\`)

`scripts\piper_mac_can.py` (CAN shim), `lerobot_robot_piper\` (patched plugin), `scripts\piper_record_portal.py`
(recorder), `scripts\piper_act_portal.py` + `piper_act_portal.bat` (**unified portal, :8790 — use this**),
`piper_act_tunnel.bat` (reverse tunnel to the server), `scripts\piper_cartesian_ctl.py` (teleop, :8770 — never run
alongside a portal; they share the adapter), `vla\gpt_driver.py` (earlier VLM-in-the-loop driver). Old portals kept
as fallback.

### Server (`HETZNER_IP`)

`act-portal.service` → `/opt/act-portal`, port **8850** (training dashboard with session archive; safe, read-only).
`server/relay.py`, port **8790** (Dell portal relay; no authentication — see §9). Syncthing relay/discovery on
22067/8444. Nothing else on the server was changed by this project.

### Superseded

The Ubuntu-on-Dell plan, the RDK S100 CAN host, the USB-stick `HANDOFF-DELL.md`, the Mac-side CAN teleop, and
`portal_export.py` as a running daemon.

---

## 11. Next steps (in order)

1. **Add authentication** before exposing the Dell portal relay again (§9).
2. **Label the 11 unlabelled runs** in the portal, then **run ≥10 labelled trials of `pt2_015000`** against CURRENT
   under the same lighting. The loop promotes it automatically if it wins.
3. **Record 2+ more corrections** to trigger round 3 (8 episodes already waiting). Prioritise: missed approaches
   (~10), hovers over the card (5–10), rotated cubes at ±15/30/45° with a wrist twist (15–20).
4. **Fix or replace the scene camera** (C270 on a different cable/port, or a new UVC camera with a unique device
   name) and record a **new two-camera dataset**. This changes the model inputs, so it is a fresh training run, not a
   loop round.
5. **Move inference to the Mac GPU** (LeRobot `async_inference` policy server, Dell as client) to cut plan age from
   ~1 s to a fraction.
6. **Tighten gate B** (require beating hold-still; more held-out episodes).
7. **SmolVLA** on top of the proven pipeline (`act` and `smolvla` share LeRobot's interface; `mlx-smolvla` exists for
   native Apple-Silicon inference).

---

## 12. Quick commands

```bash
cd ~/NervusOS/robots/vla
.venv/bin/python tools/posttrain_ctl.py status        # loop state, rounds, candidate, ledger
.venv/bin/python tools/posttrain_ctl.py round-now     # force the next round at the next tick
tools/train_status.sh                                 # v1 training status (historical)
tail -f posttrain/pipeline.log                        # loop activity
launchctl bootout gui/$(id -u)/com.nervus.piper-posttrain   # stop the loop
launchctl bootout gui/$(id -u)/com.nervus.piper-portal      # stop the dashboard feed
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.nervus.piper-posttrain.plist  # start again
```

Dashboard: `http://HETZNER_IP:8850` (add `?present=1` for screenshot mode).
