#!/usr/bin/env python3
"""GPT Arm mock service: a stand-alone fake of the backend described in gpt-arm/API.md.

It serves the portal (gpt-arm/gptarm/web/) at / and every endpoint of API.md with believable, changing data:
a simulated run that advances step by step (decisions, governor refusals, missed grasps and retries, a
verdict), a simulated benchmark that pauses for a hand reset, past runs, and synthetic camera frames. It never
talks to the arm, the cameras or a model, and it binds to loopback only.

    python3 gpt-arm/tools/mock_server.py [--port 8800] [--speed 3] [--scenario idle] [--seed 7]

--speed      how much faster than real time the simulated model and arm are (3 = a run takes about 2 min)
--scenario   the state to start in: idle | running | bench | paused | camerr
             (running: a run already half way; bench: a benchmark with results; paused: a benchmark waiting
             for the scene to be reset by hand; camerr: the wrist camera is unplugged)

Mock-only endpoint, not part of API.md: POST /_mock/scenario {"name": "<scenario>"} switches scenario at run
time (used by the contract test and for screenshots).

Camera frames are drawn with Pillow if it is installed, else with OpenCV + numpy, else two small fixed JPEGs
embedded below are served. Every frame is stamped MOCK so a screenshot can't pass for the real cameras.
"""
from __future__ import annotations

import argparse
import base64
import copy
import io
import json
import math
import mimetypes
import random
import re
import threading
import time
from collections import OrderedDict
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from urllib.parse import parse_qs, unquote, urlparse

WEB_DIR = Path(__file__).resolve().parent.parent / "gptarm" / "web"
VERSION = "0.1.0-mock"

MODELS = [{"id": "gpt-6-astra", "label": "GPT-6 Astra"}, {"id": "gpt-6.1-sol", "label": "GPT-6.1 Sol"},
          {"id": "gpt-5.6-sol", "label": "GPT-5.6 Sol"}]
MODEL_IDS = [m["id"] for m in MODELS]
LABEL = {m["id"]: m["label"] for m in MODELS}
MODES = [{"id": "skills", "label": "Skills"}, {"id": "direct", "label": "Direct joints"}]
MODE_IDS = [m["id"] for m in MODES]
# How good each simulated model is (probability-ish knob), and its typical seconds per call.
QUALITY = {"gpt-6-astra": 0.9, "gpt-6.1-sol": 0.75, "gpt-5.6-sol": 0.3}
LLM_S = {"gpt-6-astra": 7.5, "gpt-6.1-sol": 5.2, "gpt-5.6-sol": 3.6}
DEFAULT_INSTRUCTION = ("Put the hair gel bottle into the basket. Then take it out again and stand it upright on the "
                       "table where it started.")

START = (330.0, 120.0)        # the taped start spot (mm, arm base frame)
BASKET = (300.0, -260.0)      # basket centre
HOME = (180.0, 0.0, 330.0)    # look pose, out of the overhead camera's way
Z_CARRY, Z_GRASP, Z_GRASP_LYING, Z_GRASP_BASKET = 180.0, 95.0, 70.0, 125.0
Z_RELEASE_BASKET, Z_RELEASE_TABLE = 165.0, 105.0
MAX_STEPS = 40                # decisions per run
RETURN_LIMIT_MM = 40.0
GRIP_OPEN, GRIP_HOLD, GRIP_EMPTY = 74.6, 41.5, 4.0

DEFAULT_CONTEXT = """# Arm context

You drive an AgileX PiPER arm (6 joints and a parallel gripper) standing on a table.
Lengths are millimetres in the arm's base frame: +x forward, +y left, +z up.

## Scene
- A hair gel bottle (about 52 mm across, 170 mm tall) starts upright on a taped cross, the start spot.
- A woven basket (160 x 120 mm, 60 mm deep) is to the arm's right.
- The overhead camera sees the whole table; the wrist camera looks down between the fingers.

## Skills (skills mode)
- `move_above <bottle|basket|start_spot>`: move at carry height (180 mm) above a target.
- `lower <grasp|release>`: descend to grasp height or release height over the current target.
- `grasp`, `release`: close or open the gripper.
- `lift`: return to carry height.
- `look`: photograph the table and update the flags.
- `done`: end the run.

## Direct mode
Answer with joint targets in degrees (`joints J1 .. J6`) or a tool move in mm (`tool x y z [gripper mm]`).
The governor still clamps every target to the workspace and refuses moves that would hit the table.

## Rules
- Never release from carry height: lower first.
- One cycle: the bottle goes into the basket, then back out, upright, within 40 mm of the start spot.
- If a grasp misses or the bottle falls over, look, then try again.
"""

# ---------------------------------------------------------------------------------------------------- drawing
# A tiny canvas interface with two back ends (Pillow, OpenCV) so the same scene code draws either way.

try:
    from PIL import Image, ImageDraw, ImageFont
    BACKEND = "pil"
except ImportError:
    try:
        import cv2
        import numpy as np
        BACKEND = "cv2"
    except ImportError:
        BACKEND = None


def _box(b):
    x0, y0, x1, y1 = b
    return [min(x0, x1), min(y0, y1), max(x0, x1), max(y0, y1)]


if BACKEND == "pil":
    _fonts = {}

    def _font(size):
        if size not in _fonts:
            try:
                _fonts[size] = ImageFont.load_default(size=size)
            except TypeError:  # Pillow < 10.1 has one fixed-size bitmap font
                _fonts[size] = ImageFont.load_default()
        return _fonts[size]

    class Canvas:
        def __init__(self, w, h, bg):
            self.w, self.h = w, h
            self.im = Image.new("RGB", (w, h), bg)
            self.d = ImageDraw.Draw(self.im)

        def rect(self, b, fill=None, outline=None, width=1):
            self.d.rectangle(_box(b), fill=fill, outline=outline, width=int(width))

        def ellipse(self, cx, cy, rx, ry, fill=None, outline=None, width=1):
            self.d.ellipse([cx - rx, cy - ry, cx + rx, cy + ry], fill=fill, outline=outline, width=int(width))

        def line(self, pts, color, width=1):
            self.d.line([(float(x), float(y)) for x, y in pts], fill=color, width=max(1, int(width)), joint="curve")

        def poly(self, pts, fill, outline=None):
            self.d.polygon([(float(x), float(y)) for x, y in pts], fill=fill, outline=outline)

        def text(self, x, y, s, color, size=14, bg=None):
            f = _font(size)
            if bg:
                l, t, r, b = self.d.textbbox((x, y), s, font=f)
                self.d.rectangle([l - 4, t - 3, r + 4, b + 3], fill=bg)
            self.d.text((x, y), s, fill=color, font=f)

        def jpeg(self, q=80):
            buf = io.BytesIO()
            self.im.save(buf, "JPEG", quality=q)
            return buf.getvalue()

