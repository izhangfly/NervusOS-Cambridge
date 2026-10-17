# TRAINING INSTRUCTIONS — PiPER pick-and-place (ACT on the Mac)

For the Mac. Everything here was **checked against the real dataset and the real LeRobot 0.4.4 code on 2026-09-13**, including an actual `lerobot-train` run on this dataset. Nothing below is from memory.

> **This file replaces the training commands in `HANDOFF-VLA.md` and `UPDATE.md`.** Those used `--policy.checkpoints_total_limit`, **which does not exist in LeRobot 0.4.4 and crashes `lerobot-train` on launch**, and passed a folder path as `repo_id`, which was never tested. Use the command in §6.

---

## 0. TL;DR

```bash
cd ~/NervusOS/robots/vla && source .venv/bin/activate

caffeinate -dimsu nohup lerobot-train \
  --dataset.repo_id=local/piper_pick_place \
  --dataset.root="/Users/ianzhang/Shared/Piper Arm/datasets/piper_pick_place" \
  --dataset.image_transforms.enable=true \
  --policy.type=act \
  --policy.device=mps \
  --policy.push_to_hub=false \
  --batch_size=8 \
  --steps=50000 \
  --save_freq=10000 \
  --log_freq=200 \
  --output_dir=outputs/train/act_piper_pick_place_v1 \
  --job_name=act_piper_pick_place_v1 \
  --wandb.enable=false \
  > train_v1.log 2>&1 &

tail -f train_v1.log
```

**Do the pre-flight checks in §4 first.** If the dataset isn't fully synced, this fails with a confusing HuggingFace `401` error.

---

## 1. What is being trained

| | |
|---|---|
| **Robot** | AgileX **PiPER**, single arm, 6 joints + parallel gripper |
| **Camera** | **One wrist camera only** — Orbbec Dabai DC1, RGB 640×480, 15 fps. (The Logitech C270 scene camera is broken and was **not** used.) |
| **Scene** | Black table · **white cube, 5 cm** · **red card, 15 cm × 15 cm** |
| **Recording method** | Kinesthetic drag-teach: a human moved the arm and gripper by hand while joints and video were recorded |
| **Episodes** | 50 demonstrations |

### The task, in order

1. **Start** at rest pose — forearm pointing forward, gripper closed.
2. **Raise** the arm until the wrist camera looks down at the table.
3. **Find the block:**
   - If the camera **already sees the block** → go straight to it.
   - If it **doesn't** → **rotate the base to the RIGHT** until the block comes into view.
4. **Open** the gripper, approach, **grasp** the block.
5. **Lift** it (measured: ~8–19 cm, typically ~15 cm).
6. **Translate LEFT** until above the red card (measured base rotation ~60–120°, typically ~90°).
7. **Lower and release** onto the card.
8. **Close** the gripper and return.

**Success criterion:** the cube rests fully on the card. A 5 cm cube on a 15 cm card means the cube's centre must land within **±5 cm** of the card's centre, a generous tolerance.

### The method: ACT, trained with LeRobot 0.4.4

- **LeRobot 0.4.4** (`huggingface/lerobot`) is the **framework**: dataset format, data loading, training loop, the `lerobot-train` command.
- **ACT** (*Action Chunking with Transformers*) is the **policy/model** inside it, selected with `--policy.type=act`. It comes from the ALOHA paper; the original code is `tonyzhaozh/act`, but **we use LeRobot's implementation, not that repo**.
- ACT sees the wrist image plus the 7 joint positions, and predicts the next **100 actions** at once (an "action chunk"). ~52 M parameters with a ResNet-18 image backbone.
- ACT **does not read the task text.** One model = this one task.

**Why ACT and not a VLA (RDT-1B, π0, OpenVLA):** RDT-1B was ruled out earlier: its T5-XXL encoder is ~45 GB, and fine-tuning requires DeepSpeed/CUDA, which can't run on a Mac. ACT is the proven small model for 50-demo single-task imitation. SmolVLA is the natural next step once this works.

### How `action` was produced (important for deployment)

There was no leader arm, so there is no recorded command stream. Instead:

> **`action[t] = observation.state[t+1]`** — "the joint position to move to next".

