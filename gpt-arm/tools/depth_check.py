#!/usr/bin/env python3
"""Move the tool point to each X,Y,Z given and print what the wrist depth camera reports there.
    depth_check.py X Y Z [X Y Z ...]      (stop the GPT Arm service first: it holds the camera)"""
import json
import sys
import time
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from gptarm import depth as D
from gptarm.arm import Arm

v = [float(a) for a in sys.argv[1:]]
arm, dep, mount = Arm(speed=20, floor_z_mm=60), D.WristDepth(), D.Mount.load()
try:
    for i in range(0, len(v), 3):
        arm.move_tool(*v[i:i + 3])
        arm.hold()
        time.sleep(1.0)
        st = arm.raw_state()
        print(v[i:i + 3], json.dumps(D.summary(dep.settled(5), st["pose_current"], mount)), flush=True)
finally:
    dep.close()
    time.sleep(0.5)
