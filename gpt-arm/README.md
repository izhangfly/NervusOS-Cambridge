# PiPER GPT Arm

A large model drives an AgileX PiPER arm from two cameras. The task: pick up a hair gel bottle, put it in a
basket, take it out again and stand it upright where it started. That whole round trip is one **cycle**.
The cycle is the unit of the benchmark that compares models on it. It is checked automatically, by camera
and by touch, not by asking the model.

It reproduces AgileX's GPT-6 Astra demonstration ([piper-astra-jev](https://github.com/RobotKitAI/piper-astra-jev),
[blog](https://global.agilex.ai/blogs/robotics/how-to-control-robotic-arm-with-gpt-6-astra-and-jev-llm-model))
on different hardware, and adds a second way to drive the arm:

| Mode | What the model decides | What the code does |
|---|---|---|
| **direct** (default) | the motion itself: a tool point (x, y, z mm, lean, yaw), six joint angles, or a gripper width, one at a time | checks it (joint ranges, fingertips above the table on the whole path, reach), moves slowly, reports what happened |
| **skills** (AgileX style) | which named skill comes next (`move_above(bottle)`, `lower`, `close_gripper`, `lift`, `stand_up`, ...) | carries the skill out; a governor refuses a skill whose preconditions do not hold and says why |

In both modes the model sees the overhead and wrist pictures and the arm's state at every step. It also
sees what its earlier steps did, refusals included, so it can retry after a missed grasp or a fallen bottle.
Nothing is carried from one run to the next.

```
 Surface Pro 9 (overhead camera)          Dell laptop (robot host, Ubuntu 24.04)
 ┌────────────────────────────┐   SSH    ┌─────────────────────────────────────────────────────────┐
 │ overhead_cam.py            │  tunnel  │ GPT Arm service 127.0.0.1:8800  ◄── portal (desktop app) │
 │ rear camera via libcamera  ├─────────►│  runner: one cycle = fresh world, model loop, verdict    │
 │ 127.0.0.1:8798 /frame.jpg  │          │  perception · calibration · skills / direct executor     │
 └────────────────────────────┘          │      │                         │                         │
                                         │ PiPER driver 127.0.0.1:8770    Codex CLI ──► AgentRouter │
                                         │ (CAN, IK, Ruckig, guards)      (GPT-6.1 Sol, GPT-5.6 Sol)│
                                         │ wrist camera: Orbbec Dabai DC1 (USB)                     │
                                         └─────────────────────────────────────────────────────────┘
```

## What is in here

| Path | What |
|---|---|
| `gptarm/arm.py` | The arm as this project uses it: a tool point between the finger pads, a lean about the finger line where a vertical tool cannot reach, blocking moves, joint-space moves streamed at 25 Hz, the gripper, the floor and ceiling checks |
| `gptarm/calib.py` | Overhead camera calibration with no target: the arm opens and closes its gripper at known points, the camera finds the fingers, a pinhole camera is fitted (about 6 px, 5 mm on the table) |
| `gptarm/depth.py` | The wrist camera's depth (Dabai DC1 through OpenNI2): where the depth camera sits on the gripper (fitted from views of a standing bottle's cap), and what is under the gripper in the arm's millimetres |
| `gptarm/judge.py` | Free instructions: a model compares the table before and after and says success, fail or impossible, with a critique the driving model gets one more go with |
| `gptarm/lessons.py` | Learning from experience: a short list of lessons the model edits after each run (ADD / AGREE / DISAGREE / EDIT with scores, as in ExpeL) and reads before the next |
| `gptarm/recorder.py` | A video of every run: both cameras side by side, captioned with the step, H.264 MP4 |
| `gptarm/perception.py` | The bottle (by its black cap and pale body: standing or lying, and which way it lies) and the basket, in the arm's millimetres |
| `gptarm/direct.py` | Direct mode: what the model is told, the answer's JSON schema, the checks and the executor, the millimetre grid drawn on the overhead picture |
| `gptarm/task.py` | Skills mode: the skills, the governor, the state flags, the editable arm context; also the bookkeeping both modes share (where the bottle is, did it enter the basket) |
| `gptarm/runner.py` | One run (a cycle, recorded step by step with pictures) and a benchmark (models interleaved, pauses when the scene needs a hand) |
| `gptarm/llm.py` | The model client: `codex exec` against AgentRouter, one ephemeral session per decision, images and a strict JSON schema; a scripted stand-in |
| `gptarm/server.py` | The service behind the portal (`API.md`) |
| `gptarm/web/` | The portal: cameras, specs, the live run log, a box for your own instruction, benchmark, past runs, the context editor |
| `gptarm/sim.py` | A pretend arm, table and cameras with the same interfaces, for tests and for trying the portal |
| `overhead/` | The overhead camera server for a Surface Pro 9's rear camera, its systemd unit, and `overhead-proxy` (finds the Surface on Wi-Fi through Syncthing) |
| `desktop/` | The portal as a desktop app (GTK + WebKit) and the service's systemd user unit |
| `tools/` | `drive.py` (one cycle from the command line), `make_report.py` (the benchmark report), `look.py`, `peek.py`, `calib_probe.py`, `mock_server.py` |
| `docs/` | `BACKGROUND.md` (the sources and what they mean for the benchmark), `REPORT-TEMPLATE.md`, portal screenshots |
| `driver/` | The PiPER driver this runs on (stand-alone repository only; in NervusOS-Cambridge it is `hardware/piper`) |

