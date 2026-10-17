#!/usr/bin/env python3
"""
What can the Orbbec Dabai DC1 actually deliver on this machine?

The Dabai family speaks Orbbec's legacy OpenNI protocol, so only the RGB module
enumerates as a standard UVC camera (that is the MI_00 node Windows shows as
"Dabai DC1"). Depth arrives on a separate interface that needs OrbbecSDK v1's
Windows driver — SDK v2 dropped OpenNI devices entirely. This probes the UVC
side, which is what a VLA policy needs first, and reports what it found.
"""
import sys
import time

import cv2

MAX_INDEX = 6


def describe(cap):
    w = int(cap.get(cv2.CAP_PROP_FRAME_WIDTH))
    h = int(cap.get(cv2.CAP_PROP_FRAME_HEIGHT))
    fps = cap.get(cv2.CAP_PROP_FPS)
    fourcc = int(cap.get(cv2.CAP_PROP_FOURCC))
    cc = "".join(chr((fourcc >> (8 * i)) & 0xFF) for i in range(4)) if fourcc else "?"
    return w, h, fps, cc


def main():
    print("Scanning DirectShow video devices...\n")
    found = []
    for i in range(MAX_INDEX):
        cap = cv2.VideoCapture(i, cv2.CAP_DSHOW)
        if not cap.isOpened():
            cap.release()
            continue
        ok, frame = cap.read()
        if ok and frame is not None:
            w, h, fps, cc = describe(cap)
            # Time a short burst for the real frame rate.
            n, t0 = 0, time.time()
            while time.time() - t0 < 1.0:
                if cap.read()[0]:
                    n += 1
            print(f"  index {i}: {w}x{h} @ {fps:.0f} reported, {n} fps measured, "
                  f"fourcc={cc}, frame={frame.shape}")
            found.append((i, w, h, n))
        else:
            print(f"  index {i}: opens but no frame")
        cap.release()

    if not found:
        print("\nNo working video device. Check the camera is plugged in.")
        return 1

    print("\nHighest-resolution device is the one to use for VLA input.")
    best = max(found, key=lambda f: f[1] * f[2])
    print(f"  -> index {best[0]} at {best[1]}x{best[2]}, {best[3]} fps")

    out = "camera_probe_frame.jpg"
    cap = cv2.VideoCapture(best[0], cv2.CAP_DSHOW)
    ok, frame = cap.read()
    if ok:
        cv2.imwrite(out, frame)
        print(f"  saved a test frame to {out}")
    cap.release()
    return 0


if __name__ == "__main__":
    sys.exit(main())
