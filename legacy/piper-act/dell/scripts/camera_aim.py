#!/usr/bin/env python3
"""Live brightness meter -- aim a camera, or prove one is blind.

Prints a bar once a second per camera so you can point it while watching, and
saves the last frame of each so you can see what it actually captured.

    python camera_aim.py                 # every camera, 20 s
    python camera_aim.py --seconds 40
    python camera_aim.py --camera C270   # just the one whose name matches

Reading it:
    brightness 0.0 and ~1 fps      the sensor sees nothing: lens covered, film
                                   still on it, facing something black, or the
                                   camera is not really powered
    brightness 60-180              well exposed, good for recording
    brightness > 220               blown out; point away from the light
Shine a phone torch into the lens: a working camera jumps to 200+ immediately.
"""
import argparse
import sys
import time
from pathlib import Path

import cv2

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE.parent / "vla"))


def bar(v, width=28):
    n = int(max(0.0, min(1.0, v / 255.0)) * width)
    return "#" * n + "." * (width - n)


def verdict(v, fps):
    if v < 3:
        return "BLIND - no light reaching the sensor"
    if v < 25:
        return "very dark"
    if v < 60:
        return "dim - add light"
    if v > 220:
        return "blown out"
    return "good" if fps >= 10 else "good exposure, low fps"


def main():
    p = argparse.ArgumentParser()
    p.add_argument("--seconds", type=int, default=20)
    p.add_argument("--camera", default=None, help="substring of the device name")
    p.add_argument("--width", type=int, default=640)
    p.add_argument("--height", type=int, default=480)
    a = p.parse_args()

    from camera import list_cameras
    cams = [(i, n) for i, n in list_cameras()
            if a.camera is None or a.camera.lower() in n.lower()]
    if not cams:
        print("no matching camera")
        return 1

    caps = {}
    for idx, name in cams:
        c = cv2.VideoCapture(idx, cv2.CAP_DSHOW)
        if not c.isOpened():
            print(f"  {name}: OPEN FAILED (in use, or not enough USB bandwidth)")
            continue
        c.set(cv2.CAP_PROP_FRAME_WIDTH, a.width)
        c.set(cv2.CAP_PROP_FRAME_HEIGHT, a.height)
        caps[idx] = (name, c)
        print(f"  opened {idx}: {name}")

    if not caps:
        return 1
    print(f"\naim the cameras now — {a.seconds}s\n")

    t_end = time.time() + a.seconds
    last = {}
    while time.time() < t_end:
        for idx, (name, c) in caps.items():
            n, t0, frame = 0, time.time(), None
            while time.time() - t0 < 1.0 / len(caps):
                ok, f = c.read()
                if ok:
                    n += 1
                    frame = f
            if frame is None:
                print(f"  {name[:20]:20} no frame")
                continue
            last[idx] = frame
            v = float(frame.mean())
            fps = n / (1.0 / len(caps))
            print(f"  {name[:20]:20} {bar(v)} {v:6.1f} {fps:5.1f}fps  {verdict(v, fps)}")
        print()

    for idx, (name, c) in caps.items():
        if idx in last:
            out = HERE / f"aim_{idx}.jpg"
            cv2.imwrite(str(out), last[idx])
            print(f"  saved {out}")
        c.release()
    return 0


if __name__ == "__main__":
    sys.exit(main())
