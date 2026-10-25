# PiPER LLM Arm

Tell an AgileX PiPER arm what to do in plain language: *"put the cube in the tray"*. A vision-language model
(OpenAI GPT-6 Astra or DeepSeek, switchable) looks through the cameras and decides; a deterministic
executor moves the arm, one bounded skill at a time.

It follows the design AgileX published for GPT-6 Astra on the PiPER
([RobotKitAI/piper-astra-jev](https://github.com/RobotKitAI/piper-astra-jev)), rebuilt for a desktop app
and an HTTP arm driver.

## How it works

1. **Ground.** The model reads your request and points at the object and the place in the scene-camera image.
   A 4-point calibration turns those pixels into table positions in the arm's frame. They are remembered,
   because the fingers hide the object once the gripper gets close.
2. **Decide.** At every step the model sees the arm's state as plain flags (`over_target`, `object_held`,
   `at_carry_height`, ...), both cameras, an editable arm context and its recent decisions, and picks one
   skill: `approach_target`, `descend_to_target`, `close_gripper`, `lift`, `move_over_place`,
   `lower_to_place`, `open_gripper`, `retreat`, `done`.
3. **Govern.** A skill whose preconditions don't hold is refused, and the model is asked again. Skills are
   straight-line motions written in code: inside a workspace box, above the table, at a capped speed. The
   model never sends a coordinate to the arm.
4. **Touch.** Whether a grasp worked comes from the gripper (the fingers stopped part way), not the picture.

By default every motion waits for **Approve** in the app. STOP and E-STOP are always on screen, and the arm
stops if the app goes quiet for 2 s.

```
LLM Arm app ──HTTP──> LLM Arm service 127.0.0.1:8796 ──> PiPER driver 127.0.0.1:8770 ──CAN──> arm
                          │  frames: from the ACT engine if it runs, else the cameras directly
                          └──> GPT-6 Astra (OpenAI Responses API, strict JSON schema) or DeepSeek (JSON mode)
```

## Hardware

- AgileX PiPER on a USB-CAN adapter (`can0`)
- A fixed **scene camera** that sees the table (any UVC webcam, mounted high) and optionally a wrist camera
- Ubuntu 24.04

## Install

```sh
git clone https://github.com/izhangfly/piper-llm-arm && cd piper-llm-arm
sudo ./install.sh
install -m 600 /dev/stdin ~/.config/nervus/openai.key     # paste the key, Enter, Ctrl-D
install -m 600 /dev/stdin ~/.config/nervus/deepseek.key
```

Open **LLM Arm** from the desktop. Then:

1. **Calibrate** (tab): jog the gripper tip onto four spots on the table, click the tip in the scene image
   each time, add the point, save.
2. Pick the model at the top, type a request, approve the steps.

Edit what the model is told about your arm in the **Context** tab (`{task}` is replaced by your request).
Cameras are found by part of their name: `--cameras scene=046d_0825,wrist=Dabai_DC1` in the service's
unit (`/etc/systemd/system/nervus-llm-arm.service`).

## Try it without an arm

```sh
cd service
PYTHONPATH=engine python -m nervus_llmarm --sim --demo --port 8797   # simulated arm, blocks and cameras
PYTHONPATH=engine python -m unittest discover -s tests              # 9 tests
cd ../llm-arm && LLM_ARM=http://127.0.0.1:8797 ./gradlew run --args=--standalone   # the app, against it
```

## Repository

| Path | What |
|---|---|
| `service/` | The service (Python): grounding, decider loop, governor, executor, cameras, calibration, tests |
| `llm-arm/` | The app (Kotlin, Compose Desktop) |
| `driver/` | The PiPER driver (Python, piper_sdk): HTTP API on loopback, safety layer, systemd units, and **PiPER Control**, its joint/Cartesian panel as a desktop app |
| `desktop/` | Desktop entry, icon, `nervus-standalone` launcher |
| `nervus-app-sdk/`, `nervus-packaging/`, `nervus-ipc/` | NervusOS app SDK, used when it runs inside NervusOS |

## Credits

Author: **Ian Zhang** ([@izhangfly](https://github.com/izhangfly)).

Design adapted from [RobotKitAI/piper-astra-jev](https://github.com/RobotKitAI/piper-astra-jev)
(Apache-2.0), which AgileX published with
[its guide to GPT-6 Astra on the PiPER](https://global.agilex.ai/blogs/robotics/how-to-control-robotic-arm-with-gpt-6-astra-and-jev-llm-model).
Uses [OpenCV](https://opencv.org), [AgileX piper_sdk](https://github.com/agilexrobotics/piper_sdk) and
[Compose Multiplatform](https://github.com/JetBrains/compose-multiplatform). See `NOTICE.md`.

## Licence

GPL-3.0 (see `LICENSE`).
