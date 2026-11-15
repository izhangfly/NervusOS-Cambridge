#!/usr/bin/env python3
"""Overhead camera server: a Surface Pro 9's rear camera, mounted above the table, as JPEG frames over HTTP.

The rear sensor (OV13858 behind Intel IPU6) has no webcam driver; libcamera captures it (see
hosted/surface/build-libcamera.sh). One GStreamer pipeline keeps the camera running, so exposure stays
settled, and writes numbered JPEGs to /dev/shm; this server hands out the newest complete one.

    GET /frame.jpg     the latest frame
    GET /info          {"age_s", "width", "height", "frames"}

It listens on loopback only. The robot host reaches it through an SSH tunnel:
    ssh -N -L 127.0.0.1:8798:127.0.0.1:8798 <this machine>
"""
import argparse
import glob
import json
import os
import subprocess
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

DIR = "/dev/shm/overhead-cam"
ENV = dict(os.environ, LD_LIBRARY_PATH="/usr/local/lib/x86_64-linux-gnu",
           GST_PLUGIN_PATH="/usr/local/lib/x86_64-linux-gnu/gstreamer-1.0")


def pipeline(camera: str, width: int, height: int, fps: int) -> subprocess.Popen:
    os.makedirs(DIR, exist_ok=True)
    for f in glob.glob(DIR + "/f*.jpg"):
        os.remove(f)
    # gst-launch unescapes backslashes in values, and the camera id starts with one.
    cmd = ["gst-launch-1.0", "-q", "libcamerasrc", "camera-name=\\" + camera,
           "!", f"video/x-raw,width={width},height={height}", "!", "videorate", "!", f"video/x-raw,framerate={fps}/1",
           "!", "videoconvert", "!", "jpegenc", "quality=90",
           "!", "multifilesink", f"location={DIR}/f%06d.jpg", "max-files=6"]
    return subprocess.Popen(cmd, env=ENV, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)


def newest():
    """(path, mtime) of the newest file that is certainly complete: the one before the newest."""
    files = sorted(glob.glob(DIR + "/f*.jpg"))
    if len(files) < 2:
        return None, 0.0
    f = files[-2]
    try:
        return f, os.path.getmtime(f)
    except OSError:
        return None, 0.0


def keep_running(args):
    while True:
        p = pipeline(args.camera, args.width, args.height, args.fps)
        p.wait()
        time.sleep(2.0)             # the camera was busy or the pipeline died: try again


class H(BaseHTTPRequestHandler):
    def log_message(self, *a):
        pass

    def _send(self, code, body, ctype):
        self.send_response(code)
        self.send_header("Content-Type", ctype)
        self.send_header("Content-Length", str(len(body)))
        self.send_header("Cache-Control", "no-store")
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        f, t = newest()
        if self.path.startswith("/frame.jpg"):
            if not f or time.time() - t > 5.0:
                return self._send(503, b'{"error":"no fresh frame"}', "application/json")
            try:
                with open(f, "rb") as fh:
                    return self._send(200, fh.read(), "image/jpeg")
            except OSError:
                return self._send(503, b'{"error":"frame vanished"}', "application/json")
        if self.path.startswith("/info"):
            body = {"age_s": round(time.time() - t, 2) if f else None, "width": self.server.args.width,
                    "height": self.server.args.height, "frames": len(glob.glob(DIR + "/f*.jpg"))}
            return self._send(200, json.dumps(body).encode(), "application/json")
        self._send(404, b"{}", "application/json")


def main():
    ap = argparse.ArgumentParser(description="Overhead camera server (Surface Pro 9 rear camera)")
    ap.add_argument("--camera", default="\\_SB_.PC00.I2C2.CAMR", help="libcamera id (cam -l)")
    ap.add_argument("--width", type=int, default=1280)
    ap.add_argument("--height", type=int, default=720)
    ap.add_argument("--fps", type=int, default=5)
    ap.add_argument("--port", type=int, default=8798)
    args = ap.parse_args()
    threading.Thread(target=keep_running, args=(args,), daemon=True).start()
    srv = ThreadingHTTPServer(("127.0.0.1", args.port), H)
    srv.args = args
    srv.daemon_threads = True
    srv.serve_forever()


if __name__ == "__main__":
    main()
