# Models bundled with Hand Teleop

| File | Model | Source | License |
|---|---|---|---|
| `yolo26n-pose_320.onnx` | Ultralytics YOLO26n-pose, exported to ONNX at 320x320 (opset 17, `ultralytics` 8.4.178) | https://github.com/ultralytics/ultralytics | AGPL-3.0 (Ultralytics). Distributing it in a product needs AGPL compliance or an Ultralytics enterprise licence. |
| `hand_landmark.onnx` | MediaPipe hand landmark model (2023), ONNX conversion from OpenCV Zoo (`handpose_estimation_mediapipe_2023feb.onnx`) | https://github.com/opencv/opencv_zoo/tree/main/models/handpose_estimation_mediapipe | Apache-2.0 |

To regenerate the pose model:

```sh
pip install ultralytics onnx onnxslim
yolo export model=yolo26n-pose.pt format=onnx imgsz=320 opset=17 simplify=True
```
