"""The two cameras.

overhead: the Surface's rear camera above the table, served by overhead/overhead_cam.py and reached through an
          SSH tunnel on 127.0.0.1:8798. It sees the whole table; perception and calibration use it.
wrist:    the Orbbec Dabai DC1's colour stream on the gripper, opened here by its /dev/v4l/by-id name. It is
          the close-up view the model also gets, and a check on what the gripper holds.
"""
from __future__ import annotations

import glob
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
