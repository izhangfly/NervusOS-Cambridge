#!/usr/bin/env python3
"""Were any recorded episodes captured while the camera had collapsed to ~1 fps?

A camera delivering 1 fps into a 15 fps loop leaves ~14 of every 15 dataset
frames as exact repeats of the previous image. Decode runs of consecutive frames
across each episode and measure how many are near-identical to their neighbour.
Healthy capture with a hand-moved arm: very few repeats.
"""
import sys
from pathlib import Path

import numpy as np
from lerobot.datasets.lerobot_dataset import LeRobotDataset

root = Path(sys.argv[1])
ds = LeRobotDataset(f"local/{root.name}", root=root)
key = ds.meta.video_keys[0]
RUN = 45          # 3 s of consecutive frames per sample point
THRESH = 0.002    # mean abs pixel diff (0..1) below which two frames are "the same"

worst = 0.0
for e in range(ds.meta.total_episodes):
    a, b = ds.meta.episodes[e]["dataset_from_index"], ds.meta.episodes[e]["dataset_to_index"]
    starts = [a, a + (b - a) // 4, a + (b - a) // 2, a + 3 * (b - a) // 4]
    same = total = 0
    per_seg = []
    for s in starts:
        s = min(s, b - RUN - 1)
        prev = None
        seg_same = 0
        for i in range(s, s + RUN):
            img = ds[i][key].numpy()
            if prev is not None:
                d = float(np.abs(img - prev).mean())
                total += 1
                if d < THRESH:
                    same += 1
                    seg_same += 1
            prev = img
        per_seg.append(f"{100 * seg_same / (RUN - 1):.0f}%")
    frac = same / total
    worst = max(worst, frac)
    verdict = ("HEALTHY" if frac < 0.2 else
               "SUSPECT (partly low fps)" if frac < 0.6 else
               "BAD - camera was ~1 fps")
    print(f"  ep {e}: repeated frames {100 * frac:5.1f}%   by segment {per_seg}   {verdict}")

print("\nverdict:", "all episodes captured at a healthy frame rate" if worst < 0.2
      else "some episodes were recorded with a degraded camera -- see above")
