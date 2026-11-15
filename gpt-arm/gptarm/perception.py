"""What is on the table, from the overhead camera: the bottle and the basket, in the arm's millimetres.

The camera is a tablet's rear camera with weak colour, so this does not look for "yellow". On a white table
the two things that matter are both dark: the basket is the big dark shape, the bottle's cap the small round
one. The bottle's pale body shows where it lies against something dark (inside the basket) and as a faint
tint beside its cap. Positions come from the calibration (calib.Camera): a pixel is a ray, and the height
of the thing seen (the cap is on top of a standing bottle) picks the point on it.

The arm hides part of the table. It is masked out where the calibration says it is, and `look` in the skills
parks it to one side first.
"""
from __future__ import annotations

import math
import time
from dataclasses import dataclass, field

import cv2
import numpy as np

from . import arm as A
from . import calib as C

BOTTLE_HEIGHT_MM = 125.0      # table to the top of the cap, standing
BOTTLE_DIAMETER_MM = 50.0
CAP_DIAMETER_MM = 38.0
DARK = 0.42                   # under this fraction of the table's brightness: dark
PALE = 0.62                   # over this fraction, inside the basket: the bottle's body


@dataclass
class Scene:
    t: float = field(default_factory=time.time)
    table_level: float = 0.0
    bottle: dict | None = None      # {"where": "table"|"basket", "xy": [x, y], "upright": bool|None, "px": [u, v]}
    basket: dict | None = None      # {"xy": [x, y], "px": [u, v], "box_px": [x, y, w, h], "size_mm": [a, b]}
    notes: list = field(default_factory=list)

    def to_dict(self):
        return {"t": self.t, "table_level": round(self.table_level, 1), "bottle": self.bottle, "basket": self.basket,
                "notes": self.notes}


def px_per_mm(cam, xyz):
    u = cam.project(xyz)
    v = cam.project(np.asarray(xyz, float) + np.array([10.0, 0.0, 0.0]))
    w = cam.project(np.asarray(xyz, float) + np.array([0.0, 10.0, 0.0]))
    return float(np.linalg.norm(v - u) + np.linalg.norm(w - u)) / 20.0


def arm_mask(cam, joints_deg, shape, width_mm=150.0):
    """The arm's pixels (generously), from its joints: links as thick lines, the gripper and its camera as a disc."""
    mask = np.zeros(shape[:2], np.uint8)
    if joints_deg is None or len(joints_deg) < 6:
        return mask
    pts = A.KIN.points(joints_deg[:6])
    tip = pts["flange"] + pts["R"] @ np.array([0.0, 0.0, A.GRASP_OFFSET_MM + A.TIP_OFFSET_MM])
    chain = [np.array([0.0, 0.0, 0.0]), np.array([0.0, 0.0, 123.0]), pts["elbow"], pts["wrist"], pts["flange"], tip]
    px = [cam.project(p) for p in chain]
    for (p, a), (q, b) in zip(zip(chain, px), zip(chain[1:], px[1:])):
        t = int(max(20, width_mm * px_per_mm(cam, (p + q) / 2.0)))
        cv2.line(mask, tuple(np.round(a).astype(int)), tuple(np.round(b).astype(int)), 255, t)
    for p, a, r in ((chain[4], px[4], 110.0), (chain[5], px[5], 80.0), (chain[0], px[0], 120.0)):
        cv2.circle(mask, tuple(np.round(a).astype(int)), int(r * px_per_mm(cam, p)), 255, -1)
    return mask


