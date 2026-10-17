"""Diagnose jitter / gripper behaviour in real policy runs: model vs execution."""
import json
import sys
from pathlib import Path

import numpy as np

J = ["joint1", "joint2", "joint3", "joint4", "joint5", "joint6", "gripper"]
P99 = np.array([3.70, 6.51, 6.75, 4.76, 11.39, 4.84, 16.66])
runs = sorted(d for d in Path(r"C:\Users\zhy10\Shared\Piper Arm\deploy\runs").iterdir() if d.is_dir())
sel = [d for d in runs if d.name >= "20260914-184"]
for d in sel:
    rows = [json.loads(l) for l in (d / "steps.jsonl").read_text(encoding="utf-8").splitlines() if l.strip()]
    rows = [r for r in rows if r.get("target") is not None]  # stale-hold and human-correction rows log target: null
    n = len(rows)
    S = np.array([[r["state"][j] for j in J] for r in rows])
    T = np.array([[r["target"][j] for j in J] for r in rows])        # what was sent (clamped, squeezed)
    raw = np.array([r["raw"] if r["raw"] is not None else [np.nan] * 7 for r in rows])  # plan value (post crossfade)
    obs = np.array([r["obs_step"] if r["obs_step"] is not None else -1 for r in rows])
    k = np.arange(n)
    sw = np.r_[False, obs[1:] != obs[:-1]]                            # a new plan took over at this step
    print(f"\n==== {d.name}: {n} steps ({n/15:.0f} s), plan switches {sw.sum()} (every {n/max(1,sw.sum()):.1f} steps), "
          f"plan age at use: median {np.median(k[obs>=0]-obs[obs>=0]):.0f} steps")

    dR = np.abs(np.diff(raw, axis=0))
    at_sw, not_sw = dR[sw[1:]], dR[~sw[1:]]
    print("  per-step change of the MODEL plan (normalized units)      " + " ".join(f"{j[:6]:>7}" for j in J))
    print("    median, normal steps                                      " + " ".join(f"{v:7.2f}" for v in np.nanmedian(not_sw, 0)))
    print("    median, at plan switch                                    " + " ".join(f"{v:7.2f}" for v in np.nanmedian(at_sw, 0)))
    print("    p90,    at plan switch                                    " + " ".join(f"{v:7.2f}" for v in np.nanpercentile(at_sw, 90, 0)))
    print("    demo p99 per step (reference)                             " + " ".join(f"{v:7.2f}" for v in P99))
    # direction reversals of the commanded target per second (jitter), vs of the measured state
    def reversals(X):
        v = np.diff(X, axis=0)
        v[np.abs(v) < 0.3] = 0
        s = np.sign(v)
        nz = [np.sign(col[col != 0]) for col in s.T]
        return np.array([np.sum(c[1:] != c[:-1]) for c in nz]) / (n / 15)
    print("  direction reversals / s: sent target                        " + " ".join(f"{v:7.2f}" for v in reversals(T)))
    print("  direction reversals / s: measured arm                       " + " ".join(f"{v:7.2f}" for v in reversals(S)))
    lag = np.abs(T[:-1] - S[1:])
    print("  |target(k) - measured(k+1)| median (tracking)               " + " ".join(f"{v:7.2f}" for v in np.median(lag, 0)))
    print("  clamp active (sent != plan, joints)  %                      " + " ".join(
        f"{v:7.1f}" for v in 100 * np.nanmean(np.abs(T[:, :6] - raw[:, :6]) > 0.05, 0)) )

    g_raw, g_sent, g_meas = raw[:, 6], T[:, 6], S[:, 6]
    print(f"  gripper plan: min {np.nanmin(g_raw):.1f} max {np.nanmax(g_raw):.1f}; measured: min {g_meas.min():.1f} max {g_meas.max():.1f} (100 = raw 68,000 cap)")
    # open/close cycles of the plan: crossings of 50
    hi = g_raw > 50
    cyc = int(np.sum(hi[1:] != hi[:-1]))
    print(f"  gripper plan crossed 50 (open<->closed) {cyc} times; measured crossed 50 {int(np.sum((g_meas>50)[1:] != (g_meas>50)[:-1]))} times")
    # time from plan starting to open (>50) until measured > 90
    opens = np.where((g_raw[1:] > 50) & (g_raw[:-1] <= 50))[0]
    for o in opens[:4]:
        reach = np.where(g_meas[o:] > 90)[0]
        t90 = reach[0] / 15 if len(reach) else None
        print(f"    open request at t={o/15:5.1f}s: plan {g_raw[o+1]:.0f}->{np.nanmax(g_raw[o:o+15]):.0f} within 1 s; "
              f"measured reached >90 after {t90 if t90 is None else round(t90,1)} s")
    # while (probably) holding: measured between 55 and 90 and plan < measured
    hold = (g_meas > 55) & (g_meas < 90)
    if hold.any():
        print(f"  frames with measured gripper 55-90 (cube-width zone): {hold.sum()} ({hold.sum()/15:.1f} s); "
              f"plan there: median {np.nanmedian(g_raw[hold]):.1f}, p10 {np.nanpercentile(g_raw[hold],10):.1f}, p90 {np.nanpercentile(g_raw[hold],90):.1f}; "
              f"sent median {np.median(g_sent[hold]):.1f}")
    # was the arm ever lifted with gripper partly closed (carrying)?  joint2 high + gripper 55-90
    print(f"  joint2 (shoulder) max {S[:,1].max():.0f}; joint1 range {S[:,0].min():.0f}..{S[:,0].max():.0f}")
