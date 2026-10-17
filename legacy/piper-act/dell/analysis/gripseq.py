import json, sys
from pathlib import Path
import numpy as np
root = Path(sys.argv[1]); FPS = 15.0
def state(v):
    if v > 80000: return "OPEN"
    if 45000 <= v <= 60000: return "HOLD"
    if v < 10000: return "CLOSED"
    return "mid"
def seq(g):
    out, cur, start = [], state(g[0]), 0
    for i in range(1, len(g)):
        s = state(g[i])
        if s != cur:
            out.append((cur, start, i)); cur, start = s, i
    out.append((cur, start, len(g)))
    # drop blips shorter than 0.4 s by merging into neighbours
    return [(s, a, b) for s, a, b in out if (b - a) / FPS >= 0.4]
flagged = [1, 6, 8, 20, 21, 23, 30, 31, 40, 48, 49]
normal_examples = [2, 15, 35]
for e in flagged + normal_examples:
    tr = [json.loads(l) for l in (root/"traces"/f"episode_{e:06d}.jsonl").read_text(encoding="utf-8").splitlines() if l.strip()]
    g = np.array([t["gripper_raw"] for t in tr], float)
    z = np.array([t["z"] for t in tr], float)
    s = seq(g)
    txt = "  ".join(f"{name}[{a/FPS:.1f}-{b/FPS:.1f}s]" for name, a, b in s)
    tag = "(normal ref)" if e in normal_examples else ""
    print(f"ep {e:2d} {tag:12} end_gripper={g[-1]:7.0f}  last_state={s[-1][0]:6}  max_after_first_hold={g[[i for i in range(len(g)) if 45000<=g[i]<=60000][0]:].max() if any(45000<=x<=60000 for x in g) else float('nan'):7.0f}")
    print(f"       {txt}")
