#!/usr/bin/env python3
"""Park the arm, photograph the table, print and draw what the detector finds.
    look.py [--no-move]      writes /tmp/gptarm-explore/look.jpg (annotated), look-raw.jpg and wrist.jpg
"""
import json
import time
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
import cv2

from gptarm import calib, perception, task
from gptarm.arm import Arm
from gptarm.cameras import Overhead, Wrist

out = Path("/tmp/gptarm-explore")
out.mkdir(exist_ok=True)
cam = calib.Camera.load()
arm, ovh, wrist = Arm(speed=20, floor_z_mm=60), Overhead(), Wrist()
w = task.World(arm, ovh, wrist, cam)
if "--no-move" in sys.argv:
    img = ovh.settled(3)
    sc = perception.look(img, cam, arm.state()["joints"], w.cfg.bottle_height)
    w.scene, w.last_frames["overhead"] = sc, img
    print(json.dumps(sc.to_dict()))
else:
    tool, _ = w.tool()
    if tool[2] < w.cfg.travel_z - 15:
        w._go(tool[0], tool[1], w.cfg.travel_z, min_tip_z=tool[2] - 30)
    print(w._look())
    print(json.dumps(w.scene.to_dict()))
    img = w.last_frames["overhead"]
arm.hold()
vis = perception.draw(img, cam, w.scene, w.tool()[0])
mask = perception.arm_mask(cam, arm.state()["joints"], img.shape)
vis[mask > 0] = (vis[mask > 0] * 0.6 + (60, 0, 60)).astype("uint8")
cv2.imwrite(str(out / "look.jpg"), vis)
cv2.imwrite(str(out / "look-raw.jpg"), img)
try:
    cv2.imwrite(str(out / "wrist.jpg"), wrist.frame())
except Exception as e:
    print("wrist:", e)
print(json.dumps(w.state()))
wrist.close()
time.sleep(0.5)