elif BACKEND == "cv2":
    def _c(c):
        return (int(c[2]), int(c[1]), int(c[0]))  # RGB -> BGR

    class Canvas:
        def __init__(self, w, h, bg):
            self.w, self.h = w, h
            self.im = np.empty((h, w, 3), np.uint8)
            self.im[:] = _c(bg)

        def rect(self, b, fill=None, outline=None, width=1):
            x0, y0, x1, y1 = (int(round(v)) for v in _box(b))
            if fill:
                cv2.rectangle(self.im, (x0, y0), (x1, y1), _c(fill), -1)
            if outline:
                cv2.rectangle(self.im, (x0, y0), (x1, y1), _c(outline), max(1, int(width)), cv2.LINE_AA)

        def ellipse(self, cx, cy, rx, ry, fill=None, outline=None, width=1):
            c, a = (int(cx), int(cy)), (max(1, int(rx)), max(1, int(ry)))
            if fill:
                cv2.ellipse(self.im, c, a, 0, 0, 360, _c(fill), -1, cv2.LINE_AA)
            if outline:
                cv2.ellipse(self.im, c, a, 0, 0, 360, _c(outline), max(1, int(width)), cv2.LINE_AA)

        def line(self, pts, color, width=1):
            cv2.polylines(self.im, [np.array(pts, np.int32)], False, _c(color), max(1, int(width)), cv2.LINE_AA)

        def poly(self, pts, fill, outline=None):
            p = np.array(pts, np.int32)
            cv2.fillPoly(self.im, [p], _c(fill), cv2.LINE_AA)
            if outline:
                cv2.polylines(self.im, [p], True, _c(outline), 1, cv2.LINE_AA)

        def text(self, x, y, s, color, size=14, bg=None):
            scale = size / 28
            (tw, th), base = cv2.getTextSize(s, cv2.FONT_HERSHEY_SIMPLEX, scale, 1)
            if bg:
                cv2.rectangle(self.im, (int(x) - 4, int(y) - 3), (int(x) + tw + 4, int(y) + th + base + 3), _c(bg), -1)
            cv2.putText(self.im, s, (int(x), int(y) + th), cv2.FONT_HERSHEY_SIMPLEX, scale, _c(color), 1, cv2.LINE_AA)

        def jpeg(self, q=80):
            return cv2.imencode(".jpg", self.im, [cv2.IMWRITE_JPEG_QUALITY, q])[1].tobytes()


TABLE_A, TABLE_B, GRAIN = (196, 178, 146), (186, 168, 136), (170, 152, 122)
BOTTLE, BOTTLE_EDGE, CAP = (52, 132, 190), (22, 62, 98), (236, 236, 228)
BASKET_FILL, BASKET_IN, WEAVE = (122, 86, 52), (98, 66, 38), (150, 112, 72)


def _rot_rect(cx, cy, length, width, ang):
    """Corners of a length x width rectangle centred at (cx, cy), long side at angle ang (screen radians)."""
    c, s = math.cos(ang), math.sin(ang)
    pts = []
    for dl, dw in ((-1, -1), (1, -1), (1, 1), (-1, 1)):
        u, v = dl * length / 2, dw * width / 2
        pts.append((cx + u * c - v * s, cy + u * s + v * c))
    return pts


def _draw_world(cv, P, k, snap, bottle_too=True):
    """Draws the table, start cross, basket and (optionally) a bottle that is not held.
    P maps (x_mm, y_mm) to pixels; k is pixels per mm."""
    for i, y in enumerate(range(-200, cv.h + 200, int(max(30, 55 * k)))):
        cv.rect((0, y, cv.w, y + int(max(30, 55 * k))), fill=TABLE_A if i % 2 else TABLE_B)
        cv.line([(0, y + 17 * k), (cv.w, y + 21 * k)], GRAIN, 1)
    sx, sy = P(*START)
    a = 22 * k
    cv.line([(sx - a, sy - a), (sx + a, sy + a)], (232, 228, 210), max(2, 6 * k))
    cv.line([(sx - a, sy + a), (sx + a, sy - a)], (232, 228, 210), max(2, 6 * k))
    x0, y0 = P(BASKET[0] + 60, BASKET[1] + 80)
    x1, y1 = P(BASKET[0] - 60, BASKET[1] - 80)
    cv.rect((x0, y0, x1, y1), fill=BASKET_FILL, outline=(70, 46, 26), width=max(1, 2 * k))
    inset = 9 * k
    cv.rect((x0 + inset, y0 + inset, x1 - inset, y1 - inset), fill=BASKET_IN)
    step = max(6, 11 * k)
    xa, xb = sorted((x0, x1))
    ya, yb = sorted((y0, y1))
    x = xa + inset
    while x < xb - inset:
        cv.line([(x, ya + inset), (x, yb - inset)], WEAVE, 1)
        x += step
    y = ya + inset
    while y < yb - inset:
        cv.line([(xa + inset, y), (xb - inset, y)], WEAVE, 1)
        y += step
    b = snap["bottle"]
    if bottle_too and b["where"] in ("table", "basket"):
        bx, by = P(*b["xy"])
        draw_bottle(cv, bx, by, k, b["upright"], b["angle"])


def draw_bottle(cv, x, y, k, upright, ang):
    if upright:
        cv.ellipse(x, y, 27 * k, 27 * k, fill=BOTTLE, outline=BOTTLE_EDGE, width=max(1, 2 * k))
        cv.ellipse(x - 8 * k, y - 8 * k, 9 * k, 9 * k, fill=(92, 170, 220))
        cv.ellipse(x, y, 14 * k, 14 * k, fill=CAP, outline=(150, 150, 140))
    else:
        cv.poly(_rot_rect(x, y, 170 * k, 52 * k, ang), fill=BOTTLE, outline=BOTTLE_EDGE)
        cx, cy = x + math.cos(ang) * 92 * k, y + math.sin(ang) * 92 * k
        cv.poly(_rot_rect(cx, cy, 22 * k, 30 * k, ang), fill=CAP, outline=(150, 150, 140))


def _clock(t):
    return time.strftime("%H:%M:%S", time.localtime(t))


def render_overhead(snap, annotated):
    W, H, S = 800, 600, 1.15
    OX, OY = W / 2, H - 28

    def P(x, y):
        return (OX - y * S, OY - x * S)

    cv = Canvas(W, H, TABLE_A)
    _draw_world(cv, P, S, snap)
    tx, ty, tz = snap["tool"]
    B, T = P(0, 0), P(tx, ty)
    zk = 1 + (tz - 180) / 700          # nearer the camera looks bigger
    dx, dy = T[0] - B[0], T[1] - B[1]
    n = math.hypot(dx, dy) or 1.0
    ux, uy = dx / n, dy / n
    if snap["bottle"]["where"] == "gripper":
        draw_bottle(cv, T[0], T[1], S * (1 + (tz - 120) / 900), True, 0)
    # Arm links: shoulder to elbow to a wrist that stops short of the tool so a held bottle stays visible.
    E = (B[0] + dx * 0.55, B[1] + dy * 0.55)
    Wr = (T[0] - ux * 26 * zk, T[1] - uy * 26 * zk)
    for pts, w in (([B, E], 40), ([E, Wr], 28)):
        cv.line(pts, (64, 66, 72), (w + 6) * zk)
        cv.line(pts, (206, 209, 215), w * zk)
    cv.ellipse(B[0], B[1], 52, 52, fill=(46, 48, 54), outline=(20, 20, 24), width=2)
    cv.ellipse(B[0], B[1], 26, 26, fill=(90, 94, 102))
    cv.ellipse(E[0], E[1], 18 * zk, 18 * zk, fill=(70, 73, 80))
    # Two fingers either side of the tool point, perpendicular to the arm.
    gap = (snap["gripper_mm"] / 2 + 6) * S * zk
    for side in (-1, 1):
        cx, cy = T[0] - uy * gap * side, T[1] + ux * gap * side
        cv.poly(_rot_rect(cx, cy, 34 * zk, 9 * zk, math.atan2(uy, ux)), fill=(58, 60, 66), outline=(20, 20, 24))
    cv.poly(_rot_rect(Wr[0], Wr[1], 22 * zk, gap * 2 + 14, math.atan2(uy, ux)), fill=(84, 87, 95))

    if annotated:
        x0, y0 = P(480, 320)
        x1, y1 = P(60, -320)
        cv.rect((x0, y0, x1, y1), outline=(222, 200, 90), width=1)
        cv.text(x0 + 6, y0 + 6, "workspace", (222, 200, 90), 13, bg=(40, 36, 20))
        sx, sy = P(*START)
        cv.ellipse(sx, sy, 40 * S, 40 * S, outline=(80, 200, 255), width=2)
        cv.text(sx + 48, sy - 8, "start spot", (80, 200, 255), 14, bg=(14, 34, 46))
        bx0, by0 = P(BASKET[0] + 64, BASKET[1] + 84)
        bx1, by1 = P(BASKET[0] - 64, BASKET[1] - 84)
        cv.rect((bx0, by0, bx1, by1), outline=(255, 170, 60), width=3)
        cv.text(min(bx0, bx1), min(by0, by1) - 24, "basket 0.98", (255, 170, 60), 14, bg=(48, 30, 8))
        b = snap["bottle"]
        bx, by = T if b["where"] == "gripper" else P(*b["xy"])
        r = 32 * S if (b["upright"] or b["where"] == "gripper") else 92 * S
        cv.rect((bx - r, by - r, bx + r, by + r), outline=(70, 220, 120), width=3)
        tag = "held" if b["where"] == "gripper" else ("upright" if b["upright"] else "lying")
        cv.text(bx - r, by + r + 6, f"bottle 0.94 {tag}", (70, 220, 120), 14, bg=(10, 40, 20))
        cv.ellipse(T[0], T[1], 9, 9, outline=(240, 90, 220), width=3)
        cv.text(min(T[0] + 14, W - 190), max(T[1] - 30, 34), f"tool {tx:.0f}, {ty:.0f}, {tz:.0f} mm", (240, 90, 220), 14,
                bg=(44, 14, 40))
    cv.text(10, 10, f"OVERHEAD  MOCK  {_clock(snap['t'])}", (255, 255, 255), 15, bg=(0, 0, 0))
    return cv.jpeg(78)


