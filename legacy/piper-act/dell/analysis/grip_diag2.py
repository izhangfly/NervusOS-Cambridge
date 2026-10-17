import json, sys, itertools
from pathlib import Path
import numpy as np
root = Path(r"C:\Users\zhy10\Shared\Piper Arm\deploy\runs")
for name in sys.argv[1:]:
    rows = [json.loads(l) for l in (root / name / "steps.jsonl").read_text(encoding="utf-8").splitlines() if l.strip()]
    rows = [r for r in rows if r.get("target") is not None]
    g_meas = np.array([r["state"]["gripper"] for r in rows])
    g_sent = np.array([r["target"]["gripper"] for r in rows])
    g_plan = np.array([r["raw"][6] if r["raw"] else np.nan for r in rows])
    modes = [r.get("grip") for r in rows]
    t = np.array([r["t"] for r in rows])
    cam = np.array([r["cam_age_ms"] for r in rows]); arm = np.array([r["arm_age_ms"] for r in rows])
    dt = np.diff(t)
    print(f"\n=== {name}: {len(rows)} steps, loop period median {np.median(dt)*1000:.0f} ms p95 {np.percentile(dt,95)*1000:.0f} max {dt.max()*1000:.0f}; "
          f"arm age p95 {np.percentile(arm,95):.0f} max {arm.max()}; cam age p95 {np.percentile(cam,95):.0f}")
    print("  grip modes:", {m: modes.count(m) for m in set(modes)}, " mode switches:", sum(1 for a, b in zip(modes, modes[1:]) if a != b))
    d = np.diff(g_sent)
    rev = np.sum(np.sign(d[1:][np.abs(d[1:])>0.5]) != np.sign(d[:-1][np.abs(d[1:])>0.5]))
    print(f"  sent gripper: reversals {rev} ({rev/(len(rows)/15):.2f}/s); measured gripper reversals "
          f"{np.sum(np.diff(np.sign(np.diff(g_meas))[np.abs(np.diff(g_meas))>0.3]) != 0)}")
    # hold stretches
    for m, grp in itertools.groupby(enumerate(modes), key=lambda x: x[1]):
        idx = [i for i, _ in grp]
        if m == "hold" and len(idx) >= 15:
            i0, i1 = idx[0], idx[-1]
            print(f"  hold t={t[i0]:.1f}-{t[i1]:.1f}s: meas {g_meas[i0]:.1f}->{g_meas[i1]:.1f} (min {g_meas[i0:i1+1].min():.1f}), "
                  f"sent {g_sent[i0]:.1f}->{g_sent[i1]:.1f}, plan median {np.nanmedian(g_plan[i0:i1+1]):.1f}, "
                  f"plan>=meas+8 steps {int(np.sum(g_plan[i0:i1+1] >= g_meas[i0:i1+1] + 8))}")
