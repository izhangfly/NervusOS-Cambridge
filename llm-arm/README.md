# LLM Arm

Tell the PiPER arm what to do in plain language ("put the cube in the tray"). The design follows
[piper-astra-jev](https://github.com/RobotKitAI/piper-astra-jev) (Apache-2.0), which AgileX published for
GPT-6 Astra on the PiPER:

1. **Ground**: the model finds the object and the place in the scene image; the calibration turns those
   pixels into table positions, remembered once the fingers hide the object.
2. **Decide**: every step, the model sees the arm's state flags (over_target, object_held, ...), both
   cameras, the editable arm context and its recent decisions, and picks ONE skill: approach_target,
   descend_to_target, close_gripper, lift, move_over_place, lower_to_place, open_gripper, retreat, done.
3. **Govern**: a skill whose preconditions don't hold is refused; skills are bounded motions written here.
4. **Touch**: whether a grasp worked comes from the gripper (fingers stopped part way), not the picture.

GPT-6 Astra goes through OpenAI's Responses API with a strict JSON schema and a reasoning effort (setting
`openai_effort`); DeepSeek through Chat Completions in JSON mode. The arm context is a Markdown file
(`context.md`, overridden by `~/.config/nervus/llm-arm-context.md`), editable in the app.

```
LLM Arm app (NervusOS) ──HTTP──> nervus-llm-arm 127.0.0.1:8796 ──> PiPER driver 127.0.0.1:8770 ──> CAN ──> arm
                                       │  frames                   (client "llm", /api/servo at 10 Hz)
                                       └──> ACT engine 127.0.0.1:8795 /api/camera/{scene,wrist}.jpg
```

## Keys

One file per provider, readable only by you. The service refuses a key file other users can read.

```sh
mkdir -p ~/.config/nervus
install -m 600 /dev/stdin ~/.config/nervus/deepseek.key   # paste the key, Enter, Ctrl-D
install -m 600 /dev/stdin ~/.config/nervus/openai.key
```

Then press **Reload keys** in the app. `DEEPSEEK_API_KEY` / `OPENAI_API_KEY` in the environment work too.
The model names and base URLs are settings (`deepseek_model`, `openai_model`, ...); both providers must
accept images and OpenAI-style tool calls.

## Safety

- The model only names a skill; it never sends a coordinate. Targets come from the grounding, checked
  against the workspace box (x 80..460, y -300..300 mm) and kept above the calibrated table; at most 25
  decisions per request; two missed grasps end the request.
- Moves are straight lines at `speed_mm_s` (default 50, never above 100).
- **Confirm moves** (on by default): each motion waits for Approve in the app.
- STOP ends the request and holds the arm with the motors on. E-STOP is the driver's damped emergency stop.
  If the app stops sending its heartbeat for 2 s, the request stops too.

## Calibration

Grounding needs a fixed scene camera that sees the table, and a calibration from its pixels to the table.
In the app's **Calibrate** tab: jog the gripper tip down onto a spot on the table, click the tip in the
scene image, press **Add point**; do that at four or more spots spread over the table, then **Save**. The
lowest tip height becomes the table height. The same works over HTTP:

```sh
curl -s 127.0.0.1:8796/api/calibrate -d '{"table_z_mm": 95,
  "points": [[120,400, 200,150], [520,400, 200,-150], [140,120, 420,150], [500,130, 420,-150]]}'
```

## Without hardware

```sh
PYTHONPATH=llm-arm/engine python -m nervus_llmarm --sim --demo --port 8797   # scripted model
PYTHONPATH=llm-arm/engine python -m unittest discover -s llm-arm/tests      # 9 tests
```

`--sim` alone uses the real model with a simulated arm, blocks and cameras.
