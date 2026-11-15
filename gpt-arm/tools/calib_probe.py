#!/usr/bin/env python3
"""Does opening and closing the gripper show up in the overhead camera? One pose, two photos, the difference."""
import sys, json
from pathlib import Path
sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
import cv2, numpy as np
from gptarm.arm import Arm
from gptarm.cameras import Overhead

x, y, z, lean = (float(v) for v in sys.argv[1:5])
out = Path("/tmp/gptarm-explore"); out.mkdir(exist_ok=True)
arm, ovh = Arm(speed=15, floor_z_mm=100), Overhead()
st = arm.move_tool(x, y, z, lean=lean)
print("at", json.dumps({k: st[k] for k in ("tool", "lean", "flange", "joints")}), flush=True)
print("open ->", arm.grip(75)); a = ovh.settled(5)
print("closed ->", arm.grip(0)); b = ovh.settled(5)
arm.grip(75); arm.hold()
d = cv2.absdiff(a, b).max(axis=2)
d = cv2.GaussianBlur(d, (5, 5), 0)
mask = (d > 28).astype(np.uint8)
mask = cv2.morphologyEx(mask, cv2.MORPH_OPEN, np.ones((3, 3), np.uint8))
n, lab, stats, cent = cv2.connectedComponentsWithStats(mask)
blobs = sorted([(int(stats[i, 4]), [round(float(c), 1) for c in cent[i]]) for i in range(1, n) if stats[i, 4] > 40], reverse=True)[:6]
print("diff blobs (area, centroid px):", blobs)
ys, xs = np.nonzero(mask)
if len(xs):
    print("all changed pixels: n", len(xs), "centroid", round(float(xs.mean()), 1), round(float(ys.mean()), 1), "bbox", int(xs.min()), int(ys.min()), int(xs.max()), int(ys.max()))
    x0, y0, x1, y1 = max(0, xs.min() - 120), max(0, ys.min() - 120), xs.max() + 120, ys.max() + 120
    vis = np.hstack([a[y0:y1, x0:x1], b[y0:y1, x0:x1], cv2.cvtColor((mask[y0:y1, x0:x1] * 255), cv2.COLOR_GRAY2BGR)])
    cv2.imwrite(str(out / "probe.jpg"), vis)
cv2.imwrite(str(out / "probe-open.jpg"), a)
