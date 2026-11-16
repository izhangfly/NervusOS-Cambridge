# GPT Arm: service API (contract between the backend and the portal)

The service runs on the robot host at `http://127.0.0.1:8800` (loopback only: it moves the arm and has no
login). The portal is a single web page served at `/`, shown in a desktop window. All bodies are JSON.
Times are Unix seconds; lengths are millimetres in the arm's base frame (+x forward, +y left, +z up).

## Objects

### Step (one entry of a run's log)

```json
{"n": 7, "t": 1794700000.2, "kind": "decision",
 "skill": "move_above", "target": "basket", "note": "The bottle is held; carry it over the basket.",
 "ok": true, "detail": "", "llm_s": 6.1, "tokens": 10840, "motion_s": 8.4,
 "state": {"...": "the flags the model was given, see State"},
 "images": {"overhead": "007-overhead.jpg", "wrist": "007-wrist.jpg"}}
```

`kind` is one of:
- `user`: the instruction (`note` holds it)
- `perception`: what the cameras found (`note` is a sentence, `detail` optional)
- `decision`: the model chose `skill` (+ `target`), with its `note`; `ok` false means the governor refused
  it or it failed, and `detail` says why
- `info`, `error`: text in `note`
- `verdict`: the automatic check at the end (`ok` = success, `note` = why). For a free instruction (see
  "Free instructions" below) `ok` is null
- `lesson`: after the verdict, what the run learned (see "Lessons"): `note` holds the lessons, one per line;
  `detail` is optional (for example "2 new, 14 kept in all"); `llm_s` and `tokens` as for a decision

Fields that don't apply are absent or null. `images` names files under the run (see GET file).

### State (the flags given to the model, also shown live)

```json
{"tool_xyz_mm": [250, 0, 180], "gripper": "open", "holding": false,
 "bottle": {"where": "table", "upright": true, "xy_mm": [330, 120], "seen_s_ago": 3.2},
 "basket": {"xy_mm": [300, -260], "seen_s_ago": 3.2},
 "start_spot_xy_mm": [330, 120],
 "above": "bottle", "height": "carry", "steps_used": 7, "steps_left": 33}
```

- `gripper`: `open` | `closed`
- `bottle.where`: `table` | `basket` | `gripper` | `unknown`
- `above`: `bottle` | `basket` | `start_spot` | `nothing`
- `height`: `carry` (travelling height) | `working` (down at the bottle, or where a held bottle is let go) | `between`
- any of `bottle`, `basket`, `start_spot_xy_mm` can be null (not seen yet)

### Run

```json
{"id": "20261115-142233-gpt-6.1-sol", "model": "gpt-6.1-sol", "instruction": "Put the bottle in the basket ...",
 "state": "running", "started": 1794700000.0, "ended": null,
 "outcome": {"success": null, "reason": "", "checks": {"entered_basket": true, "returned_mm": null, "upright": null}},
 "usage": {"llm_calls": 7, "tokens": 75880, "llm_s": 44.0, "motion_s": 61.0},
 "pending": null, "steps": [Step, ...]}
```

`state`: `running` | `done` | `failed` | `stopped`. `outcome.success` is null until the verdict, and stays
null for a free instruction (`outcome.reason` is then `"not_checked"`). In list endpoints `steps` is left out.

Two later additions to Run (both optional: the portal works without them):

```json
{"checked": true, "lessons_used": 7,
 "video": {"file": "video.mp4", "state": "recording", "seconds": 42.0, "size_mb": 4.6}}
```

- `checked`: whether the automatic bottle check applies (true only for the default instruction)
- `lessons_used`: how many lessons from earlier runs the model was given (0 when the run has lessons off)
- `video`: null when the run is not recorded, else `file` (the name under the run, see
  `GET /api/runs/<id>/video.mp4`), `state` `recording` | `ready` | `failed` | `off`, `seconds` (length so
  far, then final), `size_mb`. It goes `recording` -> `ready` (or `failed`) shortly after the run ends.

