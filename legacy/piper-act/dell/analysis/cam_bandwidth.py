#!/usr/bin/env python3
"""Find a two-camera configuration that fits the USB bandwidth budget.

Each camera opens fine alone; together the second fails. Both negotiate YUY2
(uncompressed, ~18 MB/s at 640x480x30), which busts a single USB 2.0
controller. MJPG is ~10x smaller, so the fix is to make MJPG actually stick --
or to drop resolution. This reports what each camera really negotiates.
"""
import time

import cv2

DABAI, C270 = 0, 2


def fourcc_of(cap):
    v = int(cap.get(cv2.CAP_PROP_FOURCC))
    return "".join(chr((v >> (8 * i)) & 0xFF) for i in range(4)) if v else "?"


def open_cam(idx, w, h, mjpg):
    cap = cv2.VideoCapture(idx, cv2.CAP_DSHOW)
    if not cap.isOpened():
        return None
    # Order matters for DSHOW: FOURCC before the frame size, and both before
    # the first grab, or the driver keeps its default (YUY2).
    if mjpg:
        cap.set(cv2.CAP_PROP_FOURCC, cv2.VideoWriter_fourcc(*"MJPG"))
    cap.set(cv2.CAP_PROP_FRAME_WIDTH, w)
    cap.set(cv2.CAP_PROP_FRAME_HEIGHT, h)
    ok, _ = cap.read()
    if not ok:
        cap.release()
        return None
    return cap


def measure(cap, seconds=1.5):
    n, t0 = 0, time.time()
    while time.time() - t0 < seconds:
        if cap.read()[0]:
            n += 1
    return n / seconds


def trial(label, w, h, mjpg):
    print(f"\n{label}")
    a = open_cam(DABAI, w, h, mjpg)
    if a is None:
        print("   dabai: OPEN FAILED")
        return False
    b = open_cam(C270, w, h, mjpg)
    if b is None:
        print(f"   dabai ok ({fourcc_of(a)} {int(a.get(3))}x{int(a.get(4))}), "
              "c270: OPEN FAILED  <-- does not fit")
        a.release()
        return False
    fa, fb = measure(a), measure(b)
    print(f"   dabai {fourcc_of(a)} {int(a.get(3))}x{int(a.get(4))} {fa:.1f}fps")
    print(f"   c270  {fourcc_of(b)} {int(b.get(3))}x{int(b.get(4))} {fb:.1f}fps")
    ok = fa >= 10 and fb >= 10
    print("   => BOTH WORK" + ("" if ok else "  (but frame rate is low)"))
    a.release()
    b.release()
    return ok


print("single-camera format probe")
for idx, name in ((DABAI, "dabai"), (C270, "c270")):
    for mjpg in (True, False):
        c = open_cam(idx, 640, 480, mjpg)
        if c:
            print(f"   {name} mjpg_requested={mjpg}: negotiated {fourcc_of(c)} "
                  f"{int(c.get(3))}x{int(c.get(4))} {measure(c):.1f}fps")
            c.release()
        else:
            print(f"   {name} mjpg_requested={mjpg}: OPEN FAILED")
        time.sleep(0.8)

results = []
for label, w, h, mjpg in (
    ("A. both 640x480 MJPG", 640, 480, True),
    ("B. both 640x480 YUY2", 640, 480, False),
    ("C. both 424x240 MJPG", 424, 240, True),
    ("D. both 320x240 MJPG", 320, 240, True),
    ("E. both 320x240 YUY2", 320, 240, False),
):
    results.append((label, trial(label, w, h, mjpg)))
    time.sleep(1.0)

print("\n=== summary ===")
for label, ok in results:
    print(f"  {'PASS' if ok else 'fail'}  {label}")
