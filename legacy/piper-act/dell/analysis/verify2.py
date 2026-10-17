#!/usr/bin/env python3
"""Prove a two-camera portal recording is what the Mac needs to train ACT."""
import sys
from pathlib import Path

import numpy as np
import torch
from lerobot.datasets.lerobot_dataset import LeRobotDataset

root = Path(sys.argv[1])
name = root.name
ds = LeRobotDataset(f"local/{name}", root=root)

print("=== metadata ===")
print(f"  codebase_version : {ds.meta.info['codebase_version']}")
print(f"  robot_type       : {ds.meta.info['robot_type']}")
print(f"  fps              : {ds.fps}")
print(f"  episodes         : {ds.meta.total_episodes}")
print(f"  frames           : {ds.meta.total_frames}")
print(f"  video keys       : {ds.meta.video_keys}")
for k in ds.meta.video_keys:
    i = ds.meta.info["features"][k]["info"]
    print(f"    {k}: codec={i['video.codec']} {i['video.width']}x{i['video.height']} @{i['video.fps']}")
print(f"  tasks            : {list(ds.meta.tasks.index)}")

print("\n=== features ===")
for k, v in ds.features.items():
    print(f"  {k:34} {v['dtype']:8} {tuple(v['shape'])}")

ncam = len(ds.meta.video_keys)
print(f"\n=== camera count check ===")
print(f"  {ncam} video stream(s) — {'PASS (two views)' if ncam == 2 else 'FAIL, expected 2'}")

print("\n=== action-shift check (action[t] == state[t+1] within an episode) ===")
ep0 = ds.meta.episodes[0]
start, end = ep0["dataset_from_index"], ep0["dataset_to_index"]
bad = checked = 0
for t in range(start, min(end - 1, start + 40)):
    a = ds[t]["action"].numpy()
    s_next = ds[t + 1]["observation.state"].numpy()
    checked += 1
    if not np.allclose(a, s_next, atol=1e-5):
        bad += 1
        if bad <= 3:
            print(f"    MISMATCH t={t}: max|diff|={np.abs(a - s_next).max():.6f}")
print(f"  checked {checked} transitions, {bad} mismatched")
print("  => SHIFT CORRECT" if bad == 0 else "  => SHIFT WRONG")

states = np.stack([ds[t]["observation.state"].numpy()
                   for t in range(start, min(end, start + 30))])
print(f"  state range per joint (max) = {np.ptp(states, axis=0).max():.2f}")

print("\n=== both video streams decode, and differ from each other ===")
imgs = {}
for k in ds.meta.video_keys:
    img = ds[5][k]
    imgs[k] = img
    print(f"  {k}: {tuple(img.shape)} {img.dtype} mean={img.mean():.3f}")
if len(imgs) == 2:
    a, b = list(imgs.values())
    d = float((a - b).abs().mean())
    print(f"  mean abs difference between the two views = {d:.4f}")
    print("  => DISTINCT VIEWS" if d > 0.01 else
          "  => WARNING: the two streams look identical (same camera twice?)")

print("\n=== ACT-style batch ===")
dl = torch.utils.data.DataLoader(ds, batch_size=4, shuffle=True)
b = next(iter(dl))
for k in sorted(b):
    if isinstance(b[k], torch.Tensor):
        print(f"  {k:34} {tuple(b[k].shape)} {b[k].dtype}")
print("\nOK — loadable, decodable, batchable, two views.")