## Endpoints

| Method and path | Purpose |
|---|---|
| `GET /api/status` | Everything the portal's main screen needs (below). Poll at 1 Hz. |
| `GET /api/camera/overhead.jpg`, `GET /api/camera/wrist.jpg` | Latest frame. `?annotated=1` on overhead draws detections (bottle, basket, start spot, tool point). 404 with `{"error"}` if there is no frame. |
| `POST /api/run` `{"instruction": str, "model": str, "mode": str, "lessons": bool}` | Start one run. `mode` and `lessons` are optional (`mode` defaults to `status.mode`, `lessons` to `status.lessons.enabled`). 409 `{"error"}` if something is running or the arm/cameras aren't ready. Returns `{"ok": true, "id": str}`. |
| `POST /api/stop` | Stop the run or benchmark; the arm holds with motors on. Always accepted. |
| `POST /api/estop` | The driver's emergency stop. Always accepted. |
| `POST /api/home` | Move the arm to its look pose (out of the camera's way). 409 while running. |
| `POST /api/look` | Refresh perception now (photograph, detect). 409 while running. |
| `GET /api/runs?limit=50` | `{"runs": [Run without steps, newest first]}` |
| `GET /api/runs/<id>` | One Run with steps. |
| `GET /api/runs/<id>/<file>` | An image of that run (JPEG). |
| `GET /api/runs/<id>/video.mp4` | The run's video (see "Video"). 404 `{"error"}` until `video.state` is `ready`. |
| `POST /api/bench/start` `{"models": [str], "runs_per_model": int, "instruction": str, "mode": str, "lessons": bool}` | Start a benchmark (runs interleaved across models). `lessons` is optional and defaults to **false**, so benchmark runs start with no memory unless chosen. 409 if busy. |
| `POST /api/bench/resume` | Continue after a pause (the scene was reset by hand). |
| `GET /api/bench` | Bench (below). |
| `POST /api/calibrate` | Run the automatic overhead-camera calibration (the arm moves). 409 while running. |
| `GET /api/context?mode=direct`, `POST /api/context` `{"text": str, "mode": str}` or `{"reset": true, "mode": str}` | The editable arm context (Markdown) given to the model; each mode has its own (`mode` defaults to `direct`). |
| `POST /api/record` `{"on": bool}` | Start or stop a free recording (see "Video"). Allowed while busy. 400 if `on` is not a boolean. |
| `GET /api/recordings` | The finished free recordings (see "Video"). |
| `GET /api/recordings/<id>.mp4` | One free recording, as an attachment. |
| `GET /api/lessons`, `POST /api/lessons` | The lessons (see "Lessons"). |

Errors are `{"error": "sentence"}` with 400 (bad request), 409 (not now) or 500.

### GET /api/status

```json
{"version": "0.1.0", "time": 1794700000.5,
 "arm": {"reachable": true, "enabled": true, "status": "NORMAL", "tool_xyz_mm": [250, 0, 180], "lean_deg": 10,
         "joints_deg": [0, 45, -60, 0, 45, 0], "gripper_mm": 74.6, "holding": false, "moving": false,
         "guard": null, "error": null},
 "cameras": {"overhead": {"ok": true, "age_s": 0.3, "error": null, "size": [1600, 1200]},
             "wrist": {"ok": true, "age_s": 0.1, "error": null, "size": [1280, 720]}},
 "calibration": {"ok": true, "points": 18, "residual_px": 2.1, "when": 1794690000.0},
 "scene": State,
 "models": [{"id": "gpt-6-astra", "label": "GPT-6 Astra"}, {"id": "gpt-6.1-sol", "label": "GPT-6.1 Sol"},
            {"id": "gpt-5.6-sol", "label": "GPT-5.6 Sol"}],
 "model": "gpt-6.1-sol",
 "default_instruction": "Put the hair gel bottle into the basket. Then take it out again and stand it upright on the table where it started.",
 "busy": "run",
 "run": Run or null,
 "bench": Bench summary or null,
 "specs": {"arm": "AgileX PiPER, firmware S-V1.9-0", "driver": "smooth (own IK, 100 Hz)", "speed_pct": 20,
           "workspace_mm": {"x": [60, 480], "y": [-320, 320], "z": [80, 600]},
           "overhead": "Surface Pro 9 rear camera, 1600x1200", "wrist": "Orbbec Dabai DC1, 1280x720",
           "llm_route": "Codex CLI -> AgentRouter"}}
```

`busy`: null | `run` | `bench` | `calibrate` | `home`. `run` is the current or most recent run, with steps.

Later additions to status (optional; the portal hides the matching controls when they are missing):

```json
{"modes": [{"id": "direct", "label": "Direct (model moves the arm)"}, {"id": "skills", "label": "Skills (AgileX style)"}],
 "mode": "direct",
 "recording": {"id": "rec-20261115-142233", "started": 1794700000.0, "seconds": 12.4, "file": "rec-20261115-142233.mp4"},
 "lessons": {"enabled": true, "count": 14}}
```

- `modes`, `mode`: the control modes and the default one (`direct`: the model moves the arm itself)
- `recording`: the free recording in progress, or null (see "Video"). The key is present whenever the
  service can record
- `lessons`: whether new runs read lessons by default, and how many are kept (see "Lessons")

### Bench

```json
{"state": "running", "instruction": "...", "runs_per_model": 10, "started": 1794700000.0,
 "current": {"model": "gpt-5.6-sol", "index": 3, "run_id": "..."},
 "message": "",
 "results": [{"model": "gpt-6.1-sol", "runs": 4, "successes": 3, "rate": 0.75, "mean_steps": 19.5,
              "mean_s": 212.0, "tokens": 812345, "failures": {"missed_grasp": 1}}],
 "run_ids": ["...", "..."]}
```

`state`: `idle` | `running` | `paused` | `done` | `stopped`. `paused` means the scene needs resetting by hand
(`message` says what to do); the portal shows it prominently with a Resume button. A bench may also carry
`mode` and `lessons` (bool), echoing what it was started with.

## Free instructions

The instruction is free text: the model reads it, looks at both cameras and acts, whatever it asks ("Wave
hello", "Point at the basket", "Look around and tell me what is on the table"). Only the default bottle task
(`status.default_instruction`, compared after trimming spaces) is checked automatically. For any other
instruction the run still ends with a `verdict` step, but:

```json
{"kind": "verdict", "ok": null, "note": "The model says it is done. Free instructions are not checked automatically."}
```

(`detail` may repeat the model's last note) and the Run has `"checked": false` and
`"outcome": {"success": null, "reason": "not_checked", "checks": {}}` (`checks` may be empty or all null),
with `state` `done`. A free run can still fail on its own (`state` `failed`, or `outcome.success` false with a
reason such as `step_limit`, `model_unavailable`, `arm_fault`). The model's answer is the `note` of its last
`decision` step whose `skill` is `done`; the portal shows it as "The model says", which is how a question
gets answered.

## Video

Every run is recorded: both cameras side by side in one MP4, each frame captioned with the current step.
While it records, `run.video.state` is `recording` and `seconds` grows; when the run ends the file is
closed and the state becomes `ready` (or `failed`, with the run unaffected). `off` means recording is
switched off on this service. Videos live next to the run: `~/gptarm-runs/<id>/video.mp4`.

`GET /api/runs/<id>/video.mp4` serves it with `Content-Type: video/mp4` and
`Content-Disposition: attachment; filename="<id>.mp4"`, so a click downloads it (the desktop app saves to
`~/Downloads`).

Free recording, outside runs or across several, uses the same camera layout:

- `POST /api/record {"on": true}` starts one (if one is already running it is returned unchanged);
  `{"on": false}` stops and saves it (a no-op when nothing records). Reply:
  `{"ok": true, "recording": {"id": str, "started": float, "seconds": float, "file": str} | null}`,
  the recording in progress after the call (null after a stop)
- `status.recording` is the same object, or null
- `GET /api/recordings` -> `{"recordings": [{"id": str, "started": float, "seconds": float | null, "size_mb": float | null,
  "url": "api/recordings/<id>.mp4"}]}`, newest first (`seconds` may be null when the service does not
  measure the file). `url` is relative to the portal page
- `GET /api/recordings/<id>.mp4`: the file, served like a run's video (attachment, `video/mp4`)

## Lessons

Learning from experience: after a run's verdict, the model writes a few short lessons from what happened
(what was refused, what missed, what worked); later runs are given the kept lessons with their prompt.
A run with lessons on reads them before its first decision and writes new ones after its verdict, as a
step of kind `lesson`; a run with lessons off does neither. `run.lessons_used` says how many it was given.

- `status.lessons = {"enabled": bool, "count": int}`. `enabled` is the default for new runs
- `GET /api/lessons` ->
  `{"text": str, "enabled": bool, "count": int, "items": [{"text": str, "source_run": str | null, "when": float | null, "score": float | null}]}`.
  `text` is every lesson, one per line (what the editor shows); `items` is the same list, newest first,
  with the run that wrote each one (null for a lesson typed by hand), when, and `score`, the service's own
  measure of how useful the lesson has been (higher is better; null if not known)
- `POST /api/lessons` with any of, in one body:
  - `{"enabled": bool}`: the default for new runs
  - `{"text": str}`: replace all lessons, one per line (blank lines dropped; a line that matches an
    existing lesson keeps its source run and score). An empty string leaves none
  - `{"clear": true}`: remove all lessons

  Reply: `{"ok": true, ...the GET body}`. 400 if the body has none of these, or a value of the wrong type
- `POST /api/run {"lessons": bool}` overrides `enabled` for that run. `POST /api/bench/start` takes
  `"lessons"` too, but defaults to false: benchmark runs start with no memory unless chosen

## Files on disk

Runs live under `~/gptarm-runs/<id>/`: `run.json` (the Run with steps), the step images and `video.mp4`. Benchmarks under
`~/gptarm-runs/bench-<stamp>/bench.json` listing run ids. The report generator reads only these.

## Wrist depth and the checker (backend additions)

- `POST /api/calibrate/depth` `{"method": "table"|"bottle"}`: fit where the wrist depth camera sits on the gripper.
  `table` (default): the arm views the bare table from about 18 poses with different tilts and twists; each view
  gives 'up' and the camera's height, which fix the mount. `bottle`: the bottle stands on the table; the arm grips it
  once (to know its position exactly), lets go and looks at its cap from about a dozen poses. 409 while busy.
- `status.cameras.depth`: `{ok, age_s, error, size, calibrated}`; `GET /api/camera/depth.jpg` is the depth picture
  (near warm, far cool, black = no reading, with a millimetre scale and the nearest distance).
- Direct-mode decisions record what the model was given under `state.model_saw`, including `wrist_depth`:
  `{seen, surface_under_tool_z_mm, fingertips_above_it_mm, nearest_object: {top_centre_xy_mm, top_z_mm,
  extent_xy_mm, offset_from_tool_xy_mm}}`.
- Free instructions: when the model says done, the arm parks and a `perception` step "Checker: success|fail|
  impossible. ..." records the checker's verdict; `run.judge` holds it in full (`before, after, changes, status,
  failure_kind, explanation, critique, confidence`). The verdict follows the checker (`outcome.checks.judge`);
  `reason` is the checker's `failure_kind`, or `not_checked` if it could not be asked.
- A run in direct mode starts with an `info` step "Plan: ..." (`run.plan`: `[{goal, done_when}]`).
- Direct mode also gives the model `objects_on_table`: everything on the white table that is dark or colourful
  (`[{label: "O1", xy_mm, size_mm, colour}]`, biggest first, at most 8), drawn as labelled white boxes on the
  overhead picture it sees. `xy_mm` is where the object's middle meets the table.
