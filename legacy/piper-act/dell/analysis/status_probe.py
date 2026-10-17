import sys, time
from pathlib import Path
import numpy as np
sys.path.insert(0, r"C:\Users\zhy10\Shared\Piper Arm\deploy")
import piper_policy_portal as portal

arm = portal.RealArm("candle", r"D:\Piper-CAN-Teleop\scripts")
arm.connect()
piper = arm.piper
ages, raw_modes = [], []
for _ in range(60):
    st = piper.GetArmStatus()
    ages.append((time.time() - st.time_stamp) if st.time_stamp else 999.0)
    raw_modes.append(int(st.arm_status.ctrl_mode))
    time.sleep(0.1)
ages = np.array(ages)
print("raw ctrl_mode now:", raw_modes[-1], "  (distinct seen:", sorted(set(raw_modes)), ")")
print("status age over 6s: median %.3fs  p10 %.3f  p90 %.3f  max %.3f" % (np.median(ages), np.percentile(ages,10), np.percentile(ages,90), ages.max()))
print("gated ctrl_mode() (0.5s window) sampled 20x:", [arm.ctrl_mode() for _ in range(20)])
print("  -> gate accepts %.0f%% of the time" % (100 * (ages < 0.5).mean()))
# also: joint feedback age for comparison (the OTHER freshness signal)
jm = piper.GetArmJointMsgs()
print("joint feedback age: %.3fs" % (time.time() - jm.time_stamp if jm.time_stamp else 999))
arm.close()
