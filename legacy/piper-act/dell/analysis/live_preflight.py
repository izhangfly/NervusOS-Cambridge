"""Live, motion-free pre-flight: real CAN feedback + real wrist camera -> policy, exactly the portal's classes.
Prints what the policy WOULD command (nothing is sent), inference latency under real CAN load per thread count."""
import sys
import time
from pathlib import Path

import cv2
import numpy as np

SHARED = Path(r"C:\Users\zhy10\Shared\Piper Arm")
OUT = Path(__file__).parent
sys.path.insert(0, str(SHARED / "deploy"))
import piper_policy_portal as portal  # noqa: E402
import torch  # noqa: E402

arm = portal.RealArm("candle", r"D:\Piper-CAN-Teleop\scripts")
arm.connect()
cam = portal.Camera("Dabai DC1", None)
cam.start()
time.sleep(3)
pol = portal.Policy(SHARED / "policies" / "act_pick_place_v1_100000", threads=4)
print(f"arm mode {arm.ctrl_mode()}  camera {cam.fps():.1f} fps  error {cam.error}")

for th in (4, 3, 6, 8):
    torch.set_num_threads(th)
    lat = []
    for i in range(6):
        state, age = arm.read()
        img, cam_age = cam.latest()
        t = time.perf_counter()
        ch = pol.chunk(state, img)
        lat.append((time.perf_counter() - t) * 1000)
    print(f"threads {th}: inference median {np.median(lat[1:]):.0f} ms  max {max(lat[1:]):.0f} ms  (arm age {age*1000:.0f} ms, cam age {cam_age*1000:.0f} ms)")

torch.set_num_threads(4)
state, _ = arm.read()
img, _ = cam.latest()
cv2.imwrite(str(OUT / "preflight_live.jpg"), cv2.cvtColor(img, cv2.COLOR_RGB2BGR))
print(f"live image brightness {img.mean()/255:.2f} (training start frames 0.64-0.65)")
ch = pol.chunk(state, img)
print("current state : " + " ".join(f"{j}={state[j]:7.2f}" for j in portal.JOINTS))
for s in (0, 9, 19, 29):
    print(f"plan step {s:>2}  : " + " ".join(f"{j}={ch[s][i]:7.2f}" for i, j in enumerate(portal.JOINTS)))
d = ch[-1] - np.array([state[j] for j in portal.JOINTS])
print("planned change over 2 s: " + " ".join(f"{j}={d[i]:+.1f}" for i, j in enumerate(portal.JOINTS)))
cam.stop()
arm.close()
