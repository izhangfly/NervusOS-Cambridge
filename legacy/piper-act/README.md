# piper-act

Imitation learning on a real robot arm: an **AgileX PiPER** (6-DoF + gripper) learns to pick up a 5 cm cube and
place it on a 15 cm red square from its wrist camera, using **ACT** (Action Chunking with Transformers) in Hugging Face
[LeRobot](https://github.com/huggingface/lerobot) 0.4.4. A **human-in-the-loop post-training loop** then turns the
robot's own failures, corrected by hand, into new training data, and promotes a new model only when it wins real-arm
trials.

The full story (decisions, the data used, what went wrong and how each problem was fixed) is in
[`docs/HANDOFF.md`](docs/HANDOFF.md).

## Results

| Policy | How it was trained | Real-arm success |
|---|---|---|
| `v1_100000` | 50 hand demonstrations, 100k steps | 2 / 15 (13%) |
| `pt0_020000` | + idle starts trimmed, 20k steps | 1 / 20 (5%) — rejected |
| `pt1_100000` (current) | + 19 hand corrections + 3 clean runs, 100k steps | 7 / 17 (41%) — promoted |
| `pt2_015000` | + 27 hand corrections + 5 clean runs, 20k steps | candidate, untested |

## How it works

```
 Dell (Windows, at the arm)                         Mac (M2 Pro, MPS)
 record demos by drag-teach  ──┐                ┌── train ACT (train_supervisor.py, crash-resume)
 run the policy, correct by  ──┤  Syncthing     ├── post-training loop (tools/posttrain, every 5 min):
 hand when it fails, label   ──┘  shared folder └──   validate → convert → fine-tune → offline gate
 Success/Fail                     + CONTRACT.json       → publish candidate → promote on real trials
```

- **Two machines, one split.** The Mac trains (it has the GPU); a Windows laptop drives the arm over CAN (it has the
  bus and the camera). They share one Syncthing folder and an append-only log; `docs/CONTRACT.json` says which
  machine may write which paths.
- **Data.** Demonstrations are recorded by dragging the arm by hand; the action is defined as the next measured joint
  state (`action[t] = state[t+1]`).
- **Model.** ACT, 51.6 M parameters, ResNet-18 wrist-image encoder, **chunk size 30** (2.0 s at 15 fps, matching
  ACT's ~2 s optimum; the default 100 would have meant 6.7 s open-loop).
- **Post-training.** Hand corrections (HG-DAgger-style) and clean autonomous successes are added; failed policy
  segments are never used. A candidate must not forget the base task and must improve on held-out corrections
  (offline gate), then beat the current model over ≥10 labelled real-arm trials before it becomes the default.

## Repository layout

| Path | What it is | Runs on |
|---|---|---|
| `tools/train_supervisor.py` | Runs `lerobot-train` unattended; resumes from the last *validated* checkpoint after a crash, hang or NaN | Mac |
| `tools/run_act_piper_pick_place_v1.json` | Config of the original 100k-step run | Mac |
| `tools/train_status.sh` | One-glance status of a supervised run | Mac |
| `tools/posttrain/` | Post-training loop: `packages` (validate rollouts), `lerodata` (LeRobot datasets, idle trim), `gate` (offline evaluation), `pipeline` (rounds, publish, promotion), `config` | Mac |
| `tools/posttrain_ctl.py` | CLI: `status`, `tick`, `daemon`, `round-now`, `retry-errors`, `install-agent` | Mac |
| `tools/portal_export.py`, `tools/portal_sessions.py` | Export training metrics (every round) to the dashboard | Mac |
| `portal/` | Training dashboard: static server, systemd unit, ECharts front end with a session archive | Server |
| `dell/scripts/piper_act_portal.py` | Unified Dell portal: dashboard, drag-teach data collection and policy execution on one CAN adapter | Dell |
| `dell/piper_policy_portal.py` | Policy execution engine: async inference, safety clamps, rollout recording, hand-correction capture | Dell |
| `dell/scripts/piper_record_portal.py` | Drag-teach recorder, writing LeRobot datasets plus Cartesian trace sidecars | Dell |
| `dell/scripts/piper_mac_can.py`, `dell/plugin-patch/` | Windows CAN shim for the candleLight adapter, and the one-file patch to the PiPER plugin that installs it | Dell |
| `dell/scripts/`, `dell/vla/`, `dell/analysis/` | Teleop, hardware diagnostics, camera-by-name, and investigation scripts (see [`dell/README.md`](dell/README.md)) | Dell |
| `server/relay.py` | Reverse-proxy relay for the Dell portal through an SSH tunnel (no authentication — do not expose publicly) | Server |
| `bench_act_mps.py` | ACT training/inference benchmark on Apple Silicon | Mac |
| `docs/` | Handoff, post-training spec + contract, training and execution guides, hardware notes | — |

## Running

Environment (Mac): Python 3.11, `lerobot==0.4.4` (pulls torch 2.10).

```bash
python -m venv .venv && source .venv/bin/activate && pip install "lerobot==0.4.4"

# train, supervised (writes outputs/train/<run>/)
python tools/train_supervisor.py --config tools/run_act_piper_pick_place_v1.json --daemon
tools/train_status.sh

# post-training loop
python tools/posttrain_ctl.py status
python tools/posttrain_ctl.py install-agent      # launchd agent, ticks every 5 minutes

# dashboard feed
export PIPER_PORTAL_REMOTE=root@<server>
python tools/portal_sessions.py --once
```

Paths default to the author's machines (`PIPER_SHARED`, `PIPER_POSTTRAIN_LOCAL` override them). Datasets, checkpoints
and logs are not in this repository.

## Not included

Datasets, checkpoints, run logs and rollout packages stay in the shared folder. So does the relay server's address:
`dell/scripts/piper_act_tunnel.bat` reads it from a gitignored `piper_act_tunnel.local.cmd`. The third-party
[WeGo-Robotics/lerobot_robot_piper](https://github.com/WeGo-Robotics/lerobot_robot_piper) plugin is not vendored.
Only its one patched file is here, pinned to the upstream commit in
[`dell/plugin-patch/README.md`](dell/plugin-patch/README.md).

## Docs

| File | Contents |
|---|---|
| [`docs/HANDOFF.md`](docs/HANDOFF.md) | Whole project: decisions, data, runs, failures and corrections, next steps |
| [`docs/AUTOMATED-POST-TRAINING.md`](docs/AUTOMATED-POST-TRAINING.md) + [`docs/CONTRACT.json`](docs/CONTRACT.json) | Post-training loop spec (the contract wins on conflict) |
| [`docs/TRAINING-INSTRUCTIONS.md`](docs/TRAINING-INSTRUCTIONS.md) | Training procedure and dataset audit (originally `TRAINING INSTRUCTIONS.md`) |
| [`docs/EXECUTION.md`](docs/EXECUTION.md) | Running a policy on the arm |
| [`docs/HARDWARE.md`](docs/HARDWARE.md) | PiPER + candleLight + Dabai on Windows (originally the shared folder's `README.md`) |

## Acknowledgements

ACT by Tony Zhao, Vikash Kumar, Sergey Levine and Chelsea Finn; Hugging Face LeRobot; the WeGo-Robotics PiPER plugin;
AgileX Robotics.
