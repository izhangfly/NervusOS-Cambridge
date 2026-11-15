"""Video of both cameras while the model works: overhead and wrist side by side, captioned with the step.

A thread grabs both cameras at a fixed rate and pipes the composite frames into ffmpeg (H.264, yuv420p,
faststart: plays in any browser and on a phone). Without ffmpeg it falls back to OpenCV's mp4v writer.

    rec = Recorder(overhead, wrist, Path("video.mp4"))
    rec.caption("7  move_tool (300, 120, 60)  -  The bottle is centred between the fingers ...")
    ...
    rec.stop()        # -> {"file", "state", "seconds", "size_mm"}
"""
from __future__ import annotations

import shutil
import subprocess
import threading
import time
from pathlib import Path

import cv2
import numpy as np

W, H = 1920, 720                  # overhead 960x720 | wrist 960x540 (+ caption)
CAPTION_H = 72


def _fit(img, w, h):
    """Letterbox img into w x h."""
    out = np.zeros((h, w, 3), np.uint8)
    if img is None:
        return out
    k = min(w / img.shape[1], h / img.shape[0])
    im = cv2.resize(img, (int(img.shape[1] * k), int(img.shape[0] * k)), interpolation=cv2.INTER_AREA)
    y, x = (h - im.shape[0]) // 2, (w - im.shape[1]) // 2
    out[y:y + im.shape[0], x:x + im.shape[1]] = im
    return out


def _wrap(text, width_px, scale):
    words, lines, cur = text.split(), [], ""
    for wd in words:
        trial = (cur + " " + wd).strip()
        if cv2.getTextSize(trial, cv2.FONT_HERSHEY_SIMPLEX, scale, 1)[0][0] > width_px and cur:
            lines.append(cur)
            cur = wd
        else:
            cur = trial
    if cur:
        lines.append(cur)
    return lines


class Recorder:
    def __init__(self, overhead, wrist, path, fps=8, title=""):
        self.overhead, self.wrist, self.path, self.fps = overhead, wrist, Path(path), fps
        self.title = title
        self.text = ""
        self.t0 = time.time()
        self.frames = 0
        self.state = "recording"
        self._stop = threading.Event()
        self._last = {"overhead": None, "wrist": None}
        self.path.parent.mkdir(parents=True, exist_ok=True)
        self.proc, self.writer = None, None
        if shutil.which("ffmpeg"):
            self.proc = subprocess.Popen(
                ["ffmpeg", "-loglevel", "error", "-y", "-f", "rawvideo", "-pix_fmt", "bgr24", "-s", f"{W}x{H}",
                 "-r", str(fps), "-i", "-", "-c:v", "libx264", "-preset", "veryfast", "-crf", "26",
                 "-pix_fmt", "yuv420p", "-movflags", "+faststart", str(self.path)],
                stdin=subprocess.PIPE, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        else:
            self.writer = cv2.VideoWriter(str(self.path), cv2.VideoWriter_fourcc(*"mp4v"), fps, (W, H))
        self.thread = threading.Thread(target=self._run, daemon=True, name="recorder")
        self.thread.start()

    def caption(self, text):
        self.text = text or ""

    def _grab(self, name, cam):
        try:
            self._last[name] = cam.frame() if name == "overhead" else cam.frame(max_age_s=1.0)
        except Exception:                                   # noqa: BLE001 (keep the last frame)
            pass
        return self._last[name]

    def _compose(self):
        top = H - CAPTION_H
        frame = np.zeros((H, W, 3), np.uint8)
        frame[:top, :W // 2] = _fit(self._grab("overhead", self.overhead), W // 2, top)
        frame[:top, W // 2:] = _fit(self._grab("wrist", self.wrist) if self.wrist else None, W // 2, top)
        t = time.time() - self.t0
        stamp = f"{self.title}   {int(t // 60):02d}:{int(t % 60):02d}"
        cv2.putText(frame, stamp, (12, 28), cv2.FONT_HERSHEY_SIMPLEX, 0.7, (255, 255, 255), 2, cv2.LINE_AA)
        cv2.putText(frame, "overhead", (12, top - 12), cv2.FONT_HERSHEY_SIMPLEX, 0.6, (200, 200, 200), 1, cv2.LINE_AA)
        cv2.putText(frame, "wrist", (W // 2 + 12, top - 12), cv2.FONT_HERSHEY_SIMPLEX, 0.6, (200, 200, 200), 1, cv2.LINE_AA)
        for i, line in enumerate(_wrap(self.text, W - 24, 0.62)[:2]):
            cv2.putText(frame, line, (12, top + 28 + 30 * i), cv2.FONT_HERSHEY_SIMPLEX, 0.62, (240, 240, 240), 1, cv2.LINE_AA)
        return frame

    def _write(self, frame):
        if self.proc is not None:
            self.proc.stdin.write(frame.tobytes())
        else:
            self.writer.write(frame)
        self.frames += 1

    def _run(self):
        period = 1.0 / self.fps
        nxt = time.time()
        try:
            while not self._stop.is_set():
                frame = self._compose()
                now = time.time()
                # keep real time: a slow grab is filled by repeating the frame
                while nxt <= now:
                    self._write(frame)
                    nxt += period
                self._stop.wait(max(0.0, nxt - time.time()))
        except (BrokenPipeError, OSError):
            self.state = "failed"

    def seconds(self):
        return round(self.frames / self.fps, 1)

    def stop(self):
        self._stop.set()
        self.thread.join(timeout=5)
        try:
            if self.proc is not None:
                self.proc.stdin.close()
                self.proc.wait(timeout=60)
                ok = self.proc.returncode == 0
            else:
                self.writer.release()
                ok = True
        except Exception:                                   # noqa: BLE001
            ok = False
        if self.state != "failed":
            self.state = "ready" if ok and self.path.exists() and self.frames > 0 else "failed"
        return self.info()

    def info(self):
        size = self.path.stat().st_size / 1e6 if self.path.exists() else 0.0
        return {"file": self.path.name, "state": self.state, "seconds": self.seconds(), "size_mb": round(size, 2)}