def look(img, cam, joints_deg=None, bottle_height=BOTTLE_HEIGHT_MM):
    """One photograph -> Scene."""
    s = Scene()
    g = cv2.GaussianBlur(cv2.cvtColor(img, cv2.COLOR_BGR2GRAY), (5, 5), 0)
    table = C.table_mask(cam, img.shape) > 0
    arm = arm_mask(cam, joints_deg, img.shape) > 0
    free = table & ~arm
    if free.sum() < 5000:
        s.notes.append("the table is not in view")
        return s
    s.table_level = level = float(np.median(g[free]))
    k = px_per_mm(cam, [300.0, 0.0, 0.0])
    dark = (free & (g < DARK * level)).astype(np.uint8)
    dark = cv2.morphologyEx(dark, cv2.MORPH_OPEN, np.ones((5, 5), np.uint8))
    n, lab, stats, cent = cv2.connectedComponentsWithStats(dark)
    comps = [{"i": i, "area": int(stats[i, cv2.CC_STAT_AREA]), "c": cent[i],
              "box": [int(v) for v in stats[i, :4]]} for i in range(1, n)]

    # the basket: the biggest dark thing, at least 12 cm across
    big = [c for c in comps if c["area"] > (120.0 * k) ** 2]
    basket_px = None
    if big:
        b = max(big, key=lambda c: c["area"])
        blob = (lab == b["i"]).astype(np.uint8)
        blob = cv2.morphologyEx(blob, cv2.MORPH_CLOSE, np.ones((25, 25), np.uint8))
        cnt = max(cv2.findContours(blob, cv2.RETR_EXTERNAL, cv2.CHAIN_APPROX_SIMPLE)[0], key=cv2.contourArea)
        hull = cv2.convexHull(cnt)
        basket_px = np.zeros_like(blob)
        cv2.fillPoly(basket_px, [hull], 1)
        (cu, cv_), (w, h), _ = cv2.minAreaRect(hull)
        s.basket = {"px": [round(float(cu), 1), round(float(cv_), 1)], "box_px": b["box"],
                    "size_mm": [round(max(w, h) / k), round(min(w, h) / k)],
                    "xy": [round(v, 1) for v in cam.ground(cu, cv_, 0.0)]}

    # the bottle on the table: a small, compact dark blob the size of its cap (or of the cap seen side-on)
    cap_area = math.pi * (CAP_DIAMETER_MM / 2.0 * px_per_mm(cam, [300.0, 0.0, bottle_height])) ** 2
    caps = []
    for c in comps:
        if not 0.3 * cap_area <= c["area"] <= 3.0 * cap_area:
            continue
        x, y, w, h = c["box"]
        if max(w, h) > 2.2 * min(w, h) or c["area"] < 0.5 * w * h:
            continue
        if basket_px is not None and cv2.dilate(basket_px, np.ones((15, 15), np.uint8))[int(c["c"][1]), int(c["c"][0])]:
            continue
        caps.append(c)
    if caps:
        c = min(caps, key=lambda c: abs(math.log(c["area"] / cap_area)))
        u, v = float(c["c"][0]), float(c["c"][1])
        s.bottle = {"where": "table", "px": [round(u, 1), round(v, 1)], "upright": None,
                    "xy": [round(q, 1) for q in cam.ground(u, v, bottle_height)], "cap_px_area": c["area"]}
        s.bottle["upright"] = _upright(g, level, cam, s.bottle, bottle_height)
        if len(caps) > 1:
            s.notes.append(f"{len(caps)} cap-like blobs; took the best")
    elif basket_px is not None:
        # the bottle in the basket: something pale inside the dark shape
        inside = cv2.erode(basket_px, np.ones((15, 15), np.uint8)) > 0
        pale = (inside & ~arm & (g > PALE * level)).astype(np.uint8)
        pale = cv2.morphologyEx(pale, cv2.MORPH_OPEN, np.ones((5, 5), np.uint8))
        n2, lab2, st2, ce2 = cv2.connectedComponentsWithStats(pale)
        body = [(int(st2[i, cv2.CC_STAT_AREA]), ce2[i]) for i in range(1, n2)
                if st2[i, cv2.CC_STAT_AREA] > 0.15 * (BOTTLE_DIAMETER_MM * k) ** 2]
        if body:
            area, (u, v) = max(body, key=lambda q: q[0])
            s.bottle = {"where": "basket", "px": [round(float(u), 1), round(float(v), 1)], "upright": None,
                        "xy": [round(q, 1) for q in cam.ground(float(u), float(v), 60.0)], "pale_px_area": area}
    return s


def _upright(g, level, cam, bottle, height):
    """Is the bottle on the table standing? A standing bottle's cap is seen from above: a disc, with the body
    below it reaching to where the base stands, a short way towards the point under the camera. A fallen one
    shows its cap side-on, and its body runs a full bottle's length along the table."""
    x, y = bottle["xy"]
    top, base = cam.project([x, y, height]), cam.project([x, y, 0.0])
    lean = np.asarray(base) - np.asarray(top)              # where the body should appear, if standing
    k = px_per_mm(cam, [x, y, height / 2.0])
    # the body is paler than the cap and a little darker or tinted against the table: measure "not table"
    # along the standing direction and across it, just past the cap
    r_cap = CAP_DIAMETER_MM / 2.0 * k
    def strip(direction, length):
        d = direction / (np.linalg.norm(direction) + 1e-6)
        vals = []
        for t in np.linspace(r_cap * 1.3, r_cap * 1.3 + length, 12):
            u, v = np.asarray(top) + d * t
            if 2 <= u < g.shape[1] - 2 and 2 <= v < g.shape[0] - 2:
                vals.append(float(g[int(v) - 2:int(v) + 3, int(u) - 2:int(u) + 3].mean()))
        return np.array(vals) if vals else np.array([level])
    along = strip(lean, max(8.0, float(np.linalg.norm(lean))))
    return bool(np.linalg.norm(lean) < 6.0 or along.mean() < 0.97 * level) if False else None


def draw(img, cam, scene, tool_xyz=None, start_xy=None):
    """The detections on the photograph, for the portal and the model."""
    out = img.copy()
    if scene.basket:
        x, y, w, h = scene.basket["box_px"]
        cv2.rectangle(out, (x, y), (x + w, y + h), (255, 160, 0), 2)
        cv2.putText(out, "basket", (x, max(18, y - 8)), cv2.FONT_HERSHEY_SIMPLEX, 0.8, (255, 160, 0), 2, cv2.LINE_AA)
    if scene.bottle:
        u, v = (int(q) for q in scene.bottle["px"])
        cv2.circle(out, (u, v), 26, (0, 220, 255), 2, cv2.LINE_AA)
        label = "bottle" + (" (in basket)" if scene.bottle["where"] == "basket" else "")
        cv2.putText(out, label, (u + 30, v + 6), cv2.FONT_HERSHEY_SIMPLEX, 0.8, (0, 220, 255), 2, cv2.LINE_AA)
    if start_xy is not None:
        u, v = (int(q) for q in cam.project([start_xy[0], start_xy[1], 0.0]))
        cv2.drawMarker(out, (u, v), (0, 255, 0), cv2.MARKER_TILTED_CROSS, 26, 2, cv2.LINE_AA)
        cv2.putText(out, "start spot", (u + 16, v + 26), cv2.FONT_HERSHEY_SIMPLEX, 0.7, (0, 255, 0), 2, cv2.LINE_AA)
    if tool_xyz is not None:
        u, v = (int(q) for q in cam.project(tool_xyz))
        cv2.drawMarker(out, (u, v), (255, 0, 255), cv2.MARKER_CROSS, 22, 2, cv2.LINE_AA)
    return out