## Hardware

- AgileX PiPER with its two-finger gripper on a USB-CAN adapter (`can0`), at the middle of the long side of
  a white table about 100 x 60 cm
- Wrist camera: the Orbbec Dabai DC1 on the gripper: colour through UVC, depth through OpenNI2 with Orbbec's
  driver (`install.sh` fetches it from Orbbec's ROS 2 package; it needs Orbbec's udev rule)
- Overhead camera: any camera about a metre above the table that can serve JPEGs. Here it is a Surface Pro 9
  running Ubuntu, whose rear camera needs libcamera (`overhead/overhead_cam.py`)
- Robot host: Ubuntu 24.04, no GPU needed (the model runs remotely; perception is plain OpenCV)

## Install

On the robot host, with the PiPER driver running (`driver/README.md`; it listens on 127.0.0.1:8770):

```sh
git clone https://github.com/izhangfly/piper-gpt-arm ~/gpt-arm && cd ~/gpt-arm
./install.sh --python /path/to/python   # a Python with numpy and opencv-python; the driver's venv works
```

On the overhead camera machine (a Surface Pro 9 here), as root:

```sh
install -Dm755 overhead/overhead_cam.py /opt/gptarm/overhead_cam.py
install -Dm644 overhead/gptarm-overhead-cam.service /etc/systemd/system/
systemctl enable --now gptarm-overhead-cam
```

and on the robot host, a tunnel to it (it only listens on loopback): put the camera machine's address in
`~/.config/nervus/overhead.host` (or its Syncthing device ID in `overhead.device`, and `overhead-proxy`
finds it as it moves between networks), add a `Host overhead` entry to `~/.ssh/config` with
`ProxyCommand ~/.local/bin/overhead-proxy %p`, and run
`ssh -N -L 127.0.0.1:8798:127.0.0.1:8798 overhead` as a user service.

The model: install the Codex CLI (`npm install -g @openai/codex`). Put your AgentRouter key in
`~/.config/nervus/openai.key` (mode 600), and this in `~/.config/nervus/codex/config.toml`:

```toml
model_provider = "AgentRouter"
[model_providers.AgentRouter]
name = "AgentRouter"
base_url = "https://agentrouter.org/v1"
env_key = "CODEX_GATEWAY_API_KEY"
wire_api = "responses"
```

AgentRouter only serves its supported clients, which is why the calls go through Codex rather than plain
HTTP. Check the route: `python -m gptarm.llm gpt-5.6-sol`.

## Use

1. **Calibrate** once, and again whenever the overhead camera is moved (and **Calibrate depth** once, with the
   bottle standing on the table, for the wrist depth camera): the portal's Calibrate button, or
   `python -m gptarm.calib`. The arm visits about 40 points, high ones first. A point near something
   dark on the table is skipped, so nothing needs clearing. It takes about 10 minutes.
2. Open **GPT Arm** on the desktop. Choose a model and a mode, type an instruction (or keep the default
   task), press Run. The log shows every step: the pictures the model saw, its note, the motion, and what
   happened. STOP holds the arm where it is; E-STOP is the driver's emergency stop.
3. **Benchmark**: choose models, mode, runs per model, and whether runs may learn from each other (off by
   default, so the runs are comparable). Runs alternate between models. Each starts only when the overhead camera
   sees the bottle standing on the table; after a failed run the benchmark pauses and asks you to stand it up
   again, then Resume. (`auto_tidy` in `~/.config/gptarm/config.json` makes it try a scripted, model-free reset
   first.) A successful run ends with the bottle back where it started, so the next one needs nothing.