def render_wrist(snap):
    W, H = 640, 360
    tx, ty, tz = snap["tool"]
    k = 2.6 * 180 / max(tz - 20, 40)   # pixels per mm on the table below
    CX, CY = W / 2, H * 0.52

    def P(x, y):
        return (CX - (y - ty) * k, CY - (x - tx) * k)

    cv = Canvas(W, H, TABLE_A)
    _draw_world(cv, P, k, snap)
    fk = 3.4  # finger spacing in pixels per mm of gripper opening (fixed to the camera)
    if snap["bottle"]["where"] == "gripper":
        draw_bottle(cv, CX, H * 0.66, 3.3, True, 0)
    gap = snap["gripper_mm"] / 2 * fk
    for side in (-1, 1):
        inner = CX + side * gap
        outer = CX + side * (gap + 92)
        cv.poly([(inner, H * 0.42), (outer, H * 0.36), (outer + side * 30, H), (inner, H)], fill=(60, 63, 70),
                outline=(26, 27, 30))
        cv.line([(inner, H * 0.42), (inner, H)], (118, 122, 130), 3)
    cv.text(10, 10, f"WRIST  MOCK  {_clock(snap['t'])}  gripper {snap['gripper_mm']:.1f} mm", (255, 255, 255),
            15, bg=(0, 0, 0))
    return cv.jpeg(76)


# Served when neither Pillow nor OpenCV is installed: 320x240 and 320x180 frames of the start scene.
FALLBACK_OVERHEAD_B64 = (
    "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")
FALLBACK_WRIST_B64 = (
    "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"
    "AAH/2Q==")


def render(camera, snap, annotated=False):
    if BACKEND is None:
        return base64.b64decode(FALLBACK_OVERHEAD_B64 if camera == "overhead" else FALLBACK_WRIST_B64)
    return render_overhead(snap, annotated) if camera == "overhead" else render_wrist(snap)


# ---------------------------------------------------------------------------------------------------- the world

def dist(a, b):
    return math.hypot(a[0] - b[0], a[1] - b[1])


class World:
    """Where the bottle and the arm are. The arm's motion is interpolated so status and camera move smoothly."""

    def __init__(self):
        self.reset()

    def reset(self, t=None):
        self.tool = list(HOME)
        self.motion = None             # (t0, duration_s, from_xyz, to_xyz)
        self.gripper_mm = GRIP_OPEN
        self.holding = False
        self.bottle = {"where": "table", "upright": True, "xy": list(START), "angle": 0.0}
        self.above, self.height = "nothing", "between"
        self.seen_t = t or time.time()
        self.entered_basket = False

    def tool_at(self, now):
        if not self.motion:
            return list(self.tool)
        t0, dur, a, b = self.motion
        f = 1.0 if dur <= 0 else min(1.0, max(0.0, (now - t0) / dur))
        f = f * f * (3 - 2 * f)
        return [a[i] + (b[i] - a[i]) * f for i in range(3)]

    def moving(self, now):
        return bool(self.motion) and now < self.motion[0] + self.motion[1]

    def move(self, to, now, dur):
        self.motion = (now, dur, self.tool_at(now), list(to))
        self.tool = list(to)

    def freeze(self, now):
        self.tool = self.tool_at(now)
        self.motion = None

    def bottle_xy(self, now=None):
        return self.tool_at(now or time.time())[:2] if self.bottle["where"] == "gripper" else self.bottle["xy"]

    def snap(self, now):
        tool = self.tool_at(now)
        b = copy.deepcopy(self.bottle)
        if b["where"] == "gripper":
            b["xy"] = tool[:2]
        return {"t": now, "tool": tool, "gripper_mm": self.gripper_mm, "holding": self.holding, "bottle": b}

    def state(self, now, steps_used):
        tool = self.tool_at(now)
        b = self.bottle
        xy = tool[:2] if b["where"] == "gripper" else b["xy"]
        seen = round(max(0.0, now - self.seen_t), 1)
        return {"tool_xyz_mm": [round(v) for v in tool], "gripper": "closed" if self.gripper_mm < 50 else "open",
                "holding": self.holding,
                "bottle": {"where": b["where"], "upright": b["upright"], "xy_mm": [round(v) for v in xy],
                           "seen_s_ago": seen},
                "basket": {"xy_mm": [round(v) for v in BASKET], "seen_s_ago": seen},
                "start_spot_xy_mm": [round(v) for v in START],
                "above": self.above, "height": self.height,
                "steps_used": steps_used, "steps_left": MAX_STEPS - steps_used}

    def at_start(self):
        b = self.bottle
        return b["where"] == "table" and b["upright"] and dist(b["xy"], START) <= RETURN_LIMIT_MM

    def describe(self):
        b = self.bottle
        x, y = (round(v) for v in b["xy"])
        if b["where"] == "gripper":
            return "held in the gripper"
        if b["where"] == "basket":
            return f"{'upright' if b['upright'] else 'lying on its side'} in the basket at ({x}, {y})"
        d = dist(b["xy"], START)
        where = "on the start spot" if d < 15 else f"{d:.0f} mm from the start spot"
        return f"{'upright' if b['upright'] else 'lying on its side'} on the table at ({x}, {y}), {where}"


def fake_joints(tool):
    """Plausible joint angles for a tool point (display only; not real inverse kinematics)."""
    x, y, z = tool
    r = math.hypot(x, y)
    return [round(math.degrees(math.atan2(y, x)), 1), round(18 + (r - 150) / 5.5, 1),
            round(-38 - (z - 150) / 4.2 - (r - 150) / 9, 1), 0.0, round(48 + (z - 180) / 8, 1), 0.0]


# ---------------------------------------------------------------------------------------------------- episodes

