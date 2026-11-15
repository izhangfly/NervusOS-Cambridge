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
- `verdict`: the automatic check at the end (`ok` = success, `note` = why)

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
- `height`: `carry` | `grasp` | `release` | `between`
- any of `bottle`, `basket`, `start_spot_xy_mm` can be null (not seen yet)

### Run

```json
{"id": "20261115-142233-gpt-6.1-sol", "model": "gpt-6.1-sol", "instruction": "Put the bottle in the basket ...",
 "state": "running", "started": 1794700000.0, "ended": null,
 "outcome": {"success": null, "reason": "", "checks": {"entered_basket": true, "returned_mm": null, "upright": null}},
 "usage": {"llm_calls": 7, "tokens": 75880, "llm_s": 44.0, "motion_s": 61.0},
 "pending": null, "steps": [Step, ...]}
```

`state`: `running` | `done` | `failed` | `stopped`. `outcome.success` is null until the verdict. In list
endpoints `steps` is left out.

## Endpoints

| Method and path | Purpose |
|---|---|
| `GET /api/status` | Everything the portal's main screen needs (below). Poll at 1 Hz. |
| `GET /api/camera/overhead.jpg`, `GET /api/camera/wrist.jpg` | Latest frame. `?annotated=1` on overhead draws detections (bottle, basket, start spot, tool point). 404 with `{"error"}` if there is no frame. |
| `POST /api/run` `{"instruction": str, "model": str}` | Start one run. 409 `{"error"}` if something is running or the arm/cameras aren't ready. Returns `{"ok": true, "id": str}`. |
| `POST /api/stop` | Stop the run or benchmark; the arm holds with motors on. Always accepted. |
| `POST /api/estop` | The driver's emergency stop. Always accepted. |
| `POST /api/home` | Move the arm to its look pose (out of the camera's way). 409 while running. |
| `POST /api/look` | Refresh perception now (photograph, detect). 409 while running. |
| `GET /api/runs?limit=50` | `{"runs": [Run without steps, newest first]}` |
| `GET /api/runs/<id>` | One Run with steps. |
| `GET /api/runs/<id>/<file>` | An image of that run (JPEG). |
| `POST /api/bench/start` `{"models": [str], "runs_per_model": int, "instruction": str}` | Start a benchmark (runs interleaved across models). 409 if busy. |
| `POST /api/bench/resume` | Continue after a pause (the scene was reset by hand). |
| `GET /api/bench` | Bench (below). |
| `POST /api/calibrate` | Run the automatic overhead-camera calibration (the arm moves). 409 while running. |
| `GET /api/context`, `POST /api/context` `{"text": str}` or `{"reset": true}` | The editable arm context (Markdown) given to the model. |

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
(`message` says what to do); the portal shows it prominently with a Resume button.

## Files on disk

Runs live under `~/gptarm-runs/<id>/`: `run.json` (the Run with steps) and the step images. Benchmarks under
`~/gptarm-runs/bench-<stamp>/bench.json` listing run ids. The report generator reads only these.
