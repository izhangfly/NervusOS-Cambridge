#!/usr/bin/env python3
"""Phase analysis of all episodes from the Cartesian traces (unclamped gripper).

For each episode: grasp time, release time, gripper width while holding, base
yaw before the grasp (search) and between grasp and release (transport).
The transport always goes LEFT to the red card, so its yaw sign defines "left";
a search turn to the RIGHT is then the opposite sign. Episodes whose pre-grasp
turn to the right is far larger than typical are flagged as search episodes.
"""
import json
import sys
from pathlib import Path

import numpy as np
import pyarrow.parquet as pq

root = Path(sys.argv[1])
eps = []
for p in sorted((root / "meta" / "episodes").rglob("*.parquet")):
    eps += pq.read_table(p).to_pylist()
eps.sort(key=lambda r: r["episode_index"])
FPS = 15.0

rows = []
for r in eps:
    e = r["episode_index"]
    n = r["dataset_to_index"] - r["dataset_from_index"]
    tr = [json.loads(l) for l in (root / "traces" / f"episode_{e:06d}.jsonl")
          .read_text(encoding="utf-8").splitlines() if l.strip()]
    assert len(tr) == n, f"ep {e}: trace {len(tr)} != frames {n}"
    g = np.array([t["gripper_raw"] for t in tr], float)
    yaw = np.array([t["joints"][0] for t in tr], float) * 1.5     # norm +-100 -> +-150 deg
    z = np.array([t["z"] for t in tr], float)

    gmax = np.percentile(g, 98)
    open_thr = 0.85 * gmax
    # grasp: first time the gripper, having been open, closes and stays partly
    # open (holding the block) for at least 1 s
    opened = np.where(g > open_thr)[0]
    t_grasp = t_rel = None
    hold = None
    if len(opened):
        i = opened[0]
        while i < n:
            if g[i] < open_thr:
                j = i
                while j < n and g[j] < open_thr:
                    j += 1
                if (j - i) / FPS >= 1.0:
                    t_grasp, t_rel = i, (j if j < n else None)
                    seg = g[i + int(0.5 * FPS): j] if j - i > FPS else g[i:j]
                    hold = float(np.median(seg))
                    break
                i = j
            i += 1
    pre = yaw[:t_grasp] if t_grasp else yaw
    rows.append(dict(
        ep=e, n=n, secs=n / FPS,
        t_grasp=None if t_grasp is None else t_grasp / FPS,
        t_rel=None if t_rel is None else t_rel / FPS,
        hold=hold, gmax=gmax,
        pre_net=float(pre[-1] - pre[0]) if len(pre) else 0.0,
        pre_extreme=pre, yaw0=float(yaw[0]),
        trans_net=(float(yaw[t_rel - 1] - yaw[t_grasp]) if t_grasp and t_rel else None),
        lift=(float(z[t_grasp:t_rel].max() - z[t_grasp]) if t_grasp and t_rel else None),
    ))

trans = [r["trans_net"] for r in rows if r["trans_net"] is not None]
left_sign = np.sign(np.median(trans)) if trans else 1.0
# how far the arm swung toward the RIGHT (opposite of transport) before grasping
for r in rows:
    pre = r["pre_extreme"]
    toward_right = (-left_sign) * (pre - pre[0]) if len(pre) else np.array([0.0])
    r["right_swing"] = float(toward_right.max())
med = float(np.median([r["right_swing"] for r in rows]))
mad = float(np.median([abs(r["right_swing"] - med) for r in rows])) or 1.0
tg_med = float(np.median([r["t_grasp"] for r in rows if r["t_grasp"]]))

print(f"episodes={len(rows)}  frames={sum(r['n'] for r in rows)}  "
      f"transport yaw sign (=left) = {left_sign:+.0f}")
print(f"median right-swing before grasp = {med:.1f} deg,  median time-to-grasp = {tg_med:.1f} s\n")
print(f"{'ep':>3} {'secs':>5} {'grasp@':>6} {'release@':>8} {'hold_raw':>8} "
      f"{'open_raw':>8} {'right_swing':>11} {'transport_yaw':>13} {'lift_mm':>7}  flag")
search = []
for r in rows:
    flag = ""
    if r["t_grasp"] is None:
        flag = "NO GRASP DETECTED"
    elif r["right_swing"] > med + 4 * mad and r["right_swing"] > 15 and r["t_grasp"] > tg_med:
        flag = "SEARCH"
        search.append(r["ep"])
    f = lambda v, fmt: ("-" if v is None else format(v, fmt))
    print(f"{r['ep']:>3} {r['secs']:>5.1f} {f(r['t_grasp'], '6.1f')} {f(r['t_rel'], '8.1f')} "
          f"{f(r['hold'], '8.0f')} {r['gmax']:>8.0f} {r['right_swing']:>11.1f} "
          f"{f(r['trans_net'], '13.1f')} {f(r['lift'], '7.1f')}  {flag}")

holds = [r["hold"] for r in rows if r["hold"]]
print(f"\nsearch episodes (heuristic): {search}  -> {len(search)}/{len(rows)}")
print(f"gripper width while holding the 5 cm block: median raw {np.median(holds):.0f} "
      f"(range {min(holds):.0f}..{max(holds):.0f});  fully open raw ~{np.median([r['gmax'] for r in rows]):.0f}")
print(f"plugin calibration caps the gripper at raw 68000 -> holding width is "
      f"{'INSIDE' if max(holds) < 68000 else 'OUTSIDE'} the calibrated range")
print(f"durations: min {min(r['secs'] for r in rows):.1f}s  median {np.median([r['secs'] for r in rows]):.1f}s  "
      f"max {max(r['secs'] for r in rows):.1f}s")
