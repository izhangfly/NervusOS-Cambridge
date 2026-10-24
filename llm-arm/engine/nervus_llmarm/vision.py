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

    def __init__(self, source, interval_s, wanted=lambda: True):
        self.source = source            # callable(role) -> jpeg bytes or None
        self.interval_s = interval_s    # callable() -> seconds
        # Only grab while someone looks: every grab keeps the ACT engine's cameras switched on, and a camera
        # the engine holds can't be opened by another app (Hand Teleop uses the same Logitech).
        self.wanted = wanted
        self.lock = threading.Lock()
        self.latest = {r: (None, 0.0, None) for r in ROLES}   # role -> (jpeg, time, error)
        self._wake = threading.Event()
        self._stop = threading.Event()
        threading.Thread(target=self._run, daemon=True, name="frames").start()

    def _run(self):
        while not self._stop.is_set():
            if not self.wanted():
                self._wake.wait(0.5)
                self._wake.clear()
                continue
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


class DirectCameras:
    """Opens the cameras itself when no ACT engine serves them: each role names a camera by part of its
    /dev/v4l/by-id name (as the ACT engine does, e.g. "Dabai_DC1" or "046d_0825") or of its V4L2 name
    ("Surface Camera"), or gives /dev/videoN. A camera is closed again after 15 s without requests."""

    IDLE_S = 15.0

    def __init__(self, spec: str):
        self.roles = dict(p.split("=", 1) for p in spec.split(",") if "=" in p)
        self.caps: dict[str, tuple] = {}            # role -> (VideoCapture, last use)
        self.lock = threading.Lock()
        threading.Thread(target=self._reaper, daemon=True).start()

    @staticmethod
    def find(match: str) -> str | None:
        import glob
        import os
        if match.startswith("/dev/"):
            return match if os.path.exists(match) else None
        hits = sorted(glob.glob(f"/dev/v4l/by-id/*{match}*-video-index0"))
        if hits:
            return os.path.realpath(hits[0])
        for name in sorted(glob.glob("/sys/class/video4linux/video*/name")):
            try:
                if match in open(name).read():
                    return "/dev/" + name.split("/")[-2]
            except OSError:
                pass
        return None

    def __call__(self, role):
        match = self.roles.get(role)
        if not match:
            return None
        with self.lock:
            cap, _ = self.caps.get(role, (None, 0.0))
            if cap is None:
                dev = self.find(match)
                if dev is None:
                    raise RuntimeError(f"{role} camera ({match}) is not plugged in")
                # OpenCV's V4L2 backend opens by index, not by path.
                cap = cv2.VideoCapture(int(dev.rsplit("video", 1)[1]), cv2.CAP_V4L2)
                cap.set(cv2.CAP_PROP_FRAME_WIDTH, 640)
                cap.set(cv2.CAP_PROP_FRAME_HEIGHT, 480)
            self.caps[role] = (cap, time.time())
        for _ in range(3):              # drop buffered frames: the request wants the newest picture
            cap.grab()
        ok, img = cap.retrieve()
        if not ok:
            with self.lock:
                cap.release()
                self.caps.pop(role, None)
            raise RuntimeError(f"{role} camera gave no frame")
        ok, jpg = cv2.imencode(".jpg", img, [cv2.IMWRITE_JPEG_QUALITY, 85])
        return jpg.tobytes() if ok else None

    def _reaper(self):
        while True:
            time.sleep(2.0)
            with self.lock:
                for role, (cap, t) in list(self.caps.items()):
                    if time.time() - t > self.IDLE_S:
                        cap.release()
                        del self.caps[role]


def auto_source(act_base: str, spec: str):
    """The ACT engine's frames when it is running, else the cameras opened directly."""
    act, direct = act_engine_source(act_base), DirectCameras(spec)

    def grab(role):
        try:
            return act(role)
        except OSError:                 # no ACT engine (connection refused / unreachable)
            return direct(role)
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


def with_grid(jpg: bytes, step: int = 50) -> bytes:
    """The scene frame with a labelled pixel grid, so the model can point at things in pixels."""
    img = decode(jpg)
    h, w = img.shape[:2]
    for x in range(0, w, step):
        cv2.line(img, (x, 0), (x, h), (0, 255, 255) if x % 100 == 0 else (0, 140, 140), 1)
        if x % 100 == 0:
            cv2.putText(img, str(x), (x + 2, 12), cv2.FONT_HERSHEY_SIMPLEX, 0.35, (0, 255, 255), 1, cv2.LINE_AA)
    for y in range(0, h, step):
        cv2.line(img, (0, y), (w, y), (0, 255, 255) if y % 100 == 0 else (0, 140, 140), 1)
        if y % 100 == 0:
            cv2.putText(img, str(y), (2, y - 2), cv2.FONT_HERSHEY_SIMPLEX, 0.35, (0, 255, 255), 1, cv2.LINE_AA)
    ok, out = cv2.imencode(".jpg", img, [cv2.IMWRITE_JPEG_QUALITY, 80])
    return out.tobytes() if ok else jpg
