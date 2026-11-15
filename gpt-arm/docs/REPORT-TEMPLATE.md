# GPT Arm: three language models drive a PiPER arm through one pick-and-return cycle

Research summary of the GPT Arm project. This is the skeleton: the parts that do not depend on results are
written; every placeholder is marked `TODO(results)`. Numbers, tables and figures come from
`gpt-arm/tools/make_report.py` (section numbers below refer to its `REPORT.md`). Sources are digested in
[BACKGROUND.md](BACKGROUND.md); links there.

Author: izhangfly. Date: TODO(results). Bench id: TODO(results). Code: commit TODO(results).

---

## Abstract

We reproduced AgileX's demonstration of GPT-6 Astra driving an AgileX PiPER arm (RobotKitAI's piper-astra-jev)
on our own PiPER, with a fixed overhead camera, a wrist camera and no GPU, and used it to compare three models
reached through Codex CLI and AgentRouter: GPT-6 Astra, GPT-6.1 Sol and GPT-5.6 Sol. Each run is one cycle: put a
bottle into a basket, take it out again and stand it upright where it started. The model sees both camera images
and a few state flags and chooses the next skill from a short list; a governor in code refuses skills whose
preconditions do not hold, and a grasp is confirmed by where the fingers stop. In a second mode the model
outputs the moves itself. Over TODO(results) runs per model and mode, success rates were TODO(results) (Wilson
95% intervals TODO(results)). TODO(results): one sentence on the main difference between models, one on skills
versus direct control, one on the most common failure, one on time and tokens per cycle.

## 1. Setup

| Part | What we used |
|---|---|
| Arm | AgileX PiPER, firmware S-V1.9-0, parallel gripper (about 75 mm open) |
| Arm host | a Dell laptop; the PiPER driver owns CAN and listens on loopback only (127.0.0.1:8770); it plans and smooths motion with its own IK at 100 Hz |
| Overhead camera | a Surface Pro 9's rear camera through libcamera, 1600x1200, above the table, served as JPEG frames |
| Wrist camera | Orbbec Dabai DC1 colour stream, 1280x720, on the gripper |
| Compute | no GPU: perception runs on the CPU, TODO(results): name the detector and its typical time per frame |
| Models | GPT-6 Astra, GPT-6.1 Sol, GPT-5.6 Sol, through Codex CLI and AgentRouter; reasoning effort TODO(results) |
| Service | the GPT Arm backend at 127.0.0.1:8800 and its portal (contract in `gpt-arm/API.md`) |
| Scene | a hair gel bottle, a basket, marked start spots; workspace x 60 to 480 mm, y -320 to 320 mm, z 80 to 600 mm in the arm's base frame |
| Speed | 20% of the driver's speed |

TODO(results): a photograph of the setup with both cameras labelled; the calibration residual on the day of the
bench (`/api/status` `calibration.residual_px`); lighting.

## 2. Method

### 2.1 Architecture

```
instruction --> model (Astra / 6.1 Sol / 5.6 Sol, via Codex CLI -> AgentRouter)
                  ^ images (overhead, wrist) + state flags + arm context + last decisions + last refusal reason
                  v one skill name and target (skills mode), or one move (direct mode), with a short note
               governor: preconditions, workspace, floor, step size  --refused--> reason back to the model
                  v
               skill code: tool point, lean, gripper  -->  PiPER driver (IK, smoothing, guards)  -->  arm
                  v
               perception (overhead + wrist) updates the state; the gripper's stop confirms a grasp
```

### 2.2 Decisions and why

| Decision | Choice | Why |
|---|---|---|
| What the model outputs | a skill name and target in the main mode, never joint angles | the AgileX demonstration's design ("The model only picks which one runs next; it never sends joint angles"); motion quality then depends on code, and the comparison isolates the model's choices |
| A second mode | `direct`: the model outputs tool moves (or joint targets) itself, through the same governor and driver | the published studies that let a model move the arm (RoboCurve, JustRUA) give it end-effector targets; we test whether skills help on a real single arm |
| What the model sees | both camera images and a small set of flags (`gripper`, `holding`, bottle `where` and `upright`, `above`, `height`, steps used and left) | Astra's run in the demonstration judged the scene from the image plus the arm's own state |
| Grasp confirmation | by touch: the fingers stopped wider than 8 mm after a close command | the demonstration's "confirm by touch"; vision cannot see the bottle between closed fingers |
| Tool point and lean | a point 120 mm past the flange, between the pads; where the wrist cannot point the tool straight down, the tool leans about the line between the fingers so a bottle held upright stays upright | taken from piper-astra-jev's "about the fingers" lean |
| Governor refusals | refused skills are not executed; the reason goes back to the model as an observation | in the demonstration's notebook the reason was logged but not shown to the model; Inner Monologue shows that feedback of this kind raises success |
| Decision budget | 40 decisions per cycle, refusals included | a full cycle needs about twice the demonstration's 8 to 9 decisions for one placement, plus room for recoveries |
| Wrist camera | used for perception near the bottle and as a second image for the model | the demonstration names the hidden-target moment at grasp as its main limit and says a wrist camera would remove it |
| Verdict | automatic from the overhead camera at the end of the cycle, checked by the operator | the same rule for every run, decided before the bench |
| Order of runs | models interleaved round by round in one session | RoboCurve lists its non-interleaved runs as a limitation |

