import json, sys
from pathlib import Path
import numpy as np
d = Path(r"C:\Users\zhy10\Shared\Piper Arm\deploy\runs") / sys.argv[1]
t0, t1 = float(sys.argv[2]), float(sys.argv[3])
rows = [json.loads(l) for l in (d / "steps.jsonl").read_text(encoding="utf-8").splitlines() if l.strip()]
rows = [r for r in rows if r.get("target") is not None]  # stale-hold and human-correction rows log target: null
print("   t     k  obs | plan_g  sent_g  meas_g | j1 plan/sent/meas     | j6 plan/sent/meas")
for r in rows:
    if t0 <= r["t"] <= t1:
        raw = r["raw"] or [float("nan")] * 7
        print(f"{r['t']:6.2f} {r['k']:5d} {str(r['obs_step']):>4} | {raw[6]:6.1f}  {r['target']['gripper']:6.1f}  {r['state']['gripper']:6.1f} | "
              f"{raw[0]:6.1f} {r['target']['joint1']:6.1f} {r['state']['joint1']:6.1f} | {raw[5]:6.1f} {r['target']['joint6']:6.1f} {r['state']['joint6']:6.1f}")
