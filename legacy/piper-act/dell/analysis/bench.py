import sys, time
from pathlib import Path
import numpy as np
SHARED = Path(r"C:\Users\zhy10\Shared\Piper Arm")
sys.path.insert(0, str(SHARED / "deploy"))
import piper_policy_portal as portal
import torch
pol = portal.Policy(SHARED / "policies" / "act_pick_place_v1_100000", threads=4)
img = np.random.randint(0, 255, (480, 640, 3), np.uint8)
s = {j: 0.0 for j in portal.JOINTS}
for th in [int(x) for x in sys.argv[1].split(",")]:
    torch.set_num_threads(th)
    lat = []
    for _ in range(7):
        t = time.perf_counter(); pol.chunk(s, img); lat.append((time.perf_counter() - t) * 1000)
    print(f"threads {th}: median {np.median(lat[2:]):.0f} ms  min {min(lat[2:]):.0f}", flush=True)
