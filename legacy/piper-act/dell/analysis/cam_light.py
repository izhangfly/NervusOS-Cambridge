#!/usr/bin/env python3
"""Is the C270's 1 fps darkness or bandwidth?

1.0 fps == roughly a 1 second exposure, which is what a webcam does when it can
see almost nothing. If the frame is near-black, the fix is light, not USB. If
dropping resolution raises the rate, it is bandwidth instead.
"""
import os
import sys
import time

import cv2

OUT = os.path.dirname(os.path.abspath(__file__))
C270, DABAI = 2, 0


def grab(idx, w=640, h=480, warm=8):
    cap = cv2.VideoCapture(idx, cv2.CAP_DSHOW)
    if not cap.isOpened():
        return None, None
    cap.set(cv2.CAP_PROP_FRAME_WIDTH, w)
    cap.set(cv2.CAP_PROP_FRAME_HEIGHT, h)
    for _ in range(warm):
        cap.read()
    ok, frame = cap.read()
    return cap, (frame if ok else None)


print("brightness (mean pixel value, 0=black 255=white)")
for idx, label in ((C270, "c270"), (DABAI, "dabai")):
    cap, frame = grab(idx)
    if frame is None:
        print(f"  {label}: no frame")
    else:
        m = frame.mean()
        verdict = "VERY DARK" if m < 25 else ("dim" if m < 60 else "ok")
        path = os.path.join(OUT, f"_{label}.jpg")
        cv2.imwrite(path, frame)
        print(f"  {label:6} mean={m:6.1f}  {verdict:10} saved {path}")
    if cap:
        cap.release()
    time.sleep(0.8)

print("\nC270 frame rate vs resolution (rising rate => bandwidth, flat => light)")
for w, h in ((640, 480), (320, 240), (160, 120)):
    cap = cv2.VideoCapture(C270, cv2.CAP_DSHOW)
    if not cap.isOpened():
        print(f"  {w}x{h}: open failed")
        continue
    cap.set(cv2.CAP_PROP_FRAME_WIDTH, w)
    cap.set(cv2.CAP_PROP_FRAME_HEIGHT, h)
    for _ in range(5):
        cap.read()
    n, t0 = 0, time.time()
    last = None
    while time.time() - t0 < 2.0:
        ok, f = cap.read()
        if ok:
            n += 1
            last = f
    b = f"{last.mean():.1f}" if last is not None else "-"
    print(f"  {int(cap.get(3))}x{int(cap.get(4))}: {n / 2.0:5.1f} fps  brightness={b}")
    cap.release()
    time.sleep(0.8)
