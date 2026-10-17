import sys
from pathlib import Path
import numpy as np, pyarrow.parquet as pq
root = Path(sys.argv[1]); FPS = 15.0
eps = []
for p in sorted((root/"meta"/"episodes").rglob("*.parquet")): eps += pq.read_table(p).to_pylist()
eps.sort(key=lambda r: r["episode_index"])
tbl = pq.read_table(sorted((root/"data").rglob("*.parquet"))[0].parent)  # whole chunk dir
st = np.stack(tbl.column("observation.state").to_numpy(zero_copy_only=False))
idx = tbl.column("index").to_numpy()
order = np.argsort(idx); st = st[order]
starts, ends = [], []
for r in eps:
    s = st[r["dataset_from_index"]:r["dataset_to_index"], :6]
    d0 = np.abs(s - s[0]).max(axis=1); d1 = np.abs(s - s[-1]).max(axis=1)
    first_move = int(np.argmax(d0 > 1.0)) if (d0 > 1.0).any() else len(s)
    last_move = len(s) - 1 - int(np.argmax(d1[::-1] > 1.0)) if (d1 > 1.0).any() else 0
    starts.append(first_move / FPS); ends.append((len(s) - 1 - last_move) / FPS)
starts, ends = np.array(starts), np.array(ends)
print(f"idle before first arm motion: median {np.median(starts):.1f}s  max {starts.max():.1f}s  episodes >2s: {int((starts>2).sum())}  (worst: {sorted(range(len(starts)), key=lambda i:-starts[i])[:5]} = {np.sort(starts)[::-1][:5].round(1).tolist()})")
print(f"idle after last arm motion : median {np.median(ends):.1f}s  max {ends.max():.1f}s  episodes >2s: {int((ends>2).sum())}")
print(f"start pose spread across episodes (normalized joints 1-6): {np.ptp(np.stack([st[r['dataset_from_index'], :6] for r in eps]), axis=0).round(1).tolist()}")
