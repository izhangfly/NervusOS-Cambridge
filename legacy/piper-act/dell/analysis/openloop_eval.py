#!/usr/bin/env python3
"""Open-loop audit of a checkpoint on the recorded demos, through the portal's OWN Policy class.

For sampled frames: feed (state, wrist image) exactly as the portal will, compare the 30-step chunk
with the demo's real next 30 actions. Baselines: "hold still" (repeat current state). Also feeds the
same image as BGR to prove colour order matters. Reports Dell CPU latency.
"""
import sys
import time
from pathlib import Path

import numpy as np

SHARED = Path(r"C:\Users\zhy10\Shared\Piper Arm")
sys.path.insert(0, str(SHARED / "deploy"))
import piper_policy_portal as portal  # noqa: E402

ckpt = sys.argv[1] if len(sys.argv) > 1 else "act_pick_place_v1_100000"
eps = [int(x) for x in (sys.argv[2].split(",") if len(sys.argv) > 2 else "0,4,13,27,41,49".split(","))]
stride = int(sys.argv[3]) if len(sys.argv) > 3 else 45

from lerobot.datasets.lerobot_dataset import LeRobotDataset  # noqa: E402

pol = portal.Policy(SHARED / "policies" / ckpt, threads=4)
H = pol.chunk_size
print(f"checkpoint {ckpt}: chunk {H}, image keys {pol.image_keys}")
root = SHARED / "datasets" / "piper_pick_place"

err_pol, err_hold, err_bgr, lat = [], [], [], []
per_ep = {}
for e in eps:
    ds = LeRobotDataset("local/piper_pick_place", root=root, episodes=[e])
    n = len(ds)
    acts = np.stack([ds.hf_dataset[i]["action"].numpy() if hasattr(ds.hf_dataset[i]["action"], "numpy")
                     else np.asarray(ds.hf_dataset[i]["action"]) for i in range(n)])
    eh, ep_ = [], []
    for i in range(0, n - H, stride):
        item = ds[i]
        img = (item["observation.images.wrist"].permute(1, 2, 0).numpy() * 255).round().astype(np.uint8)
        st = item["observation.state"].numpy()
        state = {j: float(st[k]) for k, j in enumerate(portal.JOINTS)}
        t = time.perf_counter()
        ch = pol.chunk(state, img)
        lat.append((time.perf_counter() - t) * 1000)
        gt = acts[i:i + H]
        e_p = np.abs(ch - gt).mean(0)
        e_h = np.abs(st[None, :] - gt).mean(0)
        e_b = np.abs(pol.chunk(state, img[:, :, ::-1].copy()) - gt).mean(0)
        err_pol.append(e_p); err_hold.append(e_h); err_bgr.append(e_b)
        ep_.append(e_p.mean()); eh.append(e_h.mean())
    per_ep[e] = (n, len(ep_), float(np.mean(ep_)), float(np.mean(eh)))

P, Hd, B = np.mean(err_pol, 0), np.mean(err_hold, 0), np.mean(err_bgr, 0)
print("\nper-episode mean |error| over a 30-step chunk (normalized units, joints -100..100, gripper 0..100)")
print(f"{'ep':>3} {'frames':>6} {'samples':>7} {'policy':>7} {'hold-still':>10}")
for e, (n, s, p, h) in per_ep.items():
    print(f"{e:>3} {n:>6} {s:>7} {p:>7.2f} {h:>10.2f}")
print("\nper joint:      " + " ".join(f"{j:>8}" for j in portal.JOINTS))
print("policy (RGB)    " + " ".join(f"{v:>8.2f}" for v in P))
print("hold-still      " + " ".join(f"{v:>8.2f}" for v in Hd))
print("policy fed BGR  " + " ".join(f"{v:>8.2f}" for v in B))
print(f"\noverall: policy {P.mean():.2f}  vs hold-still {Hd.mean():.2f}  vs BGR {B.mean():.2f}  "
      f"-> policy explains {100 * (1 - P.mean() / Hd.mean()):.0f}% of the motion")
lat = np.array(lat[2:] if len(lat) > 4 else lat)
print(f"Dell CPU inference: median {np.median(lat):.0f} ms, p90 {np.percentile(lat, 90):.0f} ms, max {lat.max():.0f} ms "
      f"(n={len(lat)})")
