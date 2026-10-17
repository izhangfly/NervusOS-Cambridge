#!/usr/bin/env python3
"""Audit REAL recorded episodes: motion, gripper, video, action shift, traces."""
import json
import sys
from pathlib import Path

import numpy as np
from lerobot.datasets.lerobot_dataset import LeRobotDataset

root = Path(sys.argv[1])
out_dir = Path(sys.argv[2])
out_dir.mkdir(parents=True, exist_ok=True)

ds = LeRobotDataset(f"local/{root.name}", root=root)
m = ds.meta
names = m.info["features"]["observation.state"]["names"]
print(f"dataset   : {root}")
print(f"version   : {m.info['codebase_version']}   robot={m.info['robot_type']}   fps={ds.fps}")
print(f"episodes  : {m.total_episodes}    frames: {m.total_frames}")
print(f"cameras   : {m.video_keys}")
print(f"task(s)   : {list(m.tasks.index)}")

# ---- load everything once (parquet columns, no video decode) ----------
hf = ds.hf_dataset.with_format("numpy")
state_all = np.stack(hf["observation.state"])
action_all = np.stack(hf["action"])
ep_idx_all = np.asarray(hf["episode_index"]).reshape(-1)

GRIP = names.index("gripper")
problems = []

print("\n=== per episode ===")
print(f"{'ep':>3} {'frames':>6} {'secs':>6}  {'max joint range':>16}  "
      f"{'gripper min..max':>18}  {'shift':>6}")
lengths = {}
for e in range(m.total_episodes):
    info = m.episodes[e]
    a, b = info["dataset_from_index"], info["dataset_to_index"]
    n = b - a
    lengths[e] = n
    st, ac = state_all[a:b], action_all[a:b]
    assert (ep_idx_all[a:b] == e).all(), f"episode_index mismatch in ep {e}"

    rng = np.ptp(st[:, :6], axis=0)           # six arm joints
    g = st[:, GRIP]
    shift_bad = int((~np.isclose(ac[:-1], st[1:], atol=1e-5)).any(axis=1).sum())
    print(f"{e:>3} {n:>6} {n / ds.fps:>6.1f}  {rng.max():>16.2f}  "
          f"{g.min():>8.2f} .. {g.max():<7.2f}  {shift_bad:>6}")

    if rng.max() < 1.0:
        problems.append(f"ep {e}: arm barely moved (max joint range {rng.max():.2f})")
    if np.ptp(g) < 1.0:
        problems.append(f"ep {e}: gripper never changed ({g.min():.2f}) -- no grasp recorded")
    if shift_bad:
        problems.append(f"ep {e}: {shift_bad} action/state shift mismatches")
    if n / ds.fps < 2:
        problems.append(f"ep {e}: very short ({n / ds.fps:.1f}s)")

print("\n=== joint coverage across ALL episodes (normalized units) ===")
for j, nm in enumerate(names):
    col = state_all[:, j]
    print(f"  {nm:8} min={col.min():8.2f}  max={col.max():8.2f}  range={np.ptp(col):7.2f}")

# ---- video: decode a few frames per episode, check not black/frozen ----
print("\n=== video (sampled frames per episode) ===")
import cv2
key = m.video_keys[0]
for e in range(m.total_episodes):
    a, b = m.episodes[e]["dataset_from_index"], m.episodes[e]["dataset_to_index"]
    picks = [a, a + (b - a) // 3, a + 2 * (b - a) // 3, b - 1]
    means, imgs = [], []
    for i in picks:
        img = ds[i][key].numpy()                  # C,H,W float 0..1
        means.append(float(img.mean()))
        imgs.append(img)
    diffs = [float(np.abs(imgs[k + 1] - imgs[k]).mean()) for k in range(len(imgs) - 1)]
    print(f"  ep {e}: brightness {[round(x, 3) for x in means]}  "
          f"frame-to-frame change {[round(x, 3) for x in diffs]}")
    if max(means) < 0.05:
        problems.append(f"ep {e}: video is black")
    if max(diffs) < 0.005:
        problems.append(f"ep {e}: video looks frozen (no change across the episode)")
    # contact sheet: 4 frames side by side
    tiles = [(np.transpose(im, (1, 2, 0)) * 255).astype(np.uint8)[:, :, ::-1] for im in imgs]
    tiles = [cv2.resize(t, (320, 240)) for t in tiles]
    cv2.imwrite(str(out_dir / f"ep{e}_sheet.jpg"), np.hstack(tiles))

# ---- traces ------------------------------------------------------------
print("\n=== traces sidecar ===")
tdir = root / "traces"
for e in range(m.total_episodes):
    p = tdir / f"episode_{e:06d}.jsonl"
    if not p.exists():
        print(f"  ep {e}: MISSING")
        problems.append(f"ep {e}: trace file missing")
        continue
    rows = [json.loads(l) for l in p.read_text(encoding="utf-8").splitlines() if l.strip()]
    xyz = np.array([[r["x"], r["y"], r["z"]] for r in rows])
    fi = [r["frame_index"] for r in rows]
    ok = len(rows) == lengths[e]
    print(f"  ep {e}: rows={len(rows):5}  episode frames={lengths[e]:5}  "
          f"{'OK' if ok else 'MISMATCH'}   frame_index {fi[0]}..{fi[-1]}   "
          f"tool XYZ range mm = {np.ptp(xyz, axis=0).round(1).tolist()}")
    if not ok:
        problems.append(f"ep {e}: trace has {len(rows)} rows but episode has {lengths[e]} frames")

print("\n=== verdict ===")
if problems:
    print(f"  {len(problems)} problem(s):")
    for p in problems:
        print("   -", p)
else:
    print("  ALL CHECKS PASSED")
print(f"\ncontact sheets written to {out_dir}")
