#!/usr/bin/env python3
"""Move the tool point somewhere and photograph with both cameras.
    peek.py X Y Z [LEAN]     writes /tmp/gptarm-explore/peek-wrist.jpg and peek-overhead.jpg
"""
import json
import sys
import time
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
import cv2

from gptarm.arm import Arm
from gptarm.cameras import Overhead, Wrist

x, y, z = (float(v) for v in sys.argv[1:4])
lean = float(sys.argv[4]) if len(sys.argv) > 4 else None
out = Path("/tmp/gptarm-explore")
out.mkdir(exist_ok=True)
arm, wrist = Arm(speed=20, floor_z_mm=30), Wrist()
st = arm.move_tool(x, y, z, lean=lean, min_tip_z=min(z, arm.state()["tool"][2]) - 40)
arm.hold()
time.sleep(1.0)
print(json.dumps({k: st[k] for k in ("tool", "lean", "flange", "joints", "gripper_mm")}))
cv2.imwrite(str(out / "peek-wrist.jpg"), wrist.frame())
cv2.imwrite(str(out / "peek-overhead.jpg"), Overhead().settled(2))
wrist.close()
time.sleep(0.5)
