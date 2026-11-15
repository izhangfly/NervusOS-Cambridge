#!/usr/bin/env python3
"""First look at the hardware: home, point the tool down, open and close the gripper, photograph each state.
Slow and inside the workspace; nothing is near the table (tool point 250 mm up)."""
import sys, time, json
from pathlib import Path
sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
import cv2
from gptarm.arm import Arm
from gptarm.cameras import Overhead, Wrist

out = Path("/tmp/gptarm-explore"); out.mkdir(exist_ok=True)
arm, ovh, wrist = Arm(speed=15, floor_z_mm=100), Overhead(), Wrist()
def snap(tag):
    cv2.imwrite(str(out / f"{tag}-overhead.jpg"), ovh.settled())
    cv2.imwrite(str(out / f"{tag}-wrist.jpg"), wrist.frame())
    print(tag, json.dumps(arm.state()), flush=True)
snap("0-start")
arm.home(); snap("1-home")
arm.move_tool(250, 0, 250); snap("2-down")
print("open ->", arm.grip(75)); snap("3-open")
print("close ->", arm.grip(0)); snap("4-closed")
print("open ->", arm.grip(75))
arm.hold()
