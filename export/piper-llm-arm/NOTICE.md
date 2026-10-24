# Notices

PiPER LLM Arm, copyright Ian Zhang (izhangfly), GPL-3.0.

The decision design (grounding, one named skill per step from state flags, a precondition governor,
grasps confirmed by the gripper) and the skill descriptions are adapted from
[RobotKitAI/piper-astra-jev](https://github.com/RobotKitAI/piper-astra-jev), Apache-2.0, copyright its
authors. Changes: rewritten for an HTTP arm driver and a fixed-camera homography instead of a depth camera
and local detectors; DeepSeek added next to GPT-6 Astra; an editable arm context; a confirmation step.

| Path | Origin | Licence |
|---|---|---|
| `nervus-ipc/` | NervusOS IPC protocol (generated Java classes) | GPL-3.0 |
| `nervus-app-sdk/`, `nervus-packaging/` | NervusOS app SDK and Gradle packaging plugin (NervusOS team) | No licence file upstream; also public in izhangfly/NervusOS-Cambridge |

Runtime dependencies are fetched by pip and Gradle under their own licences (OpenCV Apache-2.0, NumPy
BSD-3-Clause, Compose Multiplatform Apache-2.0, piper_sdk MIT, python-can LGPL-3.0).
