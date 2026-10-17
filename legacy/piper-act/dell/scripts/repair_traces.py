#!/usr/bin/env python3
"""Repair trace sidecars written while the buffer was never cleared.

The recorder once kept one trace list for the whole session, so each
traces/episode_N.jsonl held episodes 0..N concatenated. The LAST file therefore
contains every row, in recording order. This splits it back apart using the
dataset's own episode lengths (meta/episodes), which are authoritative, and
re-numbers frame_index within each episode.

It refuses to touch anything unless the row count matches the dataset exactly,
and backs the originals up first.

    python repair_traces.py "C:\\Users\\zhy10\\Shared\\Piper Arm\\datasets\\piper_pick_place"
"""
import json
import shutil
import sys
import time
from pathlib import Path

from lerobot.datasets.lerobot_dataset import LeRobotDatasetMetadata


def main():
    root = Path(sys.argv[1])
    meta = LeRobotDatasetMetadata(f"local/{root.name}", root=root)
    tdir = root / "traces"
    n_eps = meta.total_episodes
    lengths = [meta.episodes[e]["dataset_to_index"] - meta.episodes[e]["dataset_from_index"]
               for e in range(n_eps)]
    print(f"episodes={n_eps}  lengths={lengths}  total={sum(lengths)}")

    last = tdir / f"episode_{n_eps - 1:06d}.jsonl"
    rows = [l for l in last.read_text(encoding="utf-8").splitlines() if l.strip()]
    print(f"{last.name}: {len(rows)} rows")

    already_ok = all(
        sum(1 for l in (tdir / f"episode_{e:06d}.jsonl").read_text(encoding="utf-8").splitlines()
            if l.strip()) == lengths[e]
        for e in range(n_eps))
    if already_ok:
        print("every trace already matches its episode length -- nothing to repair")
        return 0

    if len(rows) != sum(lengths):
        print(f"REFUSING: last file has {len(rows)} rows but the dataset has "
              f"{sum(lengths)} frames, so it is not a clean cumulative record.")
        return 1

    backup = root / f"traces_backup_{time.strftime('%Y%m%d_%H%M%S')}"
    shutil.copytree(tdir, backup)
    print(f"backed up originals -> {backup.name}")

    start = 0
    for e, n in enumerate(lengths):
        out = tdir / f"episode_{e:06d}.jsonl"
        with open(out, "w", encoding="utf-8") as fh:
            for i, line in enumerate(rows[start:start + n]):
                r = json.loads(line)
                r["frame_index"] = i
                fh.write(json.dumps(r, separators=(",", ":")) + "\n")
        print(f"  wrote {out.name}: {n} rows (source rows {start}..{start + n - 1})")
        start += n
    return 0


if __name__ == "__main__":
    sys.exit(main())
