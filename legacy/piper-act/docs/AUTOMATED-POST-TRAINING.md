# AUTOMATED POST-TRAINING — robot runs become training data

This file specifies a loop in which runs of the trained policy on the Dell produce new training episodes. The Mac
turns them into a better policy and the Dell tests it.

- **Dell side:** built and verified (§4).
- **Mac side:** specified here, for Claude on the Mac to build (§5).
- **Machine-readable half:** `posttrain/CONTRACT.json`. If this file and the contract disagree, **the contract wins**.

Written 2026-09-14 by the Dell. Read order for the Mac: this file → `posttrain/CONTRACT.json` →
`TRAINING INSTRUCTIONS.md` (base training) → `EXECUTION.md` (the portal) → `UPDATE.md` (latest facts).

---

## 0. What this is, in one paragraph

This is **human-in-the-loop post-training**, also called **interactive imitation learning**. It combines two
established ideas:

1. **Interventions (HG-DAgger).** When a run goes wrong, the operator presses the arm's drag-teach button and finishes
   the task by hand. The portal records that correction. It is a demonstration that starts exactly where *this*
   policy gets into trouble, which the original 50 demos never show.
2. **Filtered behaviour cloning, also called self-imitation.** Runs the policy completed **alone and successfully**
   are added as extra demonstrations. They are smooth and consistent, because the robot executed them at 15 Hz.

The Mac periodically fine-tunes the current policy on **the original demos + every accepted episode so far**. It
publishes a *candidate*; the candidate replaces the current policy only after it does at least as well **on the real
arm** (§5.9).

---

## 1. Honest expectations

