# Training on a rented GPU, executing on the Mac

Today the Mac trains ACT policies on its own GPU (MPS) and the Surface records demonstrations and runs
policies. This plan moves training and post-training to a rented cloud GPU, and makes the Mac the place
where policies run (inference), with the Surface as the robot host.

## Why

- **Speed.** ACT on the Mac runs about 1.2 steps/s (20k steps take about 4.7 h). SmolVLA (450M
  parameters) is far heavier: LeRobot quotes about 4 h for 20k steps on one A100, so on a 16 GB Mac it is
  impractical beyond smoke tests.
- **The Mac stays usable.** It has 16 GB and swaps when training and anything else run together.
- **Execution needs low latency, not much compute.** SmolVLA inference fits the Mac's GPU and runs on the
  same LAN as the arm. ACT inference is light enough for the Surface's CPU, as it runs today.

## Architecture

```
            Surface Pro 9 (robot host, Ubuntu)                    Mac (execution + coordination)
 ┌──────────────────────────────────────────────┐        ┌──────────────────────────────────────────┐
 │ PiPER driver 127.0.0.1:8770 ── CAN ── arm      │        │ Mac worker (launchd com.nervus.act-worker)│
 │ ACT engine 127.0.0.1:8795: record, run ACT,    │ Sync-  │   build datasets, gate, publish, promote  │
 │   cameras (Dabai wrist + scene webcam)         │ thing  │   ── ships jobs to the GPU, pulls results │
 │ ACT Studio / LLM Arm / Hand Teleop (desktop)   │◄──────►│ SmolVLA policy server (LeRobot async      │
 │ SmolVLA robot client ◄──── gRPC on LAN ────────┼────────┼──► inference, MPS)                        │
 └──────────────────────────────────────────────┘ ~/Shared└──────────────┬───────────────────────────┘
                                                  /NervusACT              │ SSH + rsync (jobs out,
                                                                          │ checkpoints back)
                                                         ┌────────────────▼───────────────────────────┐
                                                         │ Rented GPU (RunPod / Vast.ai / Lambda)       │
                                                         │   pinned image: LeRobot 0.4.4 + CUDA torch   │
                                                         │   lerobot_train (ACT, SmolVLA), resumable    │
                                                         │   network volume: datasets + checkpoints     │
                                                         └──────────────────────────────────────────────┘
```

Data flow for one round:

1. **Record** on the Surface (ACT Studio). Takes land in `~/Shared/NervusACT/datasets` and reach the Mac
   over Syncthing (LAN, or the relay when away).
2. **Build** on the Mac: the worker trims and checks the takes and builds the training set (as today).
3. **Ship**: the worker rsyncs the dataset and the job spec to the GPU box over SSH and starts
   `lerobot_train` in tmux. Training resumes from the last checkpoint if the box restarts.
4. **Watch**: the worker polls the remote log for steps and loss and mirrors them into the job's status,
   so ACT Studio shows progress exactly as it does now. Hang and slowdown detection carries over.
5. **Pull back** the checkpoints the publish plan wants (second half plus the final one), run the
   existing gate (set A within 1.10x of CURRENT, set B better), and publish to `policies/`.
6. **Run**: ACT runs on the Surface as today. SmolVLA runs as a policy server on the Mac, with the robot
   client on the Surface; the client streams camera frames and joint state and gets action chunks back.
7. **Post-train**: correction episodes recorded during runs go through the same loop (steps 2 to 5).
8. **Stop the GPU** as soon as the job ends, so you only pay for training time.

What changes in the code:

- `act/mac/worker.py` gets a "remote" executor next to the local one: the same job contract, but
  training runs over SSH. The contract version stays 1; only the worker changes.
- PiPER SmolVLA (`smolvla/`) brings `train --remote`, `serve` (Mac) and `run` (Surface), built on
  LeRobot's own `lerobot_train` and `async_inference` rather than new code.
- Nothing on the Surface changes for ACT.

## What to rent

| Option | Good for | Rough price |
|---|---|---|
| **RunPod**, RTX 4090 24 GB (community cloud) | ACT, SmolVLA fine-tuning at batch 32-64 | about $0.35-0.70/h |
| **RunPod / Lambda**, A100 80 GB | Faster SmolVLA, larger batches, pi0-class later | about $1.2-2/h |
| **Vast.ai**, RTX 4090 or 3090 | The cheapest; machines vary, so check reliability scores | about $0.20-0.50/h |

These prices are approximate; check current ones before you book. Pick an EU region for latency and data
location. Start with a 24 GB card: SmolVLA fine-tuning fits, and ACT is small. A typical round (20k SmolVLA
steps on a 4090, plus upload and download) should cost a few dollars.

## What to prepare

1. **An account** with RunPod (simplest) or Vast.ai, with a spending limit set. Add a few dollars of
   credit to start.
2. **A dedicated SSH key** on the Mac for the GPU boxes: `ssh-keygen -t ed25519 -f ~/.ssh/gpu_train`.
   Add the public key in the provider's settings. Never put the private key in `~/Shared`, which syncs to
   every device.
3. **A Hugging Face account and a read token** for downloading `lerobot/smolvla_base`. Keep it in a 0600
   file on the Mac (`~/.config/nervus/hf.token`), never in the repo. The worker passes it to the box as
   an environment variable for the job only.
4. **A network volume** (RunPod) of about 50 GB, so datasets and checkpoints survive stopping the pod and
   the environment isn't reinstalled each time.
5. **A template image**: an official PyTorch CUDA image with LeRobot 0.4.4 installed. The worker can do
   the install on first use; a saved template makes later starts take seconds.
6. **A scene camera** for the Surface (see the camera notes in the chat history: any 1080p UVC webcam is
   enough). VLA policies learn much better with a fixed third-person view than from the wrist alone.
7. **Data**: SmolVLA needs a task sentence per episode ("pick up the white cube and put it in the tray").
   Set it in ACT Studio when you open a dataset. Plan on about 50 good takes per task to start.

## Privacy and safety

- The datasets contain video of your room. Rented machines are other people's computers: use a provider
  with private pods, delete datasets from the volume when a project ends, and don't push datasets to
  public Hugging Face repos.
- The GPU box never gets arm access. It only trains; nothing it produces runs until the Mac's gate has
  passed it and a person starts a run in ACT Studio.
- Keys stay on the Mac. The box receives only what one job needs.

## Steps, in order

1. Done already: the Mac trains ACT, and the Surface records and runs.
2. Rent a 4090 for an hour and run one ACT job by hand over SSH, to measure speed and cost.
3. Add the remote executor to the Mac worker (same contract) and run one round end to end.
4. Fine-tune SmolVLA remotely with PiPER SmolVLA, then serve it on the Mac and run it on the arm with
   the dry run first.
5. Move post-training to the GPU.
6. Later: compare ACT and SmolVLA on the same task, with the success-rate trials ACT Studio already
   records.
