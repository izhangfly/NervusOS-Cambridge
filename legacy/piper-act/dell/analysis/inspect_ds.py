import json
from pathlib import Path
import pyarrow.parquet as pq
root = Path(r"C:\Users\zhy10\Shared\Piper Arm\datasets\piper_pick_place")
info = json.loads((root/"meta"/"info.json").read_text(encoding="utf-8"))
print("info.json: total_episodes=%s total_frames=%s" % (info["total_episodes"], info["total_frames"]))
def ok(p):
    try:
        return pq.ParquetFile(p).metadata.num_rows
    except Exception as e:
        return "CORRUPT (%s)" % type(e).__name__
for sub in ("data", "meta/episodes"):
    for p in sorted((root/sub).rglob("*.parquet")):
        print("  %-40s rows=%s  %.1f KB" % (p.relative_to(root), ok(p), p.stat().st_size/1024))
refs = set()
for p in sorted((root/"meta"/"episodes").rglob("*.parquet")):
    t = pq.read_table(p).to_pylist()
    for r in t:
        dk = (r["data/chunk_index"], r["data/file_index"])
        vk = (r["videos/observation.images.wrist/chunk_index"], r["videos/observation.images.wrist/file_index"])
        refs.add(("data",)+dk); refs.add(("video",)+vk)
        print("  episode %d -> data file %s, video file %s, frames %d..%d" % (r["episode_index"], dk, vk, r["dataset_from_index"], r["dataset_to_index"]))
for p in sorted((root/"videos").rglob("*.mp4")):
    fi = int(p.stem.split("-")[1]); ci = int(p.parent.name.split("-")[1])
    print("  %-55s %8.1f KB  %s" % (p.relative_to(root), p.stat().st_size/1024, "referenced" if ("video",ci,fi) in refs else "ORPHAN"))
for p in sorted((root/"data").rglob("*.parquet")):
    fi = int(p.stem.split("-")[1]); ci = int(p.parent.name.split("-")[1])
    print("  %-55s %s" % (p.relative_to(root), "referenced" if ("data",ci,fi) in refs else "ORPHAN"))
print("  traces:", sorted(x.name for x in (root/"traces").glob("*.jsonl")))
print("  images/ files:", sum(1 for _ in (root/"images").rglob("*") if _.is_file()) if (root/"images").exists() else 0)