class Episode:
    """One cycle as a generator of actions. The driver applies each action's effect to the world before it
    asks for the next one, so the script reacts to what happened (missed grasps, a bottle that fell over)."""

    def __init__(self, world, rng, model, mode, instruction):
        self.w, self.rng, self.q, self.mode = world, rng, QUALITY[model], mode
        self.phases = phases(instruction)
        self.code = None          # failure code decided inside the episode (else derived in verdict())
        self.attempt = 1          # current attempt within the cycle (grasp/placement retries)
        world.entered_basket = world.bottle["where"] == "basket"  # counted per run

    # --- action constructors
    def per(self, note=None):
        w = self.w
        return {"kind": "perception", "attempt": self.attempt,
                "note": note or f"Bottle {w.describe()}. Basket at ({BASKET[0]:.0f}, {BASKET[1]:.0f}). "
                                f"Gripper {'closed' if w.gripper_mm < 50 else 'open'}"
                                f"{', holding the bottle' if w.holding else ', empty'}.",
                "detail": f"detector: bottle {0.9 + self.rng.random() * 0.08:.2f}, basket 0.98"}

    def dec(self, skill, target, note, to=None, motion_s=0.0, effect=None, ok=True, detail="", grip=None):
        a = {"kind": "decision", "skill": skill, "target": target, "note": note, "to": to, "motion_s": motion_s,
             "effect": effect, "ok": ok, "detail": detail, "attempt": self.attempt}
        if self.mode == "direct" and skill not in ("look", "done"):
            # In direct mode the model writes the motion itself; show it as a tool move or joint targets.
            w = self.w
            dest = to or w.tool
            if skill in ("grasp", "release") or grip is not None:
                a["skill"], a["target"] = "tool", f"gripper {GRIP_HOLD if skill == 'grasp' else GRIP_OPEN:.1f} mm"
            elif self.rng.random() < 0.35:
                j = fake_joints(dest)
                a["skill"] = "joints"
                a["target"] = ", ".join(f"J{i + 1} {v:+.1f}" for i, v in enumerate(j))
            else:
                a["skill"], a["target"] = "tool", f"x {dest[0]:.1f}, y {dest[1]:.1f}, z {dest[2]:.1f}"
        return a

    def info(self, note):
        return {"kind": "info", "note": note, "attempt": self.attempt}

    def travel(self, xy):
        return 2.4 + dist(self.w.tool[:2], xy) / 55

    # --- effects
    def set_pose(self, above, height):
        self.w.above, self.w.height = above, height

    def try_grasp(self):
        w, b = self.w, self.w.bottle
        p = (0.6 + 0.38 * self.q) if b["upright"] else (0.35 + 0.5 * self.q)
        if b["where"] == "basket":
            p -= 0.08
        if self.rng.random() < p:
            w.holding, w.gripper_mm = True, GRIP_HOLD
            w.bottle.update(where="gripper", upright=True)
        else:
            w.holding, w.gripper_mm = False, GRIP_EMPTY
            if self.rng.random() < 0.3 and b["upright"]:  # the fingers clipped it and it fell over
                b["upright"] = False
                b["angle"] = self.rng.uniform(-math.pi, math.pi)
                b["xy"] = [b["xy"][0] + self.rng.uniform(-20, 20), b["xy"][1] + self.rng.uniform(-20, 20)]

    def do_release(self, dest):
        w, rng = self.w, self.rng
        w.gripper_mm, w.holding = GRIP_OPEN, False
        x, y = w.tool[:2]
        if dest == "basket":
            w.bottle = {"where": "basket", "upright": rng.random() < 0.25,
                        "xy": [x + rng.uniform(-15, 15), y + rng.uniform(-15, 15)], "angle": rng.uniform(-0.6, 0.6)}
            w.entered_basket = True
        else:
            spread = 4 + 30 * (1 - self.q)
            w.bottle = {"where": "table", "upright": rng.random() < 0.7 + 0.28 * self.q,
                        "xy": [x + rng.gauss(0, spread), y + rng.gauss(0, spread)],
                        "angle": rng.uniform(-math.pi, math.pi)}

    # --- the script
    def steps(self):
        yield self.per()
        if not self.phases:
            yield self.dec("look", None, "Check the table before answering.", motion_s=2.0)
            yield self.dec("done", None, "This instruction asks for something the skills can't do: they only move "
                                         "the bottle between the table and the basket. Ending without moving.")
            self.code = "outside_skills"
            return
        for dest in self.phases:
            if not (yield from self.carry(dest)):
                return
        yield self.dec("done", None, "The bottle stands on the start spot and the gripper is clear. The cycle is "
                                     "complete." if "start" in self.phases else
                       "The bottle is in the basket and the gripper is clear. Done.")

    def pick(self):
        """Grasp the bottle wherever it is, retrying after a miss. Returns False after three misses."""
        w, rng, q = self.w, self.rng, self.q
        for n in range(3):
            b = w.bottle
            bx, by = b["xy"]
            where = "in the basket" if b["where"] == "basket" else "on the table"
            pose = "upright" if b["upright"] else "lying on its side"
            yield self.dec("move_above", "bottle",
                           (f"Retry: the bottle is {pose} {where} at ({bx:.0f}, {by:.0f}). Line up above it again."
                            if n else
                            f"The gripper is empty and the bottle is {pose} {where} at ({bx:.0f}, {by:.0f}). "
                            "Move above it at carry height."),
                           to=[bx, by, Z_CARRY], motion_s=self.travel((bx, by)),
                           effect=lambda: self.set_pose("bottle", "carry"))
            if rng.random() < (1 - q) * 0.35:
                yield self.dec("grasp", None, "The gripper is above the bottle. Close it.", ok=False,
                               detail="Governor refused: grasp is only allowed at grasp height; the gripper is at "
                                      "carry height (180 mm). Lower first.")
            zg = Z_GRASP_BASKET if b["where"] == "basket" else (Z_GRASP if b["upright"] else Z_GRASP_LYING)
            yield self.dec("lower", "grasp", "Above the bottle. Lower to grasp height so the fingers straddle it."
                           if b["upright"] else "The bottle is lying down; lower to grasp it across the middle.",
                           to=[bx, by, zg], motion_s=3.2, effect=lambda: self.set_pose("bottle", "grasp"))
            yield self.dec("grasp", None, "The fingers are around the bottle. Close the gripper.", motion_s=1.6,
                           effect=self.try_grasp, grip=True)
            if w.holding:
                return True
            fell = "" if w.bottle["upright"] else " The fingers knocked it over; it is now lying on its side."
            yield self.per(f"The gripper closed on nothing ({GRIP_EMPTY:.0f} mm).{fell}")
            self.attempt += 1
            yield self.dec("lift", None, "Nothing in the gripper. Open, lift clear and try again.",
                           to=[w.tool[0], w.tool[1], Z_CARRY], motion_s=2.8,
                           effect=lambda: (setattr(w, "gripper_mm", GRIP_OPEN), self.set_pose("bottle", "carry")))
        self.code = "missed_grasp"
        yield self.info("Three grasps missed in a row; giving up on this cycle.")
        return False

    def carry(self, dest):
        w, rng, q = self.w, self.rng, self.q
        self.attempt = 1  # counted per phase: into the basket, then back to the start spot
        if dest == "basket" and w.bottle["where"] == "basket":
            yield self.info("The bottle is already in the basket; nothing to do for this part.")
            return True
        if dest == "start" and w.at_start():
            yield self.info("The bottle already stands on the start spot.")
            return True
        for placement in range(3 if dest == "start" else 1):
            if not w.holding and not (yield from self.pick()):
                return False
            yield self.dec("lift", None, f"Holding the bottle (gripper {w.gripper_mm:.0f} mm). Lift to carry height"
                           " to clear the basket rim.", to=[w.tool[0], w.tool[1], Z_CARRY], motion_s=2.8,
                           effect=lambda: self.set_pose("nothing", "carry"))
            tgt, txy = ("basket", BASKET) if dest == "basket" else ("start_spot", START)
            if rng.random() < (1 - q) * 0.2:
                yield self.dec("move_above", "table", "Carry the bottle back over the table.", ok=False,
                               detail="Governor refused: unknown target 'table'; the targets are bottle, basket and "
                                      "start_spot.")
            yield self.dec("move_above", tgt, "The bottle is held at carry height. Carry it over the basket."
                           if dest == "basket" else
                           f"Carry the bottle back to the start spot at ({START[0]:.0f}, {START[1]:.0f}).",
                           to=[txy[0], txy[1], Z_CARRY], motion_s=self.travel(txy),
                           effect=lambda: self.set_pose(tgt, "carry"))
            if rng.random() < (1 - q) * 0.6:
                yield self.dec("release", None, "Over the target. Open the gripper.", ok=False,
                               detail="Governor refused: release is only allowed at release height; the gripper is at "
                                      "carry height (180 mm), and a drop from there can tip the bottle over.")
            zr = Z_RELEASE_BASKET if dest == "basket" else Z_RELEASE_TABLE
            yield self.dec("lower", "release", "Lower to release height so the bottle is set down, not dropped.",
                           to=[txy[0], txy[1], zr], motion_s=3.0, effect=lambda: self.set_pose(tgt, "release"))
            yield self.dec("release", None, f"At release height over the {tgt.replace('_', ' ')}. Open the gripper.",
                           motion_s=1.2, effect=lambda: self.do_release(dest), grip=True)
            yield self.dec("lift", None, "Released. Lift clear so the cameras can see the bottle.",
                           to=[txy[0], txy[1], Z_CARRY], motion_s=2.6, effect=lambda: self.set_pose(tgt, "carry"))
            yield self.per()
            if dest == "basket" or w.at_start():
                return True
            # Back on the table but knocked over or too far away: pick it up again and put it back.
            why = "fell over" if not w.bottle["upright"] else \
                f"landed {dist(w.bottle['xy'], START):.0f} mm from the start spot"
            if placement == 2 or rng.random() > 0.55 + 0.4 * q:
                yield self.dec("done", None, f"The bottle {why} and further tries are unlikely to help. Ending the run.")
                return False
            self.attempt += 1
            yield self.info(f"Retry: the bottle {why}. Pick it up and place it again (attempt {self.attempt}).")
        return False

    def verdict(self):
        """(success, reason, checks, failure_code)."""
        w = self.w
        checks = {"entered_basket": None, "returned_mm": None, "upright": None}
        if "basket" in self.phases:
            checks["entered_basket"] = w.entered_basket
        if "start" in self.phases and w.bottle["where"] == "table":
            checks["returned_mm"] = round(dist(w.bottle["xy"], START), 1)
            checks["upright"] = bool(w.bottle["upright"])
        fixed = {"missed_grasp": "The gripper missed the bottle three times in a row.",
                 "step_limit": f"Step limit ({MAX_STEPS}) reached before the cycle was complete.",
                 "camera_lost": "Run aborted: the wrist camera stopped delivering frames.",
                 "outside_skills": "The instruction did not ask for a bottle move the skills can do; nothing moved."}
        if self.code in fixed:
            return False, fixed[self.code], checks, self.code
        if "basket" in self.phases and not w.entered_basket:
            return False, "The bottle never entered the basket.", checks, "never_entered"
        if "start" in self.phases:
            if w.bottle["where"] != "table":
                return False, f"The bottle did not come back to the table (it is in the {w.bottle['where']}).", \
                    checks, "not_returned"
            d = checks["returned_mm"]
            if not w.bottle["upright"]:
                return False, f"The bottle came back {d:.0f} mm from the start spot but is lying on its side.", \
                    checks, "tipped_over"
            if d > RETURN_LIMIT_MM:
                return False, f"The bottle stands {d:.0f} mm from the start spot (limit {RETURN_LIMIT_MM:.0f} mm).", \
                    checks, "not_returned"
            pre = "Entered the basket, came back " if "basket" in self.phases else "Came back "
            return True, f"{pre}{d:.0f} mm from the start spot and stands upright.", checks, None
        if w.bottle["where"] != "basket":
            return False, "The bottle is not in the basket at the end.", checks, "not_in_basket"
        return True, "The bottle is in the basket.", checks, None