4. **Report**: `python tools/make_report.py` writes `REPORT.md` and figures from `~/gptarm-runs`. It gives
   success rates with Wilson intervals, time and tokens per cycle, recoveries, failure kinds, and skills
   against direct.

Without the arm: `python -m gptarm.server --sim` serves the portal on the simulator, where a "Scripted"
model drives the skills mode with no tokens.

## Any instruction

Type anything in the portal: "wave hello", "point at the basket", "push the basket 10 cm to the left",
"what is on the table?". In direct mode the model first writes a short plan with a checkable condition per
stage, then moves the arm one motion at a time. It sees both pictures every step: the overhead one with a
millimetre grid, the wrist one with a compass and a mark between the fingertips. It also gets the joint
angles and the gripper reading, and the wrist depth camera's measurements: the height of whatever is under
the fingertips, and the position and top height of the nearest object below the gripper. When it says done,
the arm parks and a checker compares the table before and after. If the checker disagrees, its critique
goes back to the model for one more go. Every run is recorded as a video you can download, and with
"Learn from earlier runs" on, the model writes lessons afterwards that the next runs read.

## How a run is judged

At the end (the model says done, or the step budget runs out) the arm parks out of the camera's view and
the overhead camera finds the bottle. If it is on the table, the gripper closes at cap height where the
bottle should stand: a standing bottle stops the fingers, a fallen one is too low to touch. A run succeeds
when the bottle entered the basket at some point, is back on the table within 40 mm of where it started,
and stands. Any other instruction is judged by the checker (`judge.py`): success, or a failure in its own words.
A failure of the bottle cycle is recorded with its reason: `never_entered`, `left_in_basket`, `tipped_over`,
`not_returned`, `step_limit`, `still_holding`, `bottle_lost`, `model_unavailable` or `arm_fault`.

## Safety

The driver and the service listen on loopback only: they move the arm and have no login. Moves are slow:
20 % of the arm's speed, and 25 deg/s for joint moves. A move is refused before it starts if a joint
would leave its range, a fingertip would go below the floor anywhere along the path (20 mm in skills mode,
8 mm in direct mode; the table is 0), the wrist would rise into the overhead camera, or the point is out
of reach. During a move, the driver stops the arm if it lags its plan or the IK would flip. STOP and
E-STOP are always available. Keep a hand near the physical e-stop anyway: a model can choose a bad
motion, and perception can be wrong.

## Tests

```sh
python tests/test_calib.py && python tests/test_task.py && python tests/test_direct.py \
  && python tests/test_runner.py && python tests/test_depth.py && python tests/test_make_report.py
python tools/mock_server.py --port 8811 & python -m unittest tests/test_portal_contract.py
GPTARM_URL=http://127.0.0.1:8812 GPTARM_CONTRACT_MUTATE=1 python -m unittest tests/test_portal_contract.py  # against `server --sim --port 8812`
```

## Credits

Author: **Ian Zhang** ([@izhangfly](https://github.com/izhangfly)).

Borrowed and adapted: the lessons mechanism from [ExpeL](https://github.com/LeapLabTHU/ExpeL) (Apache-2.0);
the checker's prompt from [PragmaBot](https://github.com/leggedrobotics/pragmabot)'s success detector
(BSD-3-Clause), with three outcomes as in [Agent Workflow Memory](https://github.com/zorazrw/agent-workflow-memory)
and a critique loop as in [Voyager](https://github.com/MineDojo/Voyager) (MIT); the OpenNI2 runtime and
Orbbec driver from [ros2_astra_camera](https://github.com/orbbec/ros2_astra_camera) (Apache-2.0), read with
the [openni](https://pypi.org/project/openni/) bindings (MIT).

The skills-mode design (the model picks a named skill, a governor checks preconditions, a grasp counts when
the fingers stop part way, strict JSON answers, the lean about the finger line) is adapted from
[RobotKitAI/piper-astra-jev](https://github.com/RobotKitAI/piper-astra-jev) (Apache-2.0). The PiPER
driver builds on [AgileX piper_sdk](https://github.com/agilexrobotics/piper_sdk) and
[Ruckig](https://github.com/pantor/ruckig). Models are reached through the
[Codex CLI](https://github.com/openai/codex) and AgentRouter.

## Licence

Apache-2.0 (see `LICENSE`).
