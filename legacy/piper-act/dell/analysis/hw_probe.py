#!/usr/bin/env python3
"""Read-only hardware probe for policy execution. Never enables, never commands motion."""
import sys
import time
from pathlib import Path

import cv2
import numpy as np

SHARED = Path(r"C:\Users\zhy10\Shared\Piper Arm")
OUT = Path(__file__).parent
sys.path.insert(0, str(SHARED / "deploy"))
sys.path.insert(0, r"D:\Piper-CAN-Teleop\vla")
import piper_policy_portal as portal  # noqa: E402

secs = float(sys.argv[1]) if len(sys.argv) > 1 else 4.0
arm = portal.RealArm("candle", r"D:\Piper-CAN-Teleop\scripts")
arm.connect()
piper = arm.piper
can = piper._C_PiperInterface_V2__arm_can
rx0, t0 = can.bus.rx_count, time.time()
seen_modes = set()
for _ in range(int(secs / 0.25)):
    time.sleep(0.25)
    st = piper.GetArmStatus()
    seen_modes.add((int(st.arm_status.ctrl_mode), int(st.arm_status.arm_status)))
rx1, t1 = can.bus.rx_count, time.time()
st = piper.GetArmStatus()
jm = piper.GetArmJointMsgs()
vals, age = arm.read()
print(f"rx rate            : {(rx1 - rx0) / (t1 - t0):.0f} frames/s")
print(f"status             : ctrl_mode=0x{int(st.arm_status.ctrl_mode):02X} arm_status=0x{int(st.arm_status.arm_status):02X} "
      f"motion={st.arm_status.motion_status} mode_feed={st.arm_status.mode_feed}")
print(f"status age         : {time.time() - st.time_stamp:.3f} s (time_stamp={st.time_stamp})")
print(f"joint msg age      : {time.time() - jm.time_stamp:.3f} s  portal RealArm.read age={age:.3f}")
print(f"(ctrl,arm) seen    : {sorted(seen_modes)}")
print("joints (normalized): " + ", ".join(f"{k}={v:.1f}" for k, v in vals.items()))
try:
    print(f"enable status      : {piper.GetArmEnableStatus()}")
except Exception as e:
    print(f"enable status      : n/a ({e})")
arm.close()

# camera, exactly as the recorder opened it (DSHOW + MJPG)
from camera import find_camera  # noqa: E402
idx = find_camera("Dabai DC1")
for fourcc in ("MJPG", None):
    cap = cv2.VideoCapture(idx, cv2.CAP_DSHOW)
    if fourcc:
        cap.set(cv2.CAP_PROP_FOURCC, cv2.VideoWriter_fourcc(*fourcc))
    cap.set(cv2.CAP_PROP_FRAME_WIDTH, 640)
    cap.set(cv2.CAP_PROP_FRAME_HEIGHT, 480)
    for _ in range(8):
        cap.read()
    n, ts = 0, time.time()
    frame = None
    while time.time() - ts < 3.0:
        ok, f = cap.read()
        if ok:
            n, frame = n + 1, f
    got = int(cap.get(cv2.CAP_PROP_FOURCC))
    tag = "".join(chr((got >> 8 * i) & 0xFF) for i in range(4))
    print(f"camera idx {idx} req {fourcc or 'default'}: got {tag!r} {None if frame is None else frame.shape} "
          f"{n / 3:.1f} fps, brightness {0 if frame is None else frame.mean() / 255:.2f}")
    if frame is not None and fourcc:
        cv2.imwrite(str(OUT / "live_wrist.jpg"), frame)
    cap.release()
