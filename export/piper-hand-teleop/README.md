# PiPER Hand Teleop

Drive an AgileX PiPER arm with your bare hand in front of a webcam. Move your hand and the gripper follows;
turn your wrist and the tool turns; pinch and it grips. No gloves, markers or controllers.

It runs as an ordinary desktop app on Ubuntu (this README), or as an app inside
[NervusOS](https://github.com/izhangfly/NervusOS-Cambridge), from the same code.

## How it works

```
webcam ──> YOLO26n-pose (find the arm and the hand) ──> MediaPipe hand landmarks on a rotated crop
              │
              └─> metric hand position: depth from the palm's real size (world landmarks) vs its size in pixels
                  wrist roll: the palm frame from five world landmarks, twist about the finger axis
                  pinch: thumb-to-index distance over palm length, in 3-D (tilt doesn't change it)
                                    │
           clutch (engage / hold / re-anchor) + workspace box + anchor slip + outlier gate
                                    │
                    PiPER driver (127.0.0.1:8770) ──CAN──> arm   (servo at 20 Hz, client "handteleop")
```

- **Clutch.** Engaging anchors your hand to where the arm is now; you move it relative to that, so you can
  re-grip at any time. Losing the hand for 300 ms holds the arm and re-anchors.
- **Safety.** Targets stay inside a workspace box; a frame where the hand jumps more than 5 cm or 20° is
  dropped (three in a row holds the arm); the driver adds its own leash, deadman and IK-flip guard.

## Hardware

- AgileX PiPER on a USB-CAN adapter (`can0`, 1 Mbit/s; candleLight/gs_usb adapters work)
- A webcam facing you. Any UVC webcam works. On a **Surface Pro 9** the built-in front camera works
  through the bridge in `surface-camera/` (libcamera plus one sensor-register fix, see below).
- Ubuntu 24.04. 4 CPU cores are enough (vision runs at up to 20 Hz on 2 threads).

## Install

```sh
git clone https://github.com/izhangfly/piper-hand-teleop && cd piper-hand-teleop
sudo ./install.sh
```

It installs the PiPER driver as a service (started on demand), CAN bring-up on hot-plug, ffmpeg, builds
the app with its own Java runtime (needs JDK 21; installed if missing) and puts **Hand Teleop** on your
desktop. On a Surface Pro 9 it also sets up the front camera (build libcamera first:
`surface-camera/build-libcamera.sh`).

Run it from the icon, or: `nervus-standalone handteleop`. Options:
`/opt/nervus/apps/hand-teleop/bin/hand-teleop --standalone [--camera /dev/videoN | --camera-name NAME]`.

## Repository

| Path | What |
|---|---|
| `hand-teleop/` | The app (Kotlin, Compose Desktop): vision, tracking, control, UI. `./gradlew test` runs 38 tests |
| `driver/` | The PiPER driver (Python, piper_sdk): HTTP API on loopback, safety layer, systemd units, and **PiPER Control**, its joint/Cartesian panel as a desktop app |
| `desktop/` | Desktop entry, icon, `nervus-standalone` launcher |
| `surface-camera/` | Surface Pro 9 front camera bridge: libcamera → v4l2loopback "Surface Camera" |
| `nervus-app-sdk/`, `nervus-packaging/`, `nervus-ipc/` | NervusOS app SDK, used when it runs inside NervusOS |

## Surface Pro 9 front camera

The OV5693 front sensor sits behind Intel's IPU6 and has no webcam driver. libcamera captures it and
converts the images in software; the bridge streams them into a v4l2loopback device any app can open. The
kernel driver doesn't yet program `MIPI_CTRL00 (0x4800) = 0x2d`, without which the IPU6 receives no frames;
the bridge writes it over I2C at each start. That fix is from
[linux-surface PR #2171](https://github.com/linux-surface/linux-surface/pull/2171).

## Credits

Author: **Ian Zhang** ([@izhangfly](https://github.com/izhangfly)).

Built on, with thanks:

- [Joeclinton1/hand-teleop](https://github.com/Joeclinton1/hand-teleop) (Apache-2.0) and
  [arXiv 2508.14994](https://arxiv.org/abs/2508.14994) (CC BY 4.0): metric depth from world landmarks,
  roll-only wrist control, 3-D pinch
- [MediaPipe hand landmarks](https://github.com/google-ai-edge/mediapipe) via
  [OpenCV Zoo](https://github.com/opencv/opencv_zoo) (Apache-2.0)
- [Ultralytics YOLO26n-pose](https://github.com/ultralytics/ultralytics) (AGPL-3.0)
- [ONNX Runtime](https://github.com/microsoft/onnxruntime) (MIT), [Compose Multiplatform](https://github.com/JetBrains/compose-multiplatform) (Apache-2.0)
- [AgileX piper_sdk](https://github.com/agilexrobotics/piper_sdk), [libcamera](https://libcamera.org),
  [linux-surface](https://github.com/linux-surface/linux-surface)
- The One Euro filter (Casiez, Roussel and Vogel, CHI 2012)

See `NOTICE.md` for the licences of bundled files.

## Licence

GPL-3.0 (see `LICENSE`): the NervusOS protocol classes it builds with are GPL-3.0. The bundled pose model
is AGPL-3.0 (Ultralytics); see `hand-teleop/src/main/resources/models/NOTICE.md`.
