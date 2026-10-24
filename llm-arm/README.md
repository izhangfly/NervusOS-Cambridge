# LLM Arm

Tell the PiPER arm what to do in plain language ("put the red block on the blue one"). A vision model
(DeepSeek or OpenAI GPT, switchable in the app) looks at the two cameras and plans with a small set of tools;
this service checks every step and moves the arm itself.

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

- The model has no stop tool and never talks to the arm. Every tool call is validated here: workspace box
  (x 80..460, y -300..300 mm), a floor above the calibrated table (200 mm until calibrated), nudges of at
  most 50 mm, at most 12 tool calls per request.
- Moves are straight lines at `speed_mm_s` (default 50, never above 100).
- **Confirm moves** (on by default): each motion waits for Approve in the app.
- STOP ends the request and holds the arm with the motors on. E-STOP is the driver's damped emergency stop.
  If the app stops sending its heartbeat for 2 s, the request stops too.

## Calibration

Objects are found by colour (red, yellow, green, blue) on the scene camera. To turn pixels into table
positions, jog the gripper tip to four spots on the table, note the pixel where each one is in the scene
image and the arm's x, y there, and send them with the table height (the TCP z when the closed fingertips
touch the table):

```sh
curl -s 127.0.0.1:8796/api/calibrate -d '{"table_z_mm": 95,
  "points": [[120,400, 200,150], [520,400, 200,-150], [140,120, 420,150], [500,130, 420,-150]]}'
```

## Without hardware

```sh
PYTHONPATH=llm-arm/engine python -m nervus_llmarm --sim --demo --port 8797   # scripted model
PYTHONPATH=llm-arm/engine python -m unittest discover -s llm-arm/tests      # 7 tests
```

`--sim` alone uses the real model with a simulated arm, blocks and cameras.