At deploy time the policy's output is sent as **joint position targets** (`send_action` → `JointCtrl`). This was verified exactly: 0 mismatches across all 24,570 frames.

---

## 2. The dataset (verified on the Dell)

**Mac path:** `/Users/ianzhang/Shared/Piper Arm/datasets/piper_pick_place`

| Property | Value |
|---|---|
| Format | LeRobot **v3.0** (Mac's 0.4.4 reads it directly) |
| Episodes / frames | **50 / 24,570** |
| fps | **15** |
| Size | **166.4 MB, 204 files** |
| Episode length | min 20.8 s · median 29.8 s · max 55.5 s (episodes 0–4 are the long ones, ~46–55 s) |
| Features | `observation.state` (7) · `action` (7) · `observation.images.wrist` (480×640×3, h264 video) |
| Joint order | `joint1 … joint6, gripper`, normalized: joints −100…100, gripper 0…100 |

### What was checked, and passed

- ✅ Every file readable; no orphan or corrupt files. (One interrupted take was removed before the final count; it is **not** in the 50.)
- ✅ Every episode contains a complete **closed → open → hold → release → closed** gripper sequence.
- ✅ `action[t] == state[t+1]` exactly, all episodes.
- ✅ Video well exposed and changing frame to frame, with no black or frozen segments. Wrist camera at 12.5 fps into the 15 fps dataset gives the expected ~17% repeated frames.
- ✅ Training actually runs on it (§8).

### Quirks you should know about

1. **Search episodes can't be identified from the data.** Every episode rotates right before grasping (13–69°), because the block always sits right of the start heading. A search turn and "block placed further right" look the same in joint angles. The operator reports only **a few** episodes needed a search. **ACT can learn this in principle** (whether the block is visible is in the current image), **but with few examples it may be unreliable.** Test it separately (§9).
2. **Idle time at the start.** Median 1.6 s of stillness before the arm first moves, but **18 of 50 episodes wait over 2 s** (worst: episodes 4, 7, 12, 8, 6 at 5.5–6.7 s). This can teach "sometimes do nothing at the start", which is a classic cause of an ACT policy **freezing at the start**. Not trimmed; see §10 if it happens.
3. **Episode 1 contains a regrasp** (grasp → open mid-carry → grasp again). Kept: a recovery example is useful.
4. **Gripper calibration cap (known, deliberately left alone).** The plugin maps raw gripper 0…68,000 → 0…100, but the gripper physically opens to ~93,000–101,500. Everything above 68,000 reads as 100. **This does not break this task:** holding the 5 cm cube measured raw **52,000–53,600** in every episode, inside the range. So the policy can still command the gripper open wider than the block. It would only matter for objects wider than about 6.8 cm.
5. `traces/` in the dataset folder holds 3D tool paths for visualization. **Training ignores it.** Don't delete it.

---

## 3. Environment and repositories

### What to use

| Item | Version | Source |
|---|---|---|
| Python | 3.11 | via `uv` |
| **LeRobot** | **0.4.4 exactly** | `pip install lerobot==0.4.4` — https://github.com/huggingface/lerobot |
| PyTorch | 2.10.0 (MPS) | installed by LeRobot |
| torchcodec | 0.10.0 | installed by LeRobot on macOS |

**It already exists** at `~/NervusOS/robots/vla/.venv` (verified 2026-09-11: lerobot 0.4.4, torch 2.10.0, MPS available). Check it:

```bash
cd ~/NervusOS/robots/vla && source .venv/bin/activate
python -c "import lerobot, torch; print('lerobot', lerobot.__version__, '| torch', torch.__version__, '| mps', torch.backends.mps.is_available())"
```
Expect `lerobot 0.4.4 | torch 2.10.0 | mps True`.

**Only if it's missing or broken**, rebuild:
```bash
cd ~/NervusOS/robots/vla
uv venv --python 3.11 .venv && source .venv/bin/activate
uv pip install "lerobot==0.4.4"
```

### What NOT to install on the Mac

- ❌ **Don't upgrade LeRobot** (0.6.x exists). The Dell recorded with 0.4.4; keep both machines identical.
- ❌ `lerobot_robot_piper` (the PiPER plugin), `piper_sdk`, the CAN shim: only the Dell needs these, for the robot itself. Training doesn't touch the robot.
- ❌ `tonyzhaozh/act` (original ACT repo), RDT-1B, DeepSpeed, openpi.

**First run needs internet once:** ACT's ResNet-18 loads ImageNet weights from torchvision and caches them in `~/.cache/torch`.

---

## 4. Pre-flight checks (do all four)

### 4a. Syncthing must be completely finished

Wait until Syncthing shows the folder **Up to Date**. Then check the counts match the Dell exactly:

```bash
D="/Users/ianzhang/Shared/Piper Arm/datasets/piper_pick_place"
for s in data meta/episodes videos traces; do printf "%-15s %s\n" "$s" "$(find "$D/$s" -type f | wc -l)"; done
find "$D" -type f | wc -l
du -sh "$D"
```

Expected: `data 50`, `meta/episodes 50`, `videos 50`, `traces 50`, **204 files total**, **~166 MB**.

> ⚠️ **Why this matters:** if any metadata file is missing, LeRobot silently falls back to downloading from the HuggingFace Hub, and fails with **`401 Unauthorized` / `RepositoryNotFoundError`**. That error means "not fully synced", not "wrong login".

### 4b. The dataset loads

```bash
python - <<'EOF'
from lerobot.datasets.lerobot_dataset import LeRobotDataset
root = "/Users/ianzhang/Shared/Piper Arm/datasets/piper_pick_place"
ds = LeRobotDataset("local/piper_pick_place", root=root)
print("episodes", ds.meta.total_episodes, "frames", ds.meta.total_frames, "fps", ds.fps)
print("cameras", ds.meta.video_keys)
s = ds[100]
print({k: tuple(v.shape) for k, v in s.items() if hasattr(v, "shape")})
EOF
```

Expect `episodes 50 frames 24570 fps 15`, `cameras ['observation.images.wrist']`, and an image shaped `(3, 480, 640)`.

### 4c. Disk space

Each checkpoint is **591 MB** (197 MB model + 394 MB optimizer state). With `--save_freq=10000` over 50,000 steps that's **5 checkpoints ≈ 3 GB**. Keep ≥ 10 GB free (`df -h ~`).

### 4d. Power

**Plug in the charger.** Turn off Low Power Mode. Training on battery is throttled, and sleep kills the run. The `caffeinate` in the command prevents sleep only while the run is alive.

---

## 5. Parameters: what to pick and why

**Principle: change the data before the hyperparameters.** With 50 demos, almost every failure is a data problem. Start from ACT defaults, which were designed for exactly this scale.

| Flag | Value | Why |
|---|---|---|
| `--policy.type` | `act` | See §1 |
| `--policy.device` | `mps` | Apple GPU. Verified working with ACT earlier |
| `--policy.push_to_hub` | **`false`** | **Required.** Defaults to `true`, which demands `--policy.repo_id` and aborts (`'policy.repo_id' argument missing`) |
| `--dataset.repo_id` | `local/piper_pick_place` | Must look like `owner/name`; it's only a label here |
| `--dataset.root` | the Mac path | Where the data actually lives. **This** is what makes it load locally |
| `--dataset.image_transforms.enable` | **`true`** | Random brightness, contrast, saturation, hue and sharpness per frame. **Worth it here:** only 50 demos, and exposure on the black table varies. Cheap protection against lighting changes at deploy |
| `--batch_size` | `8` | ACT default; benchmarked on this Mac. Use `4` if you hit an MPS out-of-memory error |
| `--steps` | **`50000`** | 24,570 frames ÷ 8 ≈ 3,070 steps per pass → 50k ≈ 16 passes. That's already more samples than the original ACT recipe used for 50 demos. LeRobot's default of 100k is more than needed here. Resume to go further (§7) |
| `--save_freq` | `10000` | Checkpoints at 10k, 20k, 30k, 40k, 50k to compare **on the robot**. Default 20k is too coarse |
| `--log_freq` | `200` | Default |
| `--wandb.enable` | `false` | Default; no account needed |

### Leave at ACT defaults (verified values in 0.4.4)

| Setting | Default | Note |
|---|---|---|
| `chunk_size` | **100** | At 15 fps = **6.7 s** of predicted motion per inference. Keep it: it lets deployment choose how often to re-plan (see below) |
| `n_action_steps` | 100 | **Deploy-time** choice; doesn't change training |
| `vision_backbone` | resnet18 (ImageNet) | |
| `dim_model` / `n_heads` / `dim_feedforward` | 512 / 8 / 3200 | |
| `n_encoder_layers` / `n_decoder_layers` | 4 / 1 | |
| `use_vae` / `latent_dim` / `kl_weight` | True / 32 / 10.0 | The VAE handles demos that differ (e.g. search vs direct) |
| `optimizer_lr` / `optimizer_lr_backbone` | 1e-5 / 1e-5 | |
| `optimizer_weight_decay` | 1e-4 | |
| `dropout` | 0.1 | |
| `temporal_ensemble_coeff` | None | Deploy-time option |
| `use_amp` | False | Leave off on MPS |

### About chunk size and your 15 fps (read before deployment)

ACT was designed at 50 fps, where 100 steps = 2 s. At **15 fps, 100 steps = 6.7 s**. If the robot executes whole chunks (`n_action_steps=100`), it acts **6.7 s open-loop**. That's too long to react when the block comes into view during a search. Handle it **at deployment**, not now:

- **Re-plan more often:** set `n_action_steps` to ~10–25 (0.7–1.7 s per chunk). Must be ≤ `chunk_size`.
- **Temporal ensembling:** `temporal_ensemble_coeff=0.01` **requires** `n_action_steps=1`, i.e. one inference every frame (66 ms budget at 15 fps). Probably too slow on the Dell's CPU; measure first.

Only if the trained policy is sluggish even with frequent re-planning: retrain with `--policy.chunk_size=50 --policy.n_action_steps=50` (≈ 3.3 s).

---

## 6. Start training

```bash
cd ~/NervusOS/robots/vla && source .venv/bin/activate

caffeinate -dimsu nohup lerobot-train \
  --dataset.repo_id=local/piper_pick_place \
  --dataset.root="/Users/ianzhang/Shared/Piper Arm/datasets/piper_pick_place" \
  --dataset.image_transforms.enable=true \
  --policy.type=act \
  --policy.device=mps \
  --policy.push_to_hub=false \
  --batch_size=8 \
  --steps=50000 \
  --save_freq=10000 \
  --log_freq=200 \
  --output_dir=outputs/train/act_piper_pick_place_v1 \
  --job_name=act_piper_pick_place_v1 \
  --wandb.enable=false \
  > train_v1.log 2>&1 &
```

- `caffeinate -dimsu` stops the Mac sleeping for as long as the run lives.
- `nohup … &` keeps it running if the terminal closes.
- Watch: `tail -f ~/NervusOS/robots/vla/train_v1.log`
- Find it later: `pgrep -fl lerobot-train`
- Stop it: `pkill -f lerobot-train` (the last saved checkpoint is kept)

**`--output_dir` must not already exist**, or training refuses to start (`FileExistsError … resume is False`). For a new run, bump the name (`_v2`); to continue, see §7.

---

## 7. Monitoring and resuming

### Reading the log

A real line from this dataset:
```
step:4 smpl:8 ep:0 epch:0.00 loss:49.194 grdn:927.284 lr:1.0e-05 updt_s:5.091 data_s:0.157
```

| Field | Meaning |
|---|---|
| `step` | optimizer steps done (target 50,000) |
| `smpl` / `epch` | samples seen / passes over the dataset |
| `loss` | ACT loss = action L1 error + `kl_weight` × KL term. **Should fall steeply**, then flatten |
| `grdn` | gradient norm. Large early, should settle |
| `updt_s` | seconds per training step (the GPU part) |
| `data_s` | seconds loading data. Should stay far below `updt_s` |

**Speed / ETA:** after a few hundred steps, time per step ≈ `updt_s + data_s`. ETA ≈ 50,000 × that. Expect roughly **1.2–2 steps/s**: an earlier run on this Mac did 1.18 steps/s with *two* 480×640 cameras, and this dataset has one. **Roughly 7–12 hours for 50k.**

**Healthy:** the step-1 loss is ~90. It should drop by well over an order of magnitude within the first few thousand steps, then keep creeping down.

**Unhealthy, so stop and investigate:** `nan` loss; loss rising steadily; `data_s` comparable to `updt_s` (data loading is the bottleneck, see §8).

### Checkpoints

```
outputs/train/act_piper_pick_place_v1/checkpoints/
  010000/  020000/  …  050000/
  last -> 050000          (symlink to the newest)
    pretrained_model/     ← the policy (197 MB): config.json, model.safetensors, normalizer/unnormalizer
    training_state/       ← optimizer + RNG (394 MB), only needed to resume
```

### Resume after an interruption (crash, reboot, `pkill`)

```bash
cd ~/NervusOS/robots/vla && source .venv/bin/activate
caffeinate -dimsu nohup lerobot-train \
  --config_path=outputs/train/act_piper_pick_place_v1/checkpoints/last/pretrained_model/train_config.json \
  --resume=true \
  >> train_v1.log 2>&1 &
```

It continues from the last saved checkpoint. Everything since then is redone. To train **beyond** 50k (e.g. if the 50k checkpoint is still improving on the robot), add `--steps=80000` to the resume command. Then check the log's `cfg.steps=` line shows the new value.

---

## 8. Known issues and fixes (all found for real on this project)

| Symptom | Cause | Fix |
|---|---|---|
| `error: unrecognized arguments: --policy.checkpoints_total_limit` | **The flag doesn't exist in 0.4.4.** It was wrongly in the old handoff commands | Remove it. Control disk use with `--save_freq` |
| `'policy.repo_id' argument missing` | `push_to_hub` defaults to `true` | `--policy.push_to_hub=false` |
| `401 Client Error` / `RepositoryNotFoundError` | Dataset metadata incomplete → LeRobot falls back to the HuggingFace Hub | Syncthing not finished. Redo §4a |
| `Output directory … already exists and resume is False` | Re-using an output folder | New `--output_dir`, or resume (§7) |
| `OSError: [WinError 1314]` creating `checkpoints/last` | Windows needs admin rights for symlinks | **Dell/Windows only.** macOS is fine. (Seen in the smoke test on the Dell) |
| Video decode errors | torchcodec problem | Add `--dataset.video_backend=pyav` |
| `objc … Class AVFFrameReceiver is implemented in both …` | Two FFmpeg copies (Homebrew + PyAV) | Harmless warning, seen before on this Mac. Ignore |
| MPS out of memory | Batch too large | `--batch_size=4` (double `--steps` to keep the same number of samples) |
| `NotImplementedError: … not currently implemented for the MPS device` | An op missing on MPS | Prefix the command with `PYTORCH_ENABLE_MPS_FALLBACK=1` |
| DataLoader worker crashes / hangs on macOS | Multiprocessing + video decoding | `--num_workers=2`, or `0` |
| Run dies overnight | Mac slept | Use `caffeinate -dimsu`, keep the charger in, then resume (§7) |

**Don't edit the dataset folder while training.** Syncthing syncs both ways. Nothing on the Mac should write inside `datasets/`.

### Proof the command works

On 2026-09-13 the Dell ran `lerobot-train` against this exact dataset. It used `local/piper_pick_place` + `--dataset.root` + `image_transforms.enable=true` + `push_to_hub=false`, on CPU. Result: dataset loaded offline (`num_frames=24570`, `num_episodes=50`), policy built (`num_learnable_params=51599239`), 4 training steps with loss **93.6 → 67.8 → 58.4 → 49.2**, checkpoint saved. The only failure afterwards was the Windows-only symlink error above.

---

## 9. After training: pick a checkpoint and hand it back

**The training loss is not a reliable way to pick a checkpoint** for real-robot tasks. Choose by **success rate on the robot**.

### Hand checkpoints to the Dell

Copy **only `pretrained_model/`** (197 MB) of the checkpoints to test. Skip `training_state/` (394 MB, only for resuming):

```bash
RUN=~/NervusOS/robots/vla/outputs/train/act_piper_pick_place_v1/checkpoints
DEST="/Users/ianzhang/Shared/Piper Arm/policies"
mkdir -p "$DEST"
for s in 020000 030000 040000 050000; do
  mkdir -p "$DEST/act_pick_place_v1_$s"
  cp -R "$RUN/$s/pretrained_model/." "$DEST/act_pick_place_v1_$s/"
done
du -sh "$DEST"/*
```

Then log a `[MAC] … DONE` entry in `UPDATE.md` with the checkpoint names, final loss, and training time.

### Deployment is a Dell job, and not built yet

A deploy script **does not exist yet**. Whoever builds it needs to know:

1. **Leave drag-teach mode first.** In TEACHING mode the arm **ignores every command** (README §4). Press the teach button so the light goes off.
2. The policy outputs **joint position targets** in the normalized units above. The checkpoint contains the normalizer and unnormalizer. Send with `send_action` → `JointCtrl`, with torque **enabled**.
3. **Safety:** set `max_relative_target` to limit per-step motion; start slow; keep a hand on the e-stop. The arm's speed limits are in README §4.
4. **Re-plan frequency** (§5): measure policy inference time on the Dell CPU first, then choose `n_action_steps`.
5. **Keep the scene identical to recording:** same camera mount and angle, same black table, same lighting, **red card to the left, block to the right of the start heading**. The policy has never seen any other layout.

### Evaluation protocol

For each checkpoint, **10 trials of each** case, recording successes:

| Case | Setup |
|---|---|
| **A. Direct** | Block placed where the wrist camera sees it once the arm rises |
| **B. Search** | Block placed further right, **out of view** when the arm rises |

Success = cube fully on the card **and** released. Pick the checkpoint with the best combined score. **Report A and B separately**: they fail for different reasons.

---

## 10. If results are bad: diagnose in this order

| Behaviour | Most likely cause | Fix (data first) |
|---|---|---|
| **Freezes / hesitates at start** | 18 episodes idle > 2 s at the start (§2) | Delete the worst idle episodes, or record replacements that start moving promptly. `lerobot-edit-dataset --operation.type delete_episodes --operation.episode_indices "[4, 7, 12, 8, 6]"` writes a **copy**; train on that. 0.4.4 has **no trim** operation; trimming would need a custom script on the Dell |
| **Good at A, fails B (won't search)** | Too few search demos | Record **10–20 more search episodes** (block out of view to the right). The recorder appends to the same dataset. Retrain |
| Searches the wrong way / fails if block is left | Data only ever shows "turn right" | Expected. Place blocks to the right, or add left-side demos |
| Reaches right place but **misses the grasp** | Wrist camera is blind at the grasp instant (gripper occludes the view) | More demos with a slow, deliberate grasp; longer term, **fix the C270 scene camera**. Adding it means a **new dataset** |
| Jerky or overshooting | Long open-loop chunks | Deploy with smaller `n_action_steps`, or temporal ensembling |
| Only works at block positions from training | Too little variety | More demos with block positions spread across the workspace |
| Sluggish even with frequent re-planning | 6.7 s chunks at 15 fps | Retrain with `--policy.chunk_size=50 --policy.n_action_steps=50` |

**Hyperparameters are the last resort.** Twenty more good demonstrations usually beat any setting change.

---

## Appendix: where things are

| What | Mac | Dell |
|---|---|---|
| Dataset | `~/Shared/Piper Arm/datasets/piper_pick_place/` | `C:\Users\zhy10\Shared\Piper Arm\datasets\piper_pick_place\` |
| Trained policies (hand-back) | `~/Shared/Piper Arm/policies/` | `C:\Users\zhy10\Shared\Piper Arm\policies\` |
| Training env | `~/NervusOS/robots/vla/.venv` | — |
| Training runs | `~/NervusOS/robots/vla/outputs/train/` | — |
| Recorder | — | Desktop `PiPER Recorder.bat` (`D:\Piper-CAN-Teleop\scripts\piper_record_portal.py`) |
| Hardware notes | `~/Shared/Piper Arm/README.md` | same folder |
| Live change log | `~/Shared/Piper Arm/UPDATE.md` | same folder |