def phases(instruction):
    s = instruction.lower()
    into = bool(re.search(r"\b(into|in)\s+(the\s+)?basket", s)) or ("put" in s and "basket" in s)
    back = bool(re.search(r"out of the basket|take it out|where it started|start(ing)? spot|back", s))
    if "bottle" not in s and "basket" not in s:
        return []
    return (["basket"] if into else []) + (["start"] if back else [])


class Stopped(Exception):
    pass


# ---------------------------------------------------------------------------------------------------- the service

def public(run, with_steps=True):
    out = {k: v for k, v in run.items() if not k.startswith("_")}
    if not with_steps:
        out.pop("steps", None)
    return out


class Mock:
    def __init__(self, speed, seed):
        self.lock = threading.RLock()
        self.speed = max(0.1, speed)
        self.rng = random.Random(seed)
        self.world = World()
        self.runs = OrderedDict()       # id -> run (oldest first); keys starting with _ are internal
        self.current = None             # id of the current or most recent run
        self.busy = None
        self.bench = None
        self.stop_evt = threading.Event()
        self.resume_evt = threading.Event()
        self.worker = None
        self.context, self.context_default = DEFAULT_CONTEXT, True
        self.calibration = {"ok": True, "points": 18, "residual_px": 2.1, "when": time.time() - 3 * 3600}
        self.estop_until = 0.0
        self.wrist_fail = False
        self.frame_cache = {}
        self.image_cache = OrderedDict()
        self._history()

    # --- runs
    def _new_run(self, model, mode, instruction, started):
        rid = time.strftime("%Y%m%d-%H%M%S", time.localtime(started)) + "-" + model
        while rid in self.runs:
            rid += "x"
        run = {"id": rid, "model": model, "mode": mode, "instruction": instruction, "state": "running",
               "started": started, "ended": None,
               "outcome": {"success": None, "reason": "", "checks": {"entered_basket": None, "returned_mm": None,
                                                                    "upright": None}},
               "usage": {"llm_calls": 0, "tokens": 0, "llm_s": 0.0, "motion_s": 0.0},
               "pending": None, "steps": [], "_snaps": {}, "_code": None}
        self.runs[rid] = run
        return run

    def _play(self, run, world, live, ff_steps=0, history=False):
        """Plays one episode into `run`. live=False (or the first ff_steps steps) uses a fake clock and returns
        at once, which is how past runs and half-finished scenarios are made."""
        rng = self.rng
        fake = [run["started"]]
        fast = lambda: (not live) or len(run["steps"]) < ff_steps  # noqa: E731
        now = lambda: fake[0] if fast() else time.time()  # noqa: E731

        def wait(s):
            if fast():
                fake[0] += s
                return
            if self.stop_evt.wait(s / self.speed):
                raise Stopped

        def pend(stage, act=None):
            if not fast():
                with self.lock:
                    run["pending"] = {"stage": stage, "since": time.time(),
                                      "skill": act and act.get("skill"), "target": act and act.get("target"),
                                      "note": act and act.get("note")}

        def add(step, images=False):
            with self.lock:
                n = len(run["steps"])
                step = {"n": n, "t": round(now(), 2), **step}
                if images:
                    snap = world.snap(now())
                    step["images"] = {}
                    for cam in ("overhead", "wrist"):
                        f = f"{n:03d}-{cam}.jpg"
                        run["_snaps"][f] = (cam, snap)
                        step["images"][cam] = f
                run["steps"].append(step)
                run["pending"] = None

        ep = Episode(world, rng, run["model"], run["mode"], run["instruction"])
        add({"kind": "user", "note": run["instruction"]})
        decisions = 0
        try:
            for act in ep.steps():
                if live and self.stop_evt.is_set():
                    raise Stopped
                kind = act.pop("kind")
                if kind == "decision":
                    if decisions >= MAX_STEPS:
                        ep.code = "step_limit"
                        add({"kind": "error", "note": f"Step limit ({MAX_STEPS}) reached."})
                        break
                    state = world.state(now(), decisions)
                    if rng.random() < 0.03:
                        pend("llm")
                        wait(4)
                        add({"kind": "error", "note": "The model call failed (HTTP 502 from the router); retrying "
                                                      "in 4 s.", "attempt": act["attempt"]})
                    llm_s = round(LLM_S[run["model"]] * rng.uniform(0.7, 1.4), 1)
                    tokens = int(8200 + 430 * decisions + rng.uniform(0, 1800))
                    t_dec = now()
                    snap = world.snap(t_dec)
                    pend("llm")
                    wait(llm_s)
                    motion_s = round(act["motion_s"] * rng.uniform(0.9, 1.15), 1) if act["ok"] else 0.0
                    if act["ok"] and motion_s:
                        pend("motion", act)
                        if act["to"]:
                            with self.lock:
                                world.move(act["to"], now(), 0 if fast() else motion_s / self.speed)
                        wait(motion_s)
                    if act["ok"] and act["effect"]:
                        with self.lock:
                            act["effect"]()
                    decisions += 1
                    with self.lock:
                        u = run["usage"]
                        u["llm_calls"] += 1
                        u["tokens"] += tokens
                        u["llm_s"] = round(u["llm_s"] + llm_s, 1)
                        u["motion_s"] = round(u["motion_s"] + motion_s, 1)
                        n = len(run["steps"])
                        files = {}
                        for cam in ("overhead", "wrist"):  # what the model was shown when it decided
                            f = f"{n:03d}-{cam}.jpg"
                            run["_snaps"][f] = (cam, snap)
                            files[cam] = f
                        step = {"kind": "decision", "skill": act["skill"], "target": act["target"],
                                "note": act["note"], "ok": act["ok"], "detail": act["detail"], "llm_s": llm_s,
                                "tokens": tokens, "motion_s": motion_s if act["ok"] else None, "state": state,
                                "images": files}
                        if act["attempt"] > 1:
                            step["attempt"] = act["attempt"]
                        run["steps"].append({"n": n, "t": round(now(), 2), **step})
                        run["pending"] = None
                elif kind == "perception":
                    pend("perception")
                    wait(1.4)
                    world.seen_t = now()
                    if act.get("attempt", 1) == 1:
                        act.pop("attempt", None)
                    add({"kind": kind, **act}, images=True)
                else:
                    if act.get("attempt", 1) == 1:
                        act.pop("attempt", None)
                    add({"kind": kind, **act})
                if history and rng.random() < 0.004:
                    ep.code = "camera_lost"
                    add({"kind": "error", "note": "No wrist frame for 5 s: the camera stopped delivering frames."})
                    break
        except Stopped:
            with self.lock:
                world.freeze(time.time())
                run["pending"] = None
                run["steps"].append({"n": len(run["steps"]), "t": round(time.time(), 2), "kind": "info",
                                     "note": "Stopped by the operator. The arm holds its position with the motors on."})
                run.update(state="stopped", ended=time.time())
                run["outcome"]["reason"] = "Stopped by the operator."
                run["_code"] = "stopped"
            return run
        ok, reason, checks, code = ep.verdict()
        with self.lock:
            add({"kind": "verdict", "ok": ok, "note": reason, "detail": json.dumps(checks)})
            run["outcome"] = {"success": ok, "reason": reason, "checks": checks}
            run["state"] = "failed" if code == "camera_lost" else "done"
            run["ended"] = round(now(), 2)
            run["_code"] = code
        return run

    def _history(self):
        """Some past runs, made instantly with a fake clock so the Runs tab has something in it."""
        w = World()
        t = time.time() - 30 * 3600
        plan = [("gpt-6.1-sol", DEFAULT_INSTRUCTION), ("gpt-6-astra", DEFAULT_INSTRUCTION),
                ("gpt-5.6-sol", DEFAULT_INSTRUCTION), ("gpt-6.1-sol", "Put the bottle in the basket"),
                ("gpt-6.1-sol", "Take the bottle out of the basket and stand it where it started"),
                ("gpt-6-astra", DEFAULT_INSTRUCTION), ("gpt-5.6-sol", DEFAULT_INSTRUCTION),
                ("gpt-6.1-sol", DEFAULT_INSTRUCTION), ("gpt-6-astra", "Wave at the camera"),
                ("gpt-5.6-sol", DEFAULT_INSTRUCTION), ("gpt-6.1-sol", DEFAULT_INSTRUCTION)]
        for i, (model, instr) in enumerate(plan):
            mode = "direct" if i in (6, 9) else "skills"
            if phases(instr)[:1] != ["start"] and not w.at_start():
                w.reset(t)  # the operator put the bottle back
            run = self._new_run(model, mode, instr, t)
            self._play(run, w, live=False, history=True)
            t = run["ended"] + self.rng.uniform(600, 5400)
        self.current = next(reversed(self.runs))

    # --- live work in a background thread
    def _start_worker(self, busy, fn):
        self.stop_evt.clear()
        self.resume_evt.clear()
        self.busy = busy

        def body():
            try:
                fn()
            finally:
                with self.lock:
                    self.busy = None
                    self.world.freeze(time.time())
        self.worker = threading.Thread(target=body, daemon=True)
        self.worker.start()

    def start_run(self, model, mode, instruction, ff_steps=0):
        started = time.time() - (ff_steps * 9 if ff_steps else 0)
        if ff_steps and not self.world.at_start():
            self.world.reset()
        run = self._new_run(model, mode, instruction, started)
        self.current = run["id"]
        self._start_worker("run", lambda: self._play(run, self.world, live=True, ff_steps=ff_steps))
        return run["id"]

    def start_bench(self, models, n, instruction, mode, ff_runs=0, force_pause=False):
        b = self.bench = {"state": "running", "instruction": instruction, "runs_per_model": n, "mode": mode,
                          "models": list(models), "started": time.time() - ff_runs * 200, "current": None,
                          "message": "", "results": [self._result(m, []) for m in models], "run_ids": []}

        def go():
            done = 0
            fake_t = b["started"]
            for i in range(1, n + 1):
                for m in models:
                    fast = done < ff_runs
                    with self.lock:
                        if fast and not self.world.at_start():
                            self.world.reset(fake_t)
                        run = self._new_run(m, mode, instruction, fake_t if fast else time.time())
                        b["current"] = {"model": m, "index": i, "run_id": run["id"]}
                        b["run_ids"].append(run["id"])
                        self.current = run["id"]
                    try:
                        self._play(run, self.world, live=not fast)
                    finally:
                        with self.lock:
                            self._tally(b)
                    fake_t = (run["ended"] or time.time()) + 20
                    done += 1
                    if run["state"] == "stopped" or self.stop_evt.is_set():
                        with self.lock:
                            b.update(state="stopped", current=None)
                        return
                    reset_needed = not self.world.at_start()
                    if (force_pause and done == ff_runs) or (reset_needed and not fast):
                        with self.lock:
                            b["state"] = "paused"
                            if reset_needed:
                                b["message"] = (f"Run {i} for {LABEL[m]} left the bottle {self.world.describe()}. "
                                                "Stand it upright on the taped start cross, then press Resume.")
                            else:
                                b["message"] = ("Scheduled scene check: make sure the bottle stands upright on the "
                                                "taped start cross and the basket sits on its corner marks, then "
                                                "press Resume.")
                        while not self.resume_evt.wait(0.2):
                            if self.stop_evt.is_set():
                                with self.lock:
                                    b.update(state="stopped", current=None)
                                return
                        self.resume_evt.clear()
                        with self.lock:
                            self.world.reset()  # the operator reset the scene by hand
                            b.update(state="running", message="")
                    elif fast and reset_needed:
                        self.world.reset(fake_t)
            with self.lock:
                b.update(state="done", current=None)
        self._start_worker("bench", go)

    def _result(self, model, runs):
        fin = [r for r in runs if r["state"] in ("done", "failed")]
        succ = [r for r in fin if r["outcome"]["success"]]
        fails = {}
        for r in fin:
            if not r["outcome"]["success"]:
                fails[r["_code"] or "other"] = fails.get(r["_code"] or "other", 0) + 1
        dec = [sum(1 for s in r["steps"] if s["kind"] == "decision") for r in fin]
        return {"model": model, "runs": len(fin), "successes": len(succ),
                "rate": round(len(succ) / len(fin), 3) if fin else None,
                "mean_steps": round(sum(dec) / len(dec), 1) if dec else None,
                "mean_s": round(sum(r["ended"] - r["started"] for r in fin) / len(fin), 1) if fin else None,
                "tokens": sum(r["usage"]["tokens"] for r in runs), "failures": fails}

    def _tally(self, b):
        runs = [self.runs[i] for i in b["run_ids"] if i in self.runs]
        b["results"] = [self._result(m, [r for r in runs if r["model"] == m]) for m in b["models"]]

    def stop_all(self, join=True):
        self.stop_evt.set()
        self.resume_evt.set()
        w = self.worker
        if join and w and w.is_alive() and w is not threading.current_thread():
            w.join(timeout=5)

    def scenario(self, name):
        self.stop_all()
        with self.lock:
            self.stop_evt.clear()
            self.resume_evt.clear()
            self.wrist_fail = name == "camerr"
            self.estop_until = 0.0
            self.busy = None
        if name == "running":
            self.start_run("gpt-6.1-sol", "skills", DEFAULT_INSTRUCTION, ff_steps=9)
        elif name == "bench":
            self.start_bench(MODEL_IDS, 5, DEFAULT_INSTRUCTION, "skills", ff_runs=7)
        elif name == "paused":
            self.start_bench(MODEL_IDS, 5, DEFAULT_INSTRUCTION, "skills", ff_runs=8, force_pause=True)
        elif name not in ("idle", "camerr"):
            raise ValueError(f"unknown scenario {name!r}; use idle, running, bench, paused or camerr")

    # --- views
    def status(self):
        now = time.time()
        with self.lock:
            w = self.world
            tool = w.tool_at(now)
            estop = now < self.estop_until
            run = self.runs.get(self.current)
            used = sum(1 for s in run["steps"] if s["kind"] == "decision") if run and run["state"] == "running" else 0
            cams = {"overhead": {"ok": True, "age_s": round(self.rng.uniform(0.1, 0.6), 1), "error": None,
                                 "size": [800, 600]},
                    "wrist": {"ok": True, "age_s": round(self.rng.uniform(0.05, 0.3), 2), "error": None,
                              "size": [640, 360]}}
            if self.wrist_fail:
                cams["wrist"] = {"ok": False, "age_s": None, "size": [640, 360],
                                 "error": "wrist camera: /dev/v4l/by-id/usb-Orbbec_Dabai_DC1-video-index0 not found "
                                          "(is the USB cable in?)"}
            return {
                "version": VERSION, "time": round(now, 2),
                "arm": {"reachable": True, "enabled": not estop, "status": "EMERGENCY_STOP" if estop else "NORMAL",
                        "tool_xyz_mm": [round(v, 1) for v in tool], "lean_deg": round(10 + (tool[2] - 180) / 40, 1),
                        "joints_deg": fake_joints(tool), "gripper_mm": w.gripper_mm, "holding": w.holding,
                        "moving": w.moving(now), "guard": "workspace clamp active" if tool[2] < 90 else None,
                        "error": "Emergency stop (mock: clears itself 10 s after it was pressed)." if estop else None},
                "cameras": cams,
                "calibration": dict(self.calibration),
                "scene": w.state(now, used),
                "models": MODELS, "model": "gpt-6.1-sol",
                "modes": MODES, "mode": "skills",
                "default_instruction": DEFAULT_INSTRUCTION,
                "busy": self.busy,
                "run": copy.deepcopy(public(run)) if run else None,
                "bench": copy.deepcopy(self.bench),
                "specs": {"arm": "AgileX PiPER, firmware S-V1.9-0", "driver": "smooth (own IK, 100 Hz)",
                          "speed_pct": 20, "workspace_mm": {"x": [60, 480], "y": [-320, 320], "z": [80, 600]},
                          "overhead": "Surface Pro 9 rear camera, 1600x1200", "wrist": "Orbbec Dabai DC1, 1280x720",
                          "llm_route": "Codex CLI -> AgentRouter", "max_steps": MAX_STEPS,
                          "backend": "mock_server.py (simulated, no arm, no model calls)"},
            }

    def bench_view(self):
        with self.lock:
            if self.bench:
                return copy.deepcopy(self.bench)
        return {"state": "idle", "instruction": "", "runs_per_model": 0, "started": None, "current": None,
                "message": "", "results": [], "run_ids": []}

    def camera(self, cam, annotated):
        if cam == "wrist" and self.wrist_fail:
            return None
        key = (cam, annotated)
        hit = self.frame_cache.get(key)
        now = time.time()
        if hit and now - hit[0] < 0.4:
            return hit[1]
        with self.lock:
            snap = self.world.snap(now)
        jpg = render(cam, snap, annotated)
        self.frame_cache[key] = (now, jpg)
        return jpg

    def run_image(self, rid, name):
        key = (rid, name)
        if key in self.image_cache:
            self.image_cache.move_to_end(key)
            return self.image_cache[key]
        with self.lock:
            run = self.runs.get(rid)
            entry = run and run["_snaps"].get(name)
        if not entry:
            return None
        jpg = render(entry[0], entry[1])
        self.image_cache[key] = jpg
        while len(self.image_cache) > 80:
            self.image_cache.popitem(last=False)
        return jpg

    # --- arm actions
    def _arm_job(self, busy, points, each_s, after=None):
        def go():
            for p in points:
                with self.lock:
                    self.world.move(p, time.time(), each_s)
                if self.stop_evt.wait(each_s):
                    return
            if after:
                with self.lock:
                    after()
        self._start_worker(busy, go)