| Question | Answer |
|---|---|
| Does the model "learn on itself" from every run? | **No, on purpose.** Imitation learning copies whatever it is shown. Training on failed runs teaches the failure. Only corrections and clean successes are used (§3.3). |
| Will it get smoother? | Partly. The 09-14 jitter was mostly **execution**, fixed in the portal the same day (UPDATE 09-14). Robot-executed successes are smoother than hand demos, so filtered self-imitation helps further. |
| More accurate grasps? | **Yes, this is what corrections are best at.** Every correction starts from a misaligned approach the policy produced and shows the fix. Published HG-DAgger results need tens of corrections, not hundreds of demos. |
| Rotated blocks (the model never saw one)? | **Only with human corrections of rotated blocks.** The policy cannot invent a wrist roll it was never shown. Plan ~15–20 corrected runs with the block at different angles. |
| Can it improve with no human at all? | Not reliably with ACT. That needs a reward signal and automatic resets, i.e. reinforcement learning (e.g. LeRobot's HIL-SERL), which is a different policy family. Out of scope, not ruled out later. |
| The hand demos were fast in some episodes and slow in others. Is that a problem? | ACT copes with mixed speeds, but it blurs them. Corrections at a **steady, moderate pace** make the data more consistent over time. |

---

## 2. Architecture

```
 DELL  (deploy\piper_policy_portal.py, :8791)                     MAC  (to build, §5)
 ─────────────────────────────────────────────                    ────────────────────────────────────────
 Run policy ──► operator presses teach button when it goes wrong
            ──► hand correction recorded (segment "human")
 Stop ─► mark Success/Fail ─► READY written                        watcher (every 5 min)
            │                                                          │ validate package (§5.2)
            ▼                                                          │ convert → LeRobot episodes (§5.3)
 Shared/Piper Arm/posttrain/rollouts/<run_id>/  ══ Syncthing ══►   │ ledger.json
                                                                      ▼
                                                               trigger? (§5.5) ── no ─► wait
                                                                      │ yes
                                                               merge: base (idle-trimmed) + ALL accepted
                                                               fine-tune from CURRENT (§5.6)
                                                               offline gate (§5.7)
                                                                      │ pass
 policies/act_pick_place_pt<N>_<step>/  ◄══ Syncthing ══════   publish candidate (§5.8)
 portal lists it; operator runs ≥10 trials, marks results
 deploy/runs/results.csv  ═══════════ Syncthing ═══════════►   promotion rule (§5.9) ─► policies/CURRENT.json
 portal preselects CURRENT  ◄═════════ Syncthing ══════════
```

**Ownership, which is never violated:**

- **Dell writes:** `posttrain/rollouts/**` and `deploy/runs/**`.
- **Mac writes:** `posttrain/mac/**`, new `policies/<name>/`, and `policies/CURRENT.json`.
- **Neither machine** edits or deletes the other's files. Built datasets and training outputs stay **local on the
  Mac**; only finished policies go into the shared folder.

---

## 3. The contract (`posttrain/CONTRACT.json`, version 1)

### 3.1 Rollout package: what the Dell writes per run

```
posttrain/rollouts/20260914-192718_act_pick_place_v1_100000/
  episode.jsonl   one row per 15 Hz control step
  wrist.mp4       mp4v; decoded frame i = exactly the image the policy saw at row i (OpenCV gives BGR → convert to RGB)
  meta.json       run facts + operator label
  READY           written LAST, after the operator marked Success/Fail; text = "success" | "fail"
```

**A folder without `READY` does not exist as far as the Mac is concerned.** Syncthing can deliver files in any
order. `READY` is tiny and written last, but the Mac must still check that `meta.json` has `outcome` and that
`frames` equals the number of rows before using a package.

**Row fields:**

| Field | Meaning |
|---|---|
| `k` | step index. A jump of more than 1 means rows and frames were dropped together: split the episode there |
| `t` | seconds since run start |
| `segment` | `policy` or `human` |
| `state` | 7 floats in `joint1..joint6, gripper` order, plugin-normalized, identical units to `observation.state` in the base dataset |
| `sent` | 7 floats commanded. `null` in human segments |
| `plan` | 7 floats, the model's value before clamps. `null` in human segments or when starved |
| `grip` | `follow`, `hold`, `release`, `starved` or `human` |
| `cam_age_ms` | age of the camera frame when it was used |
| `obs_step` | the `k` of the observation this step's plan came from |
| `frame` | index into `wrist.mp4`; equals the row index |

`meta.json` carries:
- **run facts:** `contract_version`, `run_id`, `mock`, `checkpoint`, `params`, `task`, `steps`, `stop_reason`,
  `interventions`, `intervened_at_s`, `frames`, `dropped`, `writer_error`
- **operator label:** `outcome`, `case`, `note`

### 3.2 Episode rules (how packages become training episodes)

- `action[t] = state[t+1]`, **exactly as in the teaching data**. Drop each episode's last row. Do *not* use `sent`
  or `plan` as the action; they are logged for diagnosis only.
- **Reject the whole package** if any of these holds:
  - `READY` is missing
  - `mock` is true
  - `contract_version` is not 1
  - `frames` differs from the row count
  - `writer_error` is set

### 3.3 Which segments are used

| Segment | Used when | Why |
|---|---|---|
| each contiguous `human` segment with ≥ 30 rows | **always**, success or fail | it is a correction from a state the policy actually reaches |
| the whole run (all `policy` rows) | **only** if `outcome == success` and `interventions == 0` | clean autonomous success = self-imitation |
| `policy` rows of failed runs, or of runs before an intervention | **never** | they show the mistake |

### 3.4 Changing the contract

Any change to the package format, the episode rules, the paths or ownership **bumps `contract_version`**.

- **The portal refuses to record** when `CONTRACT.json` has a version it does not know. It says so on the page and
  tells the operator to set `record_rollouts` to 0.
- **The Mac pipeline must also refuse** to process packages or publish under an unknown version, and write the reason
  to `posttrain/mac/STATUS.json`.
- **The machine that bumps the version** logs an entry tagged `[CONTRACT vN]` in `UPDATE.md` **and tells the user**
  that the other machine needs updating.

Version 1 is designed so this should rarely happen:
- **Additive changes** do not bump it: a new `meta.json` key or new optional row fields.
- **Only renames, removals, and meaning changes** do.
- **Readers must ignore unknown keys.**

---

## 4. Dell side: built and verified (2026-09-14)

### 4.1 What the portal does

| Parameter (page) | Default | Effect |
|---|---|---|
| `record_rollouts` | 1 | every run writes a package to `posttrain/rollouts/<run_id>/`. Dry runs (`--mock-*`) write to a local temp folder, never to the synced one |
| `intervene_on_teach` | 1 | pressing the drag-teach button mid-run switches to recording a **human** segment instead of stopping. The portal stops commanding and sends nothing afterwards, so the arm is never stiffened in the operator's hands |

- **On Success/Fail:** the portal writes `outcome` into `meta.json` and then `READY`. The first label wins; a second
  click still appends to `results.csv` but does not change the package.
- **Checkpoint preselection:** `policies/CURRENT.json` is preselected in the checkpoint list.
- **Contract check:** before each recorded run.

### 4.2 Verified (dry run on the Dell, checkpoint 100000)

| Check | Result |
|---|---|
| rows vs video frames | 372 / 372 |
| `k` contiguous, `frame == row index` | yes / yes |
| `mock: true` package location | `%TEMP%\piper_posttrain_mock` (not synced) |
| `READY` text / `meta.outcome` | `fail` / `fail` |
| video | 640×480, 15 fps, decodes with OpenCV |

**Not yet exercised on the real arm:** the human-segment switch. It only triggers when real firmware reports
drag-teach, which a mock arm cannot do. The first real correction should be checked in its `episode.jsonl` (`segment`
changes to `human`, `state` keeps streaming).

### 4.3 Operator workflow (for the user)

1. **Set up and start.** Place the block (vary the position and **angle** on purpose), start as in `EXECUTION.md`,
   then **Run**.
2. **If it goes wrong, correct it.** Examples: it drifts off the block, grasps badly, hesitates for more than about
   3 s, or drops the block. **Press the drag-teach button immediately** and finish the whole task by hand: grasp,
   lift, carry, release over the red card. Move at a **steady, moderate pace**.
3. **Stop** (Space).
4. **Mark:**
   - **Success** only if the **policy alone** did the task with no intervention.
   - **Fail** if it failed or you had to help. Your correction is still used.
5. **Start the next trial.** Aim for:
   - about 20 corrections of whatever fails most often
   - 15–20 runs with a rotated block
   - every clean success you get

---

## 5. Mac side: to build

Build it next to the existing `~/NervusOS/robots/vla/tools/train_supervisor.py`. Reuse its crash-resume,
`caffeinate`, and atomic-publish logic. **Everything below is required behaviour; the implementation is yours.**

### 5.1 State the Mac keeps

```
Shared/Piper Arm/posttrain/mac/            (Mac-owned, synced so the Dell can see progress)
  STATUS.json        {"state": idle|converting|training|gating|published|blocked, "detail", "updated"}
  ledger.json        per run_id: accepted episodes (+ lengths) or rejection reason; round each was first used in
  CANDIDATE.json     current candidate: name, parent, round, dataset summary, gate metrics, published time
  rounds/<N>.json    full record of round N (inputs, command, metrics, decision)
~/NervusOS/robots/vla/posttrain/            (local, NOT synced)
  base_trimmed/      LeRobot dataset, built once (§5.4)
  rollout_eps/       LeRobot dataset of all accepted rollout episodes, append-only
  round<N>/          merged dataset for round N, training output_dir
```

### 5.2 Watcher and validation

- **Scan:** every 5 minutes, scan `posttrain/rollouts/*/READY`. Skip `run_id`s already in the ledger.
- **Before accepting a package, verify:**
  - the §3.2 reject rules
  - the rows parse
  - `wrist.mp4` decodes to `frames` frames of 480×640×3
  - every `state` value is inside the base dataset's `observation.state` min/max ± 10 units
- **On failure:** log the reason in the ledger, never retry silently, and move on.

### 5.3 Conversion

- Split into episodes per §3.3.
- Use **the base dataset's features exactly**:
  - the same `observation.state` and `action` names/shapes/dtypes
  - `observation.images.wrist` (video, 480×640×3, RGB)
  - `fps=15`, `robot_type="piper_follower"`, the task string from the contract
- **Pass `vcodec` explicitly every time a LeRobotDataset is opened.** `LeRobotDataset.__init__` defaults to libsvtav1
  on resume, which is how the base dataset became mixed h264/AV1 (UPDATE 09-13 19:09 §5).
- **After writing, confirm:**
  - the reloaded episode length equals rows − 1
  - `action[t] == state[t+1]`
  - frame 0 matches the mp4's frame 0 after BGR→RGB (mean absolute difference < 3/255)

### 5.4 Base dataset, trimmed (one time)

The real-arm runs on 09-14 froze at the rest pose. Cause: 18 of the 50 demos wait for more than 2 s before moving,
teaching "at rest → stay" (UPDATE 09-14 18:35).

**Trim rule:**
1. `onset` = first frame where `max_j |state[t] − state[0]|` over joint1..joint6 exceeds 1.0.
2. Keep frames from `max(0, onset − 5)` on.

Do not modify `datasets/piper_pick_place`; write `base_trimmed` locally. Report the number of frames removed per
episode in `rounds/0.json`. Expect roughly 18 episodes to lose more than 25 frames.

### 5.5 Trigger

Start a round when **no training is running** and **either** holds:
- ≥ 10 new accepted episodes since the last round
- ≥ 5 new **human** episodes since the last round

**Round 0** needs no new episodes: `base_trimmed` fine-tuned from CURRENT, to fix the frozen start. Run it as soon as
the pipeline exists.

At most one round per 6 hours unless the user starts one by hand.

### 5.6 Fine-tune command (LeRobot 0.4.4)

```bash
lerobot-train \
  --policy.path="/Users/ianzhang/Shared/Piper Arm/policies/<CURRENT name>" \
  --policy.device=mps \
  --policy.push_to_hub=false \
  --dataset.repo_id=local/piper_pick_place_pt<N> \
  --dataset.root="$HOME/NervusOS/robots/vla/posttrain/round<N>/data" \
  --dataset.image_transforms.enable=true \
  --batch_size=8 \
  --steps=20000 \
  --save_freq=5000 \
  --log_freq=200 \
  --output_dir="$HOME/NervusOS/robots/vla/posttrain/round<N>/train" \
  --job_name=act_piper_pick_place_pt<N> \
  --wandb.enable=false
```

**Facts behind it, all checked in 0.4.4 source:**

- **`--policy.path` loads the checkpoint's config and weights** (`configs/train.py:83-88`). `chunk_size=30` and every
  architecture setting come from the checkpoint. Do not pass `--policy.type`.
- **Normalization statistics are re-taken from the dataset you train on** (`lerobot_train.py:262-280`). This is why
  the round dataset is **always base_trimmed + all accepted episodes**. Training on rollouts alone would shift the
  statistics and wreck the pretrained weights.
- **The optimizer starts fresh** at the checkpoint's lr (1e-5); it is not a `--resume`.
- **20k steps ≈ 2.3 h** at the measured 2.41 steps/s on the M2 Pro.
- **Mixing ratio:** if human episodes are under 15 % of the round dataset, duplicate them (write them twice) up to
  15 %. LeRobot 0.4.4 has no per-episode sampling weights.

**The command was smoke-tested on the Dell** with `--policy.path` = checkpoint 100000 on the base dataset (CPU, 3
steps); see UPDATE 09-14 for the log. Run a 20-step smoke test on the Mac the first time.

### 5.7 Offline gate (the Mac cannot test on the robot)

Metric: mean absolute error of the 30-step chunk against the recorded actions, in normalized units. Sample every 15th
frame and average over samples. `EXECUTION.md` §3 explains why no seed is needed.

| Set | Candidate must satisfy |
|---|---|
| A: base episodes 4, 13, 27, 41, 49 | ≤ 1.10 × CURRENT's error (no forgetting) |
| B: the newest 5 human episodes, **held out of this round's training** | < CURRENT's error (it learned the corrections) |

Train the round **without** set B. If the gate passes, **retrain nothing**; publish the candidate as it is. Set B
then joins the next round. For reference, CURRENT = 100000 scored **1.79** on set A on the Dell, against a
"hold still" baseline of 6.09.

### 5.8 Publish

1. Copy `train/checkpoints/020000/pretrained_model/` to `policies/.tmp_act_pick_place_pt<N>_020000`, then rename it
   to `policies/act_pick_place_pt<N>_020000`. The portal ignores names starting with `.`.
2. Also publish `010000` when its gate metrics are better.
3. Write `posttrain/mac/CANDIDATE.json` and `rounds/<N>.json`.
4. Append a `[MAC] … POSTTRAIN round N` entry to `UPDATE.md` with the gate table.
5. **Never touch CURRENT.json at this point.**

### 5.9 Promotion (real-arm evidence only)

Read `deploy/runs/results.csv` (Dell-owned). The candidate becomes CURRENT when:
- it has **≥ 10 labelled trials**, and
- its success rate is **≥ CURRENT's** over CURRENT's most recent 10 trials.

When both hold, write `policies/CURRENT.json` with the evidence and log an `UPDATE.md` entry.

If the candidate has ≥ 10 trials and is worse, mark it `rejected` in `CANDIDATE.json`. Its episodes stay in the ledger
and are still used.

---

## 6. Status

| Part | State |
|---|---|
| Contract v1 (`posttrain/CONTRACT.json`), `policies/CURRENT.json` seeded | ✅ Dell, 2026-09-14 |
| Portal recording, labelling, READY, mock isolation, CURRENT preselect | ✅ verified in dry run |
| Human-correction segment on the real arm | ⚠ first real correction to be checked (§4.2) |
| Fine-tune command (`--policy.path`) | ✅ smoke-tested on the Dell (see UPDATE); ⏳ Mac 20-step smoke |
| Mac watcher / conversion / trim / training / gate / publish / promotion | ⏳ to build on the Mac |
| Round 0 (idle-trimmed base, fixes the frozen start) | ⏳ first job once built |
