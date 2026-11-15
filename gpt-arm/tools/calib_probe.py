#!/usr/bin/env python3
"""One pose, three photographs (gripper open, closed, open): what does the overhead camera see change?
    calib_probe.py X Y Z LEAN      writes /tmp/gptarm-explore/probe.jpg (open | closed | change) and prints blobs
"""
import json
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
import cv2
import numpy as np

from gptarm.arm import Arm
from gptarm.cameras import Overhead

x, y, z, lean = (float(v) for v in sys.argv[1:5])
out = Path("/tmp/gptarm-explore")
out.mkdir(exist_ok=True)
arm, ovh = Arm(speed=15, floor_z_mm=60), Overhead()
st = arm.move_tool(x, y, z, lean=lean)
arm.hold()
print("at", json.dumps({k: st[k] for k in ("tool", "lean", "flange", "joints")}), flush=True)
print("open ->", arm.grip(75)); a = ovh.settled(4)
print("closed ->", arm.grip(0)); b = ovh.settled(4)
print("open ->", arm.grip(75)); c = ovh.settled(4)
ga, gb, gc = (cv2.GaussianBlur(cv2.cvtColor(i, cv2.COLOR_BGR2GRAY), (5, 5), 0).astype(np.float32) for i in (a, b, c))
hi, lo = np.maximum(ga, gb), np.minimum(ga, gb)
ratio = (hi - lo) / (hi + 8.0)
back = np.abs(ga - gc) / (np.maximum(ga, gc) + 8.0)
print("noise between the two open photographs: p99 abs", round(float(np.percentile(np.abs(ga - gc), 99)), 1))
for thr in (0.35, 0.5, 0.65):
    mask = ((ratio > thr) & (hi - lo > 14) & (back < 0.2)).astype(np.uint8)
    mask = cv2.morphologyEx(mask, cv2.MORPH_OPEN, np.ones((3, 3), np.uint8))
    n, lab, stats, cent = cv2.connectedComponentsWithStats(mask)
    blobs = sorted([(int(stats[i, 4]), [int(v) for v in cent[i]], int(hi[lab == i].mean()), int(lo[lab == i].mean()))
                    for i in range(1, n) if stats[i, 4] > 40], reverse=True)[:8]
    print(f"ratio>{thr}: (area, centre, brighter, darker):", blobs)
mask = ((ratio > 0.35) & (hi - lo > 14) & (back < 0.2)).astype(np.uint8)
ys, xs = np.nonzero(cv2.morphologyEx(mask, cv2.MORPH_OPEN, np.ones((5, 5), np.uint8)))
if len(xs):
    cx, cy = int(np.median(xs)), int(np.median(ys))
    x0, y0 = max(0, cx - 260), max(0, cy - 260)
    k = lambda im: cv2.convertScaleAbs(im[y0:y0 + 520, x0:x0 + 520], alpha=2.0)
    vis = np.hstack([k(a), k(b), cv2.cvtColor((np.clip(ratio, 0, 1) * 255).astype(np.uint8)[y0:y0 + 520, x0:x0 + 520], cv2.COLOR_GRAY2BGR)])
    cv2.imwrite(str(out / "probe.jpg"), vis)
    print("crop origin", x0, y0)
cv2.imwrite(str(out / "probe-open.jpg"), a)
