"""Contact sheet of training wrist frames (start / 25% / 50%) for a few episodes + median first state."""
import json
from pathlib import Path

import cv2
import numpy as np
import pyarrow.parquet as pq
from lerobot.datasets.lerobot_dataset import LeRobotDataset

root = Path(r"C:\Users\zhy10\Shared\Piper Arm\datasets\piper_pick_place")
out = Path(__file__).parent
rows = []
for e in (2, 20, 45):
    ds = LeRobotDataset("local/piper_pick_place", root=root, episodes=[e])
    tiles = []
    for f in (0.0, 0.25, 0.5):
        i = int(f * (len(ds) - 1))
        img = (ds[i]["observation.images.wrist"].permute(1, 2, 0).numpy() * 255).astype(np.uint8)
        tiles.append(cv2.resize(cv2.cvtColor(img, cv2.COLOR_RGB2BGR), (320, 240)))
        print(f"ep {e} frame {i}: brightness {img.mean() / 255:.2f}")
    rows.append(np.hstack(tiles))
cv2.imwrite(str(out / "train_sheet.jpg"), np.vstack(rows))

# median first-frame state over all 50 episodes vs the portal's hard-coded start pose
firsts = []
for p in sorted((root / "data").rglob("*.parquet")):
    t = pq.read_table(p, columns=["episode_index", "frame_index", "observation.state"]).to_pandas()
    firsts += [np.asarray(s) for s in t[t.frame_index == 0]["observation.state"]]
F = np.stack(firsts)
names = json.loads((root / "meta/info.json").read_text())["features"]["observation.state"]["names"]
print("n first frames:", len(F))
print("median first state:", {n: round(float(v), 2) for n, v in zip(names, np.median(F, 0))})
print("min first state   :", {n: round(float(v), 2) for n, v in zip(names, F.min(0))})
print("max first state   :", {n: round(float(v), 2) for n, v in zip(names, F.max(0))})
