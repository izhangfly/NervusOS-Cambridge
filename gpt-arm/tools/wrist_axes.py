#!/usr/bin/env python3
"""Which way is which in the wrist picture? Photograph a standing bottle's cap from above, then move the tool
+dx along x and +dy along y and see where the cap goes. Prints the image axes in the arm's frame.
    wrist_axes.py X Y [Z] [D]     the cap's xy, look height (default 250), step (default 30)
Needs the GPT Arm service stopped (the wrist camera opens once)."""
import json
import math
import sys
import time
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
import cv2
import numpy as np

from gptarm.arm import Arm, tool_from_flange
from gptarm.cameras import Wrist


def cap(img):
    """Centre (px) of the largest dark round blob surrounded by yellow: the cap seen from above."""
    hsv = cv2.cvtColor(img, cv2.COLOR_BGR2HSV)
    yellow = cv2.inRange(hsv, (12, 90, 90), (40, 255, 255))
    v = hsv[..., 2]
    dark = ((v < 0.55 * np.percentile(v, 90)) & (hsv[..., 1] < 90)).astype(np.uint8) * 255
    n, lab, st, cen = cv2.connectedComponentsWithStats(cv2.morphologyEx(dark, cv2.MORPH_OPEN, np.ones((5, 5), np.uint8)))
    best = None
    for i in range(1, n):
        a = st[i, cv2.CC_STAT_AREA]
        w, h = st[i, cv2.CC_STAT_WIDTH], st[i, cv2.CC_STAT_HEIGHT]
        if a < 800 or max(w, h) > 2.0 * min(w, h):
            continue
        blob = (lab == i).astype(np.uint8)
        ring = cv2.dilate(blob, np.ones((31, 31), np.uint8)) & ~cv2.dilate(blob, np.ones((5, 5), np.uint8)) & 1
        near_yellow = (yellow[ring > 0] > 0).mean() if ring.any() else 0.0
        if near_yellow > 0.25 and (best is None or a > best[0]):
            best = (a, cen[i])
    return None if best is None else best[1]


x, y = float(sys.argv[1]), float(sys.argv[2])
z = float(sys.argv[3]) if len(sys.argv) > 3 else 250.0
d = float(sys.argv[4]) if len(sys.argv) > 4 else 30.0
arm, wrist = Arm(speed=20, floor_z_mm=60), Wrist()
out = {}
try:
    for name, (px, py) in (("here", (x, y)), ("+x", (x + d, y)), ("+y", (x, y + d)), ("back", (x, y))):
        st = arm.move_tool(px, py, z, yaw_deg=math.degrees(math.atan2(y, x)))
        arm.hold()
        time.sleep(1.2)
        img = wrist.frame()
        cv2.imwrite(f"/tmp/gptarm-explore/axes-{name}.jpg", img)
        out[name] = {"cap_px": None if cap(img) is None else [round(float(v), 1) for v in cap(img)],
                     "lean": st["lean"], "yaw": tool_from_flange(st["flange"])[2]}
        print(name, out[name], flush=True)
finally:
    wrist.close()
    time.sleep(0.5)
h, ax_, ay_ = out["here"]["cap_px"], out["+x"]["cap_px"], out["+y"]["cap_px"]
if h and ax_ and ay_:
    # moving the TOOL +x makes the (still) cap move by -x in the camera: image shift per mm of tool motion
    jx = (np.array(ax_) - np.array(h)) / d
    jy = (np.array(ay_) - np.array(h)) / d
    J = np.column_stack([jx, jy])          # d(px)/d(tool mm)
    print(json.dumps({"yaw": out["here"]["yaw"], "px_per_mm_tool_x": jx.round(2).tolist(),
                      "px_per_mm_tool_y": jy.round(2).tolist(), "det": round(float(np.linalg.det(J)), 3)}))
