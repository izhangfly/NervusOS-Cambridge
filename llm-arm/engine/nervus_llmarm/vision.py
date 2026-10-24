"""Camera frames, coloured-object detection and the scene camera's image-to-table calibration.

Frames come from the ACT engine (GET /api/camera/<role>.jpg on 127.0.0.1:8795), which already owns the cameras:
the wrist camera (Orbbec Dabai DC1, RGB only) and the scene camera (Logitech C270). Asking for a frame also keeps
the engine's cameras switched on. In the simulator the frames are drawn from the simulated arm and blocks.

Calibration maps scene-camera pixels to the table plane in the arm's base frame: a homography from at least four
pixel <-> (x, y) mm pairs, plus the table's height. It lives in ~/.config/nervus/llm-arm-calib.json.
"""
from __future__ import annotations

import json
import threading
import time
import urllib.request
from pathlib import Path

import cv2
import numpy as np

from .config import STALE_FRAME_S, write_json

ROLES = ("scene", "wrist")
# HSV ranges (OpenCV: H 0..180). Red wraps around 0.
COLOURS = {
    "red": [((0, 120, 80), (8, 255, 255)), ((170, 120, 80), (180, 255, 255))],
    "yellow": [((20, 120, 100), (34, 255, 255))],
    "green": [((40, 80, 60), (85, 255, 255))],
    "blue": [((95, 120, 60), (130, 255, 255))],
}
MIN_AREA_PX = 150


class Frames:
    """Grabs one JPEG per role every `interval` seconds and keeps the latest of each."""

    def __init__(self, source, interval_s):
        self.source = source            # callable(role) -> jpeg bytes or None
        self.interval_s = interval_s    # callable() -> seconds
        self.lock = threading.Lock()
        self.latest = {r: (None, 0.0, None) for r in ROLES}   # role -> (jpeg, time, error)
        self._wake = threading.Event()
        self._stop = threading.Event()
        threading.Thread(target=self._run, daemon=True, name="frames").start()

    def _run(self):
        while not self._stop.is_set():
            for r in ROLES:
                try:
                    jpg, err = self.source(r), None
                except Exception as e:          # a camera that is unplugged must not stop the other one
                    jpg, err = None, str(e)
                with self.lock:
                    if jpg:
                        self.latest[r] = (jpg, time.time(), None)
                    else:
                        old = self.latest[r]
                        self.latest[r] = (old[0], old[1], err or "no frame")
            self._wake.wait(self.interval_s())
            self._wake.clear()

    def now(self):
        """Grab again right away (before a model request)."""
        self._wake.set()

    def get(self, role):
        with self.lock:
            jpg, t, err = self.latest[role]
        fresh = jpg is not None and time.time() - t < STALE_FRAME_S
        return (jpg if fresh else None), t, err

    def info(self) -> dict:
        out = {}
        for r in ROLES:
            jpg, t, err = self.get(r)
            out[r] = {"fresh": jpg is not None, "age_s": round(time.time() - t, 1) if t else None,
                      "bytes": len(jpg) if jpg else 0, "error": None if jpg else err}
        return out

    def close(self):
        self._stop.set()
        self._wake.set()


def act_engine_source(base="http://127.0.0.1:8795"):
    def grab(role):
        with urllib.request.urlopen(f"{base}/api/camera/{role}.jpg", timeout=2.0) as r:
            return r.read()
    return grab


# ---------------------------------------------------------------------------------------------------------------
def decode(jpg: bytes):
    return cv2.imdecode(np.frombuffer(jpg, np.uint8), cv2.IMREAD_COLOR)


def detect(img) -> list:
    """Coloured blobs: [{colour, u, v, area}], biggest first."""
    hsv = cv2.cvtColor(cv2.GaussianBlur(img, (5, 5), 0), cv2.COLOR_BGR2HSV)
    found = []
    for name, ranges in COLOURS.items():
        mask = sum(cv2.inRange(hsv, np.array(lo), np.array(hi)) for lo, hi in ranges)
        mask = cv2.morphologyEx(mask.astype(np.uint8), cv2.MORPH_OPEN, np.ones((5, 5), np.uint8))
        contours, _ = cv2.findContours(mask, cv2.RETR_EXTERNAL, cv2.CHAIN_APPROX_SIMPLE)
        for c in contours:
            a = cv2.contourArea(c)
            if a < MIN_AREA_PX:
                continue
            m = cv2.moments(c)
            found.append({"colour": name, "u": m["m10"] / m["m00"], "v": m["m01"] / m["m00"], "area": a})
    return sorted(found, key=lambda o: -o["area"])


