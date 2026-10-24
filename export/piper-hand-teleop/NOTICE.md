# Notices

PiPER Hand Teleop, copyright Ian Zhang (izhangfly), GPL-3.0.

Third-party material in this repository:

| Path | Origin | Licence |
|---|---|---|
| `hand-teleop/src/main/resources/models/hand_landmark.onnx` | MediaPipe hand landmark model, ONNX from OpenCV Zoo | Apache-2.0 |
| `hand-teleop/src/main/resources/models/yolo26n-pose_320.onnx` | Ultralytics YOLO26n-pose, exported to ONNX | AGPL-3.0 |
| `nervus-ipc/` | NervusOS IPC protocol (generated Java classes) | GPL-3.0 |
| `nervus-app-sdk/`, `nervus-packaging/` | NervusOS app SDK and Gradle packaging plugin (NervusOS team) | No licence file upstream; also public in izhangfly/NervusOS-Cambridge |

Ideas and methods adapted (no code copied verbatim): Joeclinton1/hand-teleop (Apache-2.0), arXiv 2508.14994
(CC BY 4.0). The Surface camera register fix follows linux-surface PR #2171 (GPL-2.0 kernel patch,
re-implemented here as an I2C write).

Runtime dependencies are fetched by Gradle and pip under their own licences (ONNX Runtime MIT, Compose
Multiplatform Apache-2.0, kotlinx Apache-2.0, protobuf BSD-3-Clause, piper_sdk MIT, python-can LGPL-3.0).