TODO(results): add any decision changed during the bench, with the date and the runs it affected.

### 2.3 Skills

TODO(results): the final skill list with each skill's one-line description and governor precondition, as given
to the model (copy from the arm context, `GET /api/context`).

### 2.4 The context given to the model

TODO(results): the arm context text (Markdown) and its hash, the instruction, and the shape of one request.

## 3. Task and protocol

**Instruction.** "Put the hair gel bottle into the basket. Then take it out again and stand it upright on the
table where it started."

**One run is one cycle.** Within the cycle the model may retry after a missed grasp or a knocked-over bottle.
The run ends at the verdict, at the 40-decision budget, at an error, or when the operator stops it.

**Success** requires all three end checks: the bottle entered the basket; it ended within TODO(results) mm of its
start spot; it stands upright. Thresholds were fixed before the first counted run. The operator reviewed
TODO(results) of the verdicts against the overhead image; disagreements are listed in Section 5.

**Stages** (partial progress, reported separately from success): 0 no grasp, 1 grasped, 2 in the basket, 3 taken
out again, 4 complete.

**Trials.** TODO(results) runs per model per mode (target 20, minimum 10), interleaved round by round in a
shuffled model order, start spots rotated over TODO(results) fixed positions and balanced across models. The scene
was reset by hand between runs.

**Exclusions** (declared before the bench): a run that failed before the arm's first motion for an infrastructure
reason was re-run and is listed as excluded. Every run after the first motion counts, including operator stops.

**Recorded per decision**: skill, target, the model's note, whether the governor refused it or the motion failed
and why, LLM seconds, tokens, motion seconds, the state flags the model saw, and both camera images (`run.json`
and step images under `~/gptarm-runs/<id>/`).

**Statistics.** Wilson 95% score intervals for every rate; two-sided Fisher exact tests for pairs (three
comparisons per mode, uncorrected, so read p-values with that in mind); medians and ranges for time and tokens.
With 20 runs, 18/20 against 10/20 is distinguishable (p = 0.014) but 18/20 against 12/20 is not (p = 0.065).

## 4. Results

### 4.1 Success

TODO(results): paste Section 1 of `REPORT.md` (table and `figures/success_rate.png`): runs, successes, rate,
Wilson interval, clean successes and successes that needed a recovery, per model and mode; the pairwise tests.

TODO(results): one paragraph stating what the intervals allow us to say, and what they do not.

### 4.2 How far runs got

TODO(results): Section 2 of `REPORT.md` (stage distribution per model and mode), and one sentence on where
failed runs stopped.

### 4.3 Effort per cycle

TODO(results): Section 3 of `REPORT.md` (decisions, wall time, LLM time, motion time, tokens: median and range;
`figures/effort.png`). State the share of wall time spent waiting for the model.

### 4.4 Refusals and recoveries

TODO(results): Section 4 of `REPORT.md`: refusal rate, failed motions, retries and knock-overs per model and
mode. Which skills were refused most, and why.

### 4.5 Skills versus direct control

TODO(results): Section 5 of `REPORT.md`: for each model run in both modes, success, clean successes, decisions,
time, tokens, refusals and failed motions side by side, with the Fisher p-value. Then one paragraph: did skills
help, for which model, and at what cost in decisions and tokens. Compare with JustRUA's finding that semantic
operations did not beat absolute end-effector targets in simulation.

## 5. Failure analysis

