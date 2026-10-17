#!/usr/bin/env python3
"""C270 returns black at 1 fps though the device is healthy and permitted.

Tests, in order of how cheap they are to act on:
  1. does brightness climb given real time (slow auto-exposure convergence)
  2. does the MSMF backend behave differently from DSHOW
  3. do explicit brightness/gain/exposure controls move anything
A camera that stays at 0 through all three is not a software problem.
"""
import os
import time

import cv2

OUT = os.path.dirname(os.path.abspath(__file__))
C270 = 2


def show(cap, seconds, label):
    """Read continuously; report brightness as it evolves."""
    t0 = time.time()
    n = 0
    last = None
    marks = []
    while time.time() - t0 < seconds:
        ok, f = cap.read()
        if ok:
            n += 1
            last = f
            marks.append((round(time.time() - t0, 1), round(float(f.mean()), 1)))
    rate = n / seconds
    trail = " ".join(f"{t}s:{b}" for t, b in marks[:12])
    print(f"  {label}: {rate:.1f}fps  brightness trail -> {trail}")
    return last


print("1. DSHOW, 15 s of continuous reads (auto-exposure has time to settle)")
cap = cv2.VideoCapture(C270, cv2.CAP_DSHOW)
if cap.isOpened():
    cap.set(cv2.CAP_PROP_FRAME_WIDTH, 640)
    cap.set(cv2.CAP_PROP_FRAME_HEIGHT, 480)
    f = show(cap, 15, "dshow")
    if f is not None:
        cv2.imwrite(os.path.join(OUT, "_c270_dshow.jpg"), f)
    print(f"     props: bright={cap.get(cv2.CAP_PROP_BRIGHTNESS)} "
          f"gain={cap.get(cv2.CAP_PROP_GAIN)} "
          f"exposure={cap.get(cv2.CAP_PROP_EXPOSURE)} "
          f"auto_exp={cap.get(cv2.CAP_PROP_AUTO_EXPOSURE)}")
    cap.release()
else:
    print("     open failed")
time.sleep(1.5)

print("\n2. MSMF backend (different driver path than DSHOW)")
cap = cv2.VideoCapture(C270, cv2.CAP_MSMF)
if cap.isOpened():
    cap.set(cv2.CAP_PROP_FRAME_WIDTH, 640)
    cap.set(cv2.CAP_PROP_FRAME_HEIGHT, 480)
    f = show(cap, 8, "msmf")
    if f is not None:
        cv2.imwrite(os.path.join(OUT, "_c270_msmf.jpg"), f)
    cap.release()
else:
    print("     MSMF open failed")
time.sleep(1.5)

print("\n3. DSHOW with brightness/gain pushed to maximum")
cap = cv2.VideoCapture(C270, cv2.CAP_DSHOW)
if cap.isOpened():
    cap.set(cv2.CAP_PROP_FRAME_WIDTH, 640)
    cap.set(cv2.CAP_PROP_FRAME_HEIGHT, 480)
    cap.set(cv2.CAP_PROP_AUTO_EXPOSURE, 0.75)   # auto ON
    cap.set(cv2.CAP_PROP_BRIGHTNESS, 255)
    cap.set(cv2.CAP_PROP_GAIN, 255)
    f = show(cap, 8, "boosted")
    if f is not None:
        cv2.imwrite(os.path.join(OUT, "_c270_boost.jpg"), f)
    cap.release()
else:
    print("     open failed")

print("\nIf every trail stays near 0, the sensor is getting no light or no "
      "power -- not a driver issue. Most likely: the hub it is plugged into "
      "cannot drive it, or the lens is covered.")
