import ctypes, sys, time
from pathlib import Path
import numpy as np
k32 = ctypes.windll.kernel32
h = k32.GetCurrentProcess()
mask = int(sys.argv[1], 16)
if mask:
    print("affinity ok:", bool(k32.SetProcessAffinityMask(h, mask)))
if len(sys.argv) > 3 and sys.argv[3] == "high":
    print("priority ok:", bool(k32.SetPriorityClass(h, 0x00008000)))  # ABOVE_NORMAL
SHARED = Path(r"C:\Users\zhy10\Shared\Piper Arm")
sys.path.insert(0, str(SHARED / "deploy"))
import piper_policy_portal as portal
import torch
pol = portal.Policy(SHARED / "policies" / "act_pick_place_v1_100000", threads=4)
img = np.random.randint(0, 255, (480, 640, 3), np.uint8)
s = {j: 0.0 for j in portal.JOINTS}
for th in [int(x) for x in sys.argv[2].split(",")]:
    torch.set_num_threads(th)
    lat = []
    for _ in range(7):
        t = time.perf_counter(); pol.chunk(s, img); lat.append((time.perf_counter() - t) * 1000)
    print(f"mask {sys.argv[1]} threads {th}: median {np.median(lat[2:]):.0f} ms  min {min(lat[2:]):.0f}", flush=True)