TODO(results): Section 6 of `REPORT.md` (failure taxonomy table), then one paragraph per category that occurred:
what happened, in which layer (the model's choice, perception, skill code, driver, hardware), and one example run
with its step sequence from Section 7 and a frame from its contact sheet.

TODO(results): verdict disagreements found by the operator's review, and what caused them.

TODO(results): `figures/timelines.png` (a typical success, a success after a recovery, one run per failure
kind).

## 6. Discussion

TODO(results): what the results say about the three models on this task; whether the differences come from
choosing skills, reading the images or recovering; how they compare with the AgileX single run, RoboCurve's
Astra results (19/20 on a block-in-bowl task, 2/20 on a puzzle) and JustRUA's harness findings (full history and
early motion stops mattered more than the action vocabulary).

## 7. Limitations

Known before the results:

- One scene, one object, one arm, one session per bench: the rates describe this setup, not manipulation in
  general. JustRUA saw a 7-point swing between rounds of the same configuration in simulation.
- Few trials per model: the intervals in Section 4.1 are wide and only large differences are detectable.
- The skills do much of the work in `skills` mode (heights, approach paths, the lean, grasp confirmation), so
  that mode measures the model's choices and image reading, not its motor control.
- The operator who reset the scene and reviewed verdicts knew which model ran (RoboCurve names the same bias).
- Models were reached through Codex CLI and AgentRouter, which add latency and may alter requests; time per
  decision is not the model's own latency.
- No GPU: perception is slower and simpler than the demonstration's (Grounding DINO, SAM 3 on an RTX 4090).

TODO(results): limitations found during the bench (drift in calibration, lighting, route outages, anything
reset by hand mid-run).

## 8. Related work

**The AgileX demonstration.** RobotKitAI's piper-astra-jev, presented in AgileX's blog post, ran eight single
demonstrations on a PiPER: GPT-6 Astra in one (a cube into a tray), TypeSafe's Jev in seven, with Grounding
DINO, SAM 3 and part geometry for perception. The model picks one of about ten skills from state flags; a
governor checks preconditions, workspace, step size and deviation; grasps are confirmed by the gripper's stop.
The authors state that these are single runs, not a benchmark. We take the architecture and measure it over many
runs.

**JustRUA** (arXiv 2611.16009) takes apart GPT-6 Astra robot-use agents on the RoboDojo simulation benchmark
(42 bimanual tasks, scores 0 to 100 with partial credit, a 111-episode evaluation set) and finds that keeping the
full episode history, stopping each motion early and giving scene geometry matter most, raising the official
harness's score from 29.7 to 50.3. Simulation only, one model, one run per configuration.

**RoboCurve** compared GPT-6 Astra with Claude Fable 5 and 5.1 on bimanual YAM arms, 20 human-graded trials per
model per task on a 0 to 4 stage rubric, reporting completion rate against cost per run and every trial with its
transcript and video. Our report follows its structure.

**SayCan** (Ahn et al., 2022) chose among learned skills by combining a language model's usefulness score with
each skill's affordance value, on 101 instructions in real kitchens, and reported plan success (84%) and execution
success (74%) separately.

**Inner Monologue** (Huang et al., 2022) fed success detection and scene descriptions back to the planner; on a
real tabletop with 10 runs per task, success rose from 20% to 90% with both kinds of feedback.

**ORA** (Hao AI Lab, 2026) gives an open-weight model perception, grasp and planning tools behind a supervisor
that checks every motion, scores one attempt per case with each benchmark's own check, and audits its own
successes (7 of 46 sampled did not do what the instruction asked).

## 9. How to reproduce

1. Hardware: as in Section 1. Calibrate the overhead camera (`POST /api/calibrate`; residual TODO(results) px).
2. Start the driver, the overhead frame server and the GPT Arm service on the arm host (TODO(results): the exact
   commands or service names used on the day).
3. Set the arm context (`POST /api/context`) to the text in Section 2.4.
4. Start the bench: `POST /api/bench/start` with
   `{"models": ["gpt-6-astra", "gpt-6.1-sol", "gpt-5.6-sol"], "runs_per_model": N, "instruction": "..."}`; reset
   the scene by hand and `POST /api/bench/resume` whenever the bench pauses. Run the `direct` mode bench the same
   way (TODO(results): how the mode is selected).
5. Make the report:

   ```bash
   python3 gpt-arm/tools/make_report.py                     # newest bench under ~/gptarm-runs
   python3 gpt-arm/tools/make_report.py --bench bench-<stamp> --exclude <run id> --out <folder>
   python3 -m unittest discover -s gpt-arm/tests             # the generator's own tests
   ```

   It writes `REPORT.md`, `summary.json` and, with matplotlib and Pillow, `figures/`.
6. Raw data: TODO(results): where the run folders for this report are archived.
