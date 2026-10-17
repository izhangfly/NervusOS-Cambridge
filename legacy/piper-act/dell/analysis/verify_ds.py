#!/usr/bin/env python3
"""Prove a portal-recorded dataset is what the Mac needs to train ACT."""
import sys
from pathlib import Path

import numpy as np
import torch
from lerobot.datasets.lerobot_dataset import LeRobotDataset

root = Path(sys.argv[1])
ds = LeRobotDataset("local/_portal_test", root=root)

print("=== metadata ===")
print(f"  codebase_version : {ds.meta.info['codebase_version']}")
print(f"  robot_type       : {ds.meta.info['robot_type']}")
print(f"  fps              : {ds.fps}")
print(f"  episodes         : {ds.meta.total_episodes}")
print(f"  frames           : {ds.meta.total_frames}")
print(f"  video keys       : {ds.meta.video_keys}")
for k in ds.meta.video_keys:
    print(f"  codec            : {ds.meta.info['features'][k]['info']['video.codec']}")
print(f"  tasks            : {list(ds.meta.tasks.index)}")

print("\n=== features ===")
for k, v in ds.features.items():
    print(f"  {k:34} {v['dtype']:8} {tuple(v['shape'])}")

print("\n=== sample frame 0 ===")
s = ds[0]
for k in sorted(s):
    v = s[k]
    if isinstance(v, torch.Tensor):
        print(f"  {k:34} {tuple(v.shape)} {v.dtype} "
              f"min={v.float().min():.3f} max={v.float().max():.3f}")
    else:
        print(f"  {k:34} {type(v).__name__} {v!r}"[:96])

# ---- the correctness check that matters -------------------------------
# The portal sets action[t] = state[t+1]. If that shift is right, then within
# one episode, action[t] must equal observation.state[t+1] exactly.
print("\n=== action-shift check (action[t] == state[t+1] within an episode) ===")
ep0 = ds.meta.episodes[0]
start, end = ep0["dataset_from_index"], ep0["dataset_to_index"]
print(f"  episode 0 spans rows {start}..{end - 1}")

bad = 0
checked = 0
for t in range(start, min(end - 1, start + 40)):
    a = ds[t]["action"].numpy()
    s_next = ds[t + 1]["observation.state"].numpy()
    checked += 1
    if not np.allclose(a, s_next, atol=1e-5):
        bad += 1
        if bad <= 3:
            print(f"    MISMATCH at t={t}: max|diff|={np.abs(a - s_next).max():.6f}")
print(f"  checked {checked} transitions, {bad} mismatched")
print("  => SHIFT CORRECT" if bad == 0 else "  => SHIFT WRONG")

# Sanity: state must actually change (a frozen arm would pass trivially).
states = np.stack([ds[t]["observation.state"].numpy() for t in range(start, start + 30)])
print(f"  state motion across 30 frames: max range per joint = "
      f"{np.ptp(states, axis=0).max():.2f} (must be > 0)")

print("\n=== video decode ===")
for k in ds.meta.video_keys:
    img = ds[5][k]
    print(f"  {k}: {tuple(img.shape)} {img.dtype} "
          f"min={img.min():.3f} max={img.max():.3f}")

print("\n=== ACT-style batch (what lerobot-train feeds the policy) ===")
dl = torch.utils.data.DataLoader(ds, batch_size=4, shuffle=True)
b = next(iter(dl))
for k in sorted(b):
    if isinstance(b[k], torch.Tensor):
        print(f"  {k:34} {tuple(b[k].shape)} {b[k].dtype}")
print("\nOK — dataset is loadable, decodable, and batchable.")