class Calibration:
    def __init__(self, path: Path | None, preset: dict | None = None):
        self.path = path
        self.H = None
        self.table_z_mm = None
        data = preset
        if data is None and path and path.exists():
            try:
                data = json.loads(path.read_text())
            except (OSError, ValueError):
                data = None
        if data:
            self._apply(data)

    def _apply(self, d):
        self.H = np.array(d["H"], float) if d.get("H") else None
        self.table_z_mm = d.get("table_z_mm")
        self.points = d.get("points", [])

    @property
    def ready(self):
        return self.H is not None and self.table_z_mm is not None

    def set_points(self, points: list, table_z_mm: float):
        """points: [[u, v, x_mm, y_mm], ...], at least 4, not all on one line."""
        if len(points) < 4:
            raise ValueError("need at least 4 points")
        src = np.array([[p[0], p[1]] for p in points], np.float32)
        dst = np.array([[p[2], p[3]] for p in points], np.float32)
        H, _ = cv2.findHomography(src, dst, 0)
        if H is None or not np.all(np.isfinite(H)):
            raise ValueError("those points don't define a plane (are three of them on one line?)")
        d = {"H": H.tolist(), "table_z_mm": float(table_z_mm), "points": points, "time": time.time()}
        self._apply(d)
        if self.path:
            write_json(self.path, d)
        return self.residual_mm(points)

    def to_table(self, u, v):
        p = self.H @ np.array([u, v, 1.0])
        return float(p[0] / p[2]), float(p[1] / p[2])

    def residual_mm(self, points):
        errs = [np.hypot(*(np.array(self.to_table(p[0], p[1])) - np.array(p[2:4]))) for p in points]
        return round(float(max(errs)), 1)

    def info(self):
        return {"ready": self.ready, "table_z_mm": self.table_z_mm, "points": len(getattr(self, "points", []) or [])}


# ---------------------------------------------------------------------------------------------------------------
class SimCameras:
    """Draws the simulated table from above (scene) and from the gripper (wrist)."""

    W, H = 640, 480
    BGR = {"red": (40, 40, 220), "blue": (200, 90, 30), "green": (60, 170, 60), "yellow": (40, 210, 230)}

    def __init__(self, arm):
        self.arm = arm

    # Scene camera: x 100..500 mm runs bottom to top, y +250..-250 mm runs left to right.
    @staticmethod
    def px(x, y):
        return 320.0 - y * 1.0, 460.0 - (x - 100.0) * 1.0

    def calibration(self) -> dict:
        pts = [(x, y) for x in (150, 450) for y in (-200, 200)]
        src = np.array([self.px(x, y) for x, y in pts], np.float32)
        dst = np.array(pts, np.float32)
        H, _ = cv2.findHomography(src, dst, 0)
        return {"H": H.tolist(), "table_z_mm": 0.0, "points": []}

    def __call__(self, role):
        img = np.full((self.H, self.W, 3), (70, 80, 90), np.uint8)
        with self.arm.lock:
            blocks = {k: list(v) for k, v in self.arm.blocks.items()}
            pose = list(self.arm.pose)
        if role == "scene":
            for k, (x, y) in blocks.items():
                u, v = self.px(x, y)
                cv2.rectangle(img, (int(u - 15), int(v - 15)), (int(u + 15), int(v + 15)), self.BGR[k], -1)
            u, v = self.px(pose[0], pose[1])
            cv2.circle(img, (int(u), int(v)), 8 + int(pose[2] / 40), (230, 230, 230), 2)
        else:
            for k, (x, y) in blocks.items():
                s = 300.0 / max(40.0, pose[2])
                u, v = 320 + (pose[1] - y) * s, 240 + (pose[0] - x) * s
                h = 15 * s
                cv2.rectangle(img, (int(u - h), int(v - h)), (int(u + h), int(v + h)), self.BGR[k], -1)
        ok, jpg = cv2.imencode(".jpg", img, [cv2.IMWRITE_JPEG_QUALITY, 80])
        return jpg.tobytes() if ok else None
