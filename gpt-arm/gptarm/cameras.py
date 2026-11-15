"""The two cameras.

overhead: the Surface's rear camera above the table, served by overhead/overhead_cam.py and reached through an
          SSH tunnel on 127.0.0.1:8798. It sees the whole table; perception and calibration use it.
wrist:    the Orbbec Dabai DC1's colour stream on the gripper, opened here by its /dev/v4l/by-id name. It is
          the close-up view the model also gets, and a check on what the gripper holds.
"""
from __future__ import annotations

import glob
import json
import os
import threading
import time
import urllib.request

import cv2
import numpy as np


class CameraError(Exception):
    pass


class Overhead:
    def __init__(self, url="http://127.0.0.1:8798"):
        self.url = url.rstrip("/")

    def jpeg(self) -> bytes:
        try:
            with urllib.request.urlopen(self.url + "/frame.jpg", timeout=3.0) as r:
                return r.read()
        except OSError as e:
            raise CameraError(f"overhead camera: {getattr(e, 'reason', e)} (is the tunnel to the Surface up?)") from None

    def frame(self) -> np.ndarray:
        img = cv2.imdecode(np.frombuffer(self.jpeg(), np.uint8), cv2.IMREAD_COLOR)
        if img is None:
            raise CameraError("overhead camera sent a broken frame")
        return img

    def info(self) -> dict:
        try:
            with urllib.request.urlopen(self.url + "/info", timeout=3.0) as r:
                return json.loads(r.read())
        except (OSError, ValueError) as e:
            raise CameraError(f"overhead camera: {getattr(e, 'reason', e)}") from None

    def set_exposure(self, exposure_us, gain) -> dict:
        req = urllib.request.Request(self.url + "/controls", json.dumps({"exposure_us": exposure_us, "gain": gain}).encode(),
                                     {"Content-Type": "application/json"})
        try:
            with urllib.request.urlopen(req, timeout=5.0) as r:
                out = json.loads(r.read())
        except (OSError, ValueError) as e:
            raise CameraError(f"overhead camera: {getattr(e, 'reason', e)}") from None
        t_end = time.time() + 15.0                      # the camera restarts: wait for frames made after it
        time.sleep(2.5)
        while time.time() < t_end:
            try:
                if (self.info().get("age_s") or 9) < 1.0 and self.info().get("frames", 0) >= 4:
                    return out
            except CameraError:
                pass
            time.sleep(0.5)
        raise CameraError("the overhead camera did not come back after an exposure change")

    def tune(self, mask=None, target=170.0, log=None) -> dict:
        """Set a fixed exposure that puts the table at `target` grey. mask: the table's pixels (once a
        calibration says where it is); without one the whole picture's median is used, aiming lower.
        The exposure time goes up before the gain (less noise). Returns {"exposure_us", "gain", "level"}."""
        def level():
            g = cv2.cvtColor(self.settled(2, 0.3), cv2.COLOR_BGR2GRAY)
            return float(np.median(g[mask > 0]) if mask is not None else np.median(g))

        goal = float(target) if mask is not None else 0.6 * float(target)
        i = self.info()
        (e_lo, e_hi), (g_lo, g_hi) = i["limits"]["exposure_us"], i["limits"]["gain"]
        exp, gain, lv = float(i["exposure_us"]), float(i["gain"]), level()
        for _ in range(6):
            if abs(lv - goal) <= 0.08 * goal:
                break
            k = min(2.5, max(0.4, goal / max(lv, 4.0))) * exp * gain      # light gathered, to scale
            exp2 = min(e_hi, max(e_lo, k / g_lo)) if k / g_lo <= e_hi else e_hi
            gain2 = min(g_hi, max(g_lo, k / exp2))
            if abs(exp2 - exp) < 1 and abs(gain2 - gain) < 0.02:
                break                                   # at a limit
            exp, gain = exp2, gain2
            self.set_exposure(exp, gain)
            lv = level()
            if log:
                log(f"overhead camera: exposure {exp / 1000:.0f} ms, gain {gain:.1f}, table at {lv:.0f}")
        return {"exposure_us": int(exp), "gain": round(gain, 2), "level": round(lv, 1)}

    def settled(self, n=3, gap_s=0.25) -> np.ndarray:
        """The median of n frames: steadier for differencing and colour thresholds."""
        frames = []
        for _ in range(n):
            frames.append(self.frame())
            time.sleep(gap_s)
        return np.median(np.stack(frames), axis=0).astype(np.uint8)


class Wrist:
    """Keeps the camera open in a thread and hands out the newest frame."""

    def __init__(self, match="Dabai_DC1", width=1280, height=720):
        self.match, self.size = match, (width, height)
        self.lock = threading.Lock()
        self.latest, self.t, self.error = None, 0.0, None
        self._stop = threading.Event()
        threading.Thread(target=self._run, daemon=True, name="wrist-cam").start()

    def _device(self):
        hits = sorted(glob.glob(f"/dev/v4l/by-id/*{self.match}*-video-index0"))
        return os.path.realpath(hits[0]) if hits else None

    def _run(self):
        while not self._stop.is_set():
            dev = self._device()
            if not dev:
                self.error = f"wrist camera ({self.match}) is not plugged in"
                time.sleep(2.0)
                continue
            cap = cv2.VideoCapture(int(dev.rsplit("video", 1)[1]), cv2.CAP_V4L2)
            cap.set(cv2.CAP_PROP_FOURCC, cv2.VideoWriter_fourcc(*"MJPG"))
            cap.set(cv2.CAP_PROP_FRAME_WIDTH, self.size[0])
            cap.set(cv2.CAP_PROP_FRAME_HEIGHT, self.size[1])
            cap.set(cv2.CAP_PROP_BUFFERSIZE, 1)
            fails = 0
            while not self._stop.is_set() and fails < 20:
                ok, img = cap.read()
                if ok:
                    fails = 0
                    with self.lock:
                        self.latest, self.t, self.error = img, time.time(), None
                else:
                    fails += 1
                    time.sleep(0.05)
            cap.release()
            self.error = "wrist camera stopped delivering frames"
            time.sleep(1.0)

    def frame(self, max_age_s=2.0) -> np.ndarray:
        t_end = time.time() + 5.0
        while time.time() < t_end:
            with self.lock:
                if self.latest is not None and time.time() - self.t < max_age_s:
                    return self.latest.copy()
            time.sleep(0.05)
        raise CameraError(self.error or "wrist camera: no frame")

    def close(self):
        self._stop.set()


def to_jpeg(img: np.ndarray, quality=85, max_side=None) -> bytes:
    if max_side and max(img.shape[:2]) > max_side:
        k = max_side / max(img.shape[:2])
        img = cv2.resize(img, (int(img.shape[1] * k), int(img.shape[0] * k)), interpolation=cv2.INTER_AREA)
    ok, out = cv2.imencode(".jpg", img, [cv2.IMWRITE_JPEG_QUALITY, quality])
    if not ok:
        raise CameraError("could not encode a frame")
    return out.tobytes()
