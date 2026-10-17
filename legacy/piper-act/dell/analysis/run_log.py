import json, sys
from pathlib import Path
import numpy as np
J = ["joint1", "joint2", "joint3", "joint4", "joint5", "joint6", "gripper"]
for d in sorted(Path(r"C:\Users\zhy10\Shared\Piper Arm\deploy\runs").iterdir()):
    if not d.is_dir():
        continue
    rows = [json.loads(l) for l in (d / "steps.jsonl").read_text(encoding="utf-8").splitlines() if l.strip()]
    rows = [r for r in rows if r.get("target") is not None]  # stale-hold and human-correction rows log target: null
    S = np.array([[r["state"][j] for j in J] for r in rows])
    R = np.array([r["raw"] for r in rows if r["raw"] is not None])
    T = np.array([[r["target"][j] for j in J] for r in rows])
    print(f"== {d.name}: {len(rows)} steps")
    print("  start state  :", " ".join(f"{v:7.1f}" for v in S[0]))
    print("  state min    :", " ".join(f"{v:7.1f}" for v in S.min(0)))
    print("  state max    :", " ".join(f"{v:7.1f}" for v in S.max(0)))
    print("  model raw min:", " ".join(f"{v:7.1f}" for v in R.min(0)))
    print("  model raw max:", " ".join(f"{v:7.1f}" for v in R.max(0)))
    dev = np.abs(R[:, :6] - S[: len(R), :6]).max(1)
    print(f"  |model - state| per step: median {np.median(dev):.1f}, p95 {np.percentile(dev, 95):.1f}, max {dev.max():.1f}")
    for t in (0, 150, 300, 600, 900, 1300):
        if t < len(rows):
            print(f"  t={rows[t]['t']:5.1f}s state j1..j6,g:", " ".join(f"{v:6.1f}" for v in S[t]))
    # jitter: step-to-step reversal of commanded target
    dT = np.diff(T[:, :6], axis=0)
    print("  target step std per joint:", " ".join(f"{v:5.2f}" for v in dT.std(0)))
