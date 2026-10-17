import json, sys
from pathlib import Path
import numpy as np
from lerobot.datasets.lerobot_dataset import LeRobotDataset
root = Path(sys.argv[1])
ds = LeRobotDataset(f"local/{root.name}", root=root)
st = np.stack(ds.hf_dataset.with_format("numpy")["observation.state"])
bad = 0
for e in range(ds.meta.total_episodes):
    a, b = ds.meta.episodes[e]["dataset_from_index"], ds.meta.episodes[e]["dataset_to_index"]
    rows = [json.loads(l) for l in (root/"traces"/f"episode_{e:06d}.jsonl").read_text(encoding="utf-8").splitlines() if l.strip()]
    tj = np.array([r["joints"] for r in rows])
    fi = [r["frame_index"] for r in rows]
    err = np.abs(tj - st[a:b]).max()
    seq = fi == list(range(len(rows)))
    ok = len(rows) == b - a and err < 1e-2 and seq
    bad += not ok
    print(f"  ep {e}: rows={len(rows)} frames={b-a}  max |trace joints - dataset state| = {err:.4f}  frame_index 0..N sequential={seq}  {'ALIGNED' if ok else 'MISALIGNED'}")
print("ALL TRACES ALIGNED WITH THE DATASET" if bad == 0 else f"{bad} episode(s) misaligned")