# ---------------------------------------------------------------------------------------------------- HTTP

class Handler(BaseHTTPRequestHandler):
    mock: Mock = None
    verbose = False
    server_version = "gptarm-mock/" + VERSION

    def log_message(self, fmt, *args):
        if self.verbose:
            super().log_message(fmt, *args)

    def send(self, code, body, ctype="application/json", cache="no-store"):
        data = json.dumps(body).encode() if ctype == "application/json" else body
        self.send_response(code)
        self.send_header("Content-Type", ctype)
        self.send_header("Content-Length", str(len(data)))
        self.send_header("Cache-Control", cache)
        self.end_headers()
        if self.command != "HEAD":
            self.wfile.write(data)

    def err(self, code, msg):
        self.send(code, {"error": msg})

    def body(self):
        n = int(self.headers.get("Content-Length") or 0)
        if not n:
            return {}
        try:
            data = json.loads(self.rfile.read(n))
        except ValueError:
            raise ValueError("The request body must be JSON.") from None
        if not isinstance(data, dict):
            raise ValueError("The request body must be a JSON object.")
        return data

    def do_HEAD(self):
        self.do_GET()

    def do_GET(self):
        u = urlparse(self.path)
        path, q = unquote(u.path), parse_qs(u.query)
        m = self.mock
        try:
            if path == "/api/status":
                return self.send(200, m.status())
            mm = re.fullmatch(r"/api/camera/(overhead|wrist)\.jpg", path)
            if mm:
                jpg = m.camera(mm.group(1), q.get("annotated", ["0"])[0] in ("1", "true"))
                if jpg is None:
                    return self.err(404, "No frame from the wrist camera: the device is not found.")
                return self.send(200, jpg, "image/jpeg")
            if path == "/api/runs":
                try:
                    limit = max(1, min(500, int(q.get("limit", ["50"])[0])))
                except ValueError:
                    return self.err(400, "limit must be a whole number.")
                with m.lock:
                    runs = copy.deepcopy([public(r, False) for r in list(reversed(m.runs.values()))[:limit]])
                return self.send(200, {"runs": runs})
            mm = re.fullmatch(r"/api/runs/([^/]+)", path)
            if mm:
                with m.lock:
                    r = m.runs.get(mm.group(1))
                    out = copy.deepcopy(public(r)) if r else None
                return self.send(200, out) if out else self.err(404, f"No run with id {mm.group(1)}.")
            mm = re.fullmatch(r"/api/runs/([^/]+)/([^/]+)", path)
            if mm:
                jpg = m.run_image(mm.group(1), mm.group(2))
                return self.send(200, jpg, "image/jpeg", "max-age=3600") if jpg else \
                    self.err(404, f"No file {mm.group(2)} in run {mm.group(1)}.")
            if path == "/api/bench":
                return self.send(200, m.bench_view())
            if path == "/api/context":
                return self.send(200, {"text": m.context, "default": m.context_default})
            if path.startswith("/api/"):
                return self.err(404, f"No endpoint {path}.")
            return self.static(path)
        except Exception as e:  # noqa: BLE001 - a mock should answer, not crash
            return self.err(500, f"Mock server error: {e}")

    def static(self, path):
        rel = "index.html" if path in ("/", "") else path.lstrip("/")
        f = (WEB_DIR / rel).resolve()
        if WEB_DIR.resolve() not in f.parents or not f.is_file():
            return self.err(404, f"No file {path}.")
        ctype = mimetypes.guess_type(f.name)[0] or "application/octet-stream"
        if ctype.startswith("text/") or ctype in ("application/javascript", "image/svg+xml"):
            ctype += "; charset=utf-8"
        return self.send(200, f.read_bytes(), ctype, "no-cache")

    def do_POST(self):
        path = urlparse(self.path).path
        m = self.mock
        try:
            body = self.body()
        except ValueError as e:
            return self.err(400, str(e))
        try:
            with m.lock:
                return self.post(path, body, m)
        except ValueError as e:
            return self.err(400, str(e))
        except Exception as e:  # noqa: BLE001
            return self.err(500, f"Mock server error: {e}")

    def post(self, path, body, m):
        if path == "/api/stop":
            m.stop_all(join=False)
            return self.send(200, {"ok": True})
        if path == "/api/estop":
            m.stop_all(join=False)
            m.estop_until = time.time() + 10
            return self.send(200, {"ok": True})
        if path == "/_mock/scenario":
            m.lock.release()  # scenario() joins the worker, which needs the lock to finish
            try:
                m.scenario(str(body.get("name", "")))
            finally:
                m.lock.acquire()
            return self.send(200, {"ok": True})
        if path == "/api/context":
            if body.get("reset") is True:
                m.context, m.context_default = DEFAULT_CONTEXT, True
            elif isinstance(body.get("text"), str) and body["text"].strip():
                m.context, m.context_default = body["text"], body["text"] == DEFAULT_CONTEXT
            else:
                raise ValueError('Send {"text": "..."} (not empty) or {"reset": true}.')
            return self.send(200, {"ok": True, "text": m.context, "default": m.context_default})
        if path == "/api/bench/resume":
            if not m.bench or m.bench["state"] != "paused":
                return self.err(409, "The benchmark isn't paused.")
            m.resume_evt.set()
            return self.send(200, {"ok": True})
        # Everything below moves the arm: not while something else runs or the e-stop is latched.
        known = {"/api/run", "/api/bench/start", "/api/home", "/api/look", "/api/calibrate"}
        if path not in known:
            return self.err(404, f"No endpoint {path}.")
        if m.busy:
            what = {"run": "A run is in progress", "bench": "A benchmark is in progress",
                    "calibrate": "Calibration is in progress", "home": "The arm is moving home"}[m.busy]
            return self.err(409, f"{what}; stop it first.")
        if time.time() < m.estop_until:
            return self.err(409, "The arm is in emergency stop.")
        if path == "/api/run":
            instr = body.get("instruction")
            if not isinstance(instr, str) or not instr.strip():
                raise ValueError("instruction is required.")
            if len(instr) > 2000:
                raise ValueError("instruction is too long (2000 characters at most).")
            model = body.get("model", "gpt-6.1-sol")
            if model not in MODEL_IDS:
                raise ValueError(f"Unknown model {model!r}.")
            mode = body.get("mode", "skills")
            if mode not in MODE_IDS:
                raise ValueError(f"Unknown mode {mode!r}.")
            return self.send(200, {"ok": True, "id": m.start_run(model, mode, instr.strip())})
        if path == "/api/bench/start":
            models, n, instr = body.get("models"), body.get("runs_per_model"), body.get("instruction")
            if not isinstance(models, list) or not models or any(x not in MODEL_IDS for x in models):
                raise ValueError("models must be a non-empty list of known model ids.")
            if not isinstance(n, int) or isinstance(n, bool) or not 1 <= n <= 100:
                raise ValueError("runs_per_model must be a whole number from 1 to 100.")
            if not isinstance(instr, str) or not instr.strip():
                raise ValueError("instruction is required.")
            mode = body.get("mode", "skills")
            if mode not in MODE_IDS:
                raise ValueError(f"Unknown mode {mode!r}.")
            m.start_bench(models, n, instr.strip(), mode)
            return self.send(200, {"ok": True})
        if path == "/api/home":
            m._arm_job("home", [HOME], 3.0, after=lambda: setattr(m.world, "above", "nothing"))
            return self.send(200, {"ok": True})
        if path == "/api/calibrate":
            pts = [(140, 200, 140), (420, 200, 140), (420, -200, 140), (140, -200, 140), HOME]

            def done():
                m.calibration = {"ok": True, "points": 18, "residual_px": round(m.rng.uniform(1.5, 2.6), 1),
                                 "when": time.time()}
            m._arm_job("calibrate", pts, 2.0, after=done)
            return self.send(200, {"ok": True})
        if path == "/api/look":
            m.world.seen_t = time.time()
            return self.send(200, {"ok": True})


def main():
    ap = argparse.ArgumentParser(description="GPT Arm mock service (fake backend for the portal)")
    ap.add_argument("--port", type=int, default=8800)
    ap.add_argument("--speed", type=float, default=3.0, help="simulation speed-up (default 3)")
    ap.add_argument("--scenario", default="idle", help="idle | running | bench | paused | camerr")
    ap.add_argument("--seed", type=int, default=7)
    ap.add_argument("--verbose", action="store_true", help="log every request")
    a = ap.parse_args()
    mock = Mock(a.speed, a.seed)
    mock.scenario(a.scenario)
    Handler.mock, Handler.verbose = mock, a.verbose
    srv = ThreadingHTTPServer(("127.0.0.1", a.port), Handler)
    srv.daemon_threads = True
    print(f"GPT Arm mock on http://127.0.0.1:{a.port}/  (scenario {a.scenario}, speed x{a.speed:g}, "
          f"camera frames: {BACKEND or 'embedded fallback'})", flush=True)
    try:
        srv.serve_forever()
    except KeyboardInterrupt:
        pass


if __name__ == "__main__":
    main()
