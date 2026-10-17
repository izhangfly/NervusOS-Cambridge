#!/usr/bin/env python3
"""Does the name pygrabber reports actually match the stream OpenCV opens?

pygrabber and OpenCV's DirectShow backend enumerate video devices separately.
They usually agree, but they are not required to -- a driver that registers a
device twice (the Logitech package does exactly this for the C270) can shift
one list relative to the other. When they diverge, opening "by name" silently
gives you a different camera, which is the worst failure mode here: the dataset
looks fine and the policy learns from the wrong viewpoint.

This opens every index on its own and saves a frame labelled with the name that
index is *claimed* to be, so the mapping can be checked by eye.

    python camera_identity.py
    # then look at scripts/ident_<i>_<name>.jpg
"""
import sys
from pathlib import Path

import cv2

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE.parent / "vla"))

MAX_INDEX = 6


def main():
    try:
        from pygrabber.dshow_graph import FilterGraph
        names = FilterGraph().get_input_devices()
    except Exception as e:
        print(f"pygrabber unavailable ({e}); cannot compare names")
        names = []

    print("pygrabber enumeration (what find_camera matches against):")
    for i, n in enumerate(names):
        print(f"   {i}: {n}")

    print("\nopening each index separately with OpenCV/DSHOW:")
    for i in range(MAX_INDEX):
        cap = cv2.VideoCapture(i, cv2.CAP_DSHOW)
        if not cap.isOpened():
            cap.release()
            continue
        cap.set(cv2.CAP_PROP_FRAME_WIDTH, 640)
        cap.set(cv2.CAP_PROP_FRAME_HEIGHT, 480)
        for _ in range(8):
            cap.read()
        ok, frame = cap.read()
        claimed = names[i] if i < len(names) else "?"
        if ok and frame is not None:
            safe = "".join(c if c.isalnum() else "_" for c in claimed)[:24]
            out = HERE / f"ident_{i}_{safe}.jpg"
            cv2.imwrite(str(out), frame)
            print(f"   index {i}: claimed={claimed!r:28} "
                  f"{frame.shape[1]}x{frame.shape[0]} mean={frame.mean():6.1f} "
                  f"-> {out.name}")
        else:
            print(f"   index {i}: claimed={claimed!r:28} opened but no frame")
        cap.release()

    print("\nCompare each saved image against its claimed name. If the image "
          "does not match the name, the two enumerations disagree and opening "
          "by name is unsafe on this machine.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
