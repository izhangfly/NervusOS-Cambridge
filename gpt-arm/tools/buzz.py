#!/usr/bin/env python3
"""Find motor buzz (high-frequency vibration with no visible motion) in a PiPER CAN log.

Record (read-only; candump only listens, nothing is sent to the arm):

    python tools/buzz.py record /tmp/can.log            # until Ctrl-C / SIGTERM
    python tools/buzz.py analyze /tmp/can.log [--run ~/gptarm-runs/<id>]

What it looks at: each motor's own high-speed feedback (CAN 0x251-0x256, about 200 Hz each: speed in
0.001 rad/s, current in 0.001 A), the joint angles (0x2A5-0x2A7) and the joint commands the driver
streams (0x155-0x157). In each 0.25 s window and for each joint it measures how much the motor speed
shakes around its own mean (speed ripple), how often it changes sign, and how much the current shakes,
next to how far the joint actually moved. A buzz is a window where the motor speed ripples hard and
flips sign many times while the joint goes nowhere.
"""
import argparse
import json
import math
import os
import signal
import subprocess
import sys
from pathlib import Path

import numpy as np

WIN_S = 0.25
IDS_SPEED = range(0x251, 0x257)
IDS_JOINT = (0x2A5, 0x2A6, 0x2A7)
IDS_CMD = (0x155, 0x156, 0x157)
FILTER = ",".join(f"{i:03X}:7FF" for i in [*IDS_SPEED, *IDS_JOINT, *IDS_CMD, 0x151])


def record(path):
    with open(path, "w") as f:
        p = subprocess.Popen(["candump", "-L", f"can0,{FILTER}"], stdout=f)
        def stop(*_):
            p.terminate()
        signal.signal(signal.SIGTERM, stop)
        signal.signal(signal.SIGINT, stop)
        p.wait()


def _s16(b):
    v = int.from_bytes(b, "big")
    return v - 0x10000 if v & 0x8000 else v


def _s32(b):
    v = int.from_bytes(b, "big")
    return v - 0x100000000 if v & 0x80000000 else v


def load(path):
    """-> speed[j]: (t, rad/s, A) arrays; joint: (t, 6 deg); cmd: (t, 6 deg); modes: [(t, hex data)]."""
    spd = {j: [] for j in range(6)}
    jt, jv, cur_j = [], [], [None] * 6
    ct, cv, cur_c = [], [], [None] * 6
    modes = []
    for line in open(path):
        try:
            ts, _, frame = line.split()
            cid, data = frame.split("#")
            t, cid, b = float(ts.strip("()")), int(cid, 16), bytes.fromhex(data)
        except ValueError:
            continue
        if 0x251 <= cid <= 0x256 and len(b) >= 8:
            spd[cid - 0x251].append((t, _s16(b[0:2]) / 1000.0, _s16(b[2:4]) / 1000.0))
        elif cid in IDS_JOINT and len(b) >= 8:
            k = 2 * (cid - 0x2A5)
            cur_j[k], cur_j[k + 1] = _s32(b[0:4]) / 1000.0, _s32(b[4:8]) / 1000.0
            if cid == 0x2A7 and None not in cur_j:
                jt.append(t), jv.append(list(cur_j))
        elif cid in IDS_CMD and len(b) >= 8:
            k = 2 * (cid - 0x155)
            cur_c[k], cur_c[k + 1] = _s32(b[0:4]) / 1000.0, _s32(b[4:8]) / 1000.0
            if cid == 0x157 and None not in cur_c:
                ct.append(t), cv.append(list(cur_c))
        elif cid == 0x151:
            modes.append((t, b.hex()))
    speed = {j: np.array(v, float).reshape(-1, 3) for j, v in spd.items()}
    return speed, (np.array(jt), np.array(jv).reshape(-1, 6)), (np.array(ct), np.array(cv).reshape(-1, 6)), modes


def windows(speed, joints, cmds, win=WIN_S):
    """Per joint and window: speed ripple (rad/s), sign flips per second, current ripple (A), mean
    |current|, joint travel (deg), commanded travel (deg), command rate (Hz)."""
    t0 = min(float(a[0, 0]) for a in speed.values() if len(a))
    t1 = max(float(a[-1, 0]) for a in speed.values() if len(a))
    edges = np.arange(t0, t1, win)
    jt, jv = joints
    ct, cv = cmds
    rows = []
    for a in edges:
        b = a + win
        row = {"t": a}
        ci = (ct >= a) & (ct < b)
        row["cmd_hz"] = float(ci.sum()) / win
        for j in range(6):
            s = speed[j]
            m = (s[:, 0] >= a) & (s[:, 0] < b)
            if m.sum() < 5:
                continue
            w, cur = s[m, 1], s[m, 2]
            sign = np.sign(w[np.abs(w) > 0.02])
            flips = int(np.sum(sign[1:] != sign[:-1])) if len(sign) > 1 else 0
            ji = (jt >= a) & (jt < b)
            travel = float(np.ptp(jv[ji, j])) if ji.sum() > 1 else 0.0
            ctravel = float(np.ptp(cv[ci, j])) if ci.sum() > 1 else 0.0
            row[j] = {"ripple": float(np.std(w - np.mean(w))), "mean_w": float(np.mean(w)),
                      "flips_s": flips / win, "cur_ripple": float(np.std(cur)), "cur": float(np.mean(np.abs(cur))),
                      "travel": travel, "cmd_travel": ctravel, "n": int(m.sum())}
        rows.append(row)
    return rows


def is_buzz(c):
    """Speed ripple well above the noise floor, the sign flipping fast, the joint (nearly) still."""
    return c["ripple"] > 0.15 and c["flips_s"] > 30 and c["travel"] < 0.5


def run_steps(run_dir):
    """[(t_start, label)] from a GPT Arm run's run.json (timestamps are wall-clock seconds)."""
    try:
        d = json.load(open(Path(run_dir).expanduser() / "run.json"))
    except (OSError, ValueError):
        return []
    out = []
    for s in d.get("steps") or []:
        t = s.get("t") or s.get("time") or s.get("started")
        a = s.get("action") or {}
        if isinstance(t, (int, float)):
            out.append((float(t), f"{a.get('type') or a.get('skill') or '?'} {json.dumps(a.get('args') or {k: v for k, v in a.items() if k != 'type'})[:90]}"))
    return out


def analyze(path, run_dir=None, show=40):
    speed, joints, cmds, modes = load(path)
    n = {j: len(v) for j, v in speed.items()}
    span = max(float(v[-1, 0] - v[0, 0]) for v in speed.values() if len(v))
    print(f"{path}: {span:.0f} s; motor feedback frames per joint {n}; "
          f"{len(joints[0])} joint frames, {len(cmds[0])} command frames, {len(modes)} mode frames")
    if len(cmds[0]) > 1:
        print(f"command rate while streaming: median {1.0 / np.median(np.diff(cmds[0])):.0f} Hz")
    rows = windows(speed, joints, cmds)
    floor = {j: np.percentile([r[j]["ripple"] for r in rows if j in r], 20) for j in range(6)}
    print("speed ripple noise floor (20th pct, rad/s):", {f"J{j + 1}": round(v, 3) for j, v in floor.items()})
    steps = run_steps(run_dir) if run_dir else []
    buzz = []
    for r in rows:
        for j in range(6):
            c = r.get(j)
            if c and is_buzz(c):
                buzz.append((r["t"], j, c, r["cmd_hz"]))
    print(f"\n{len(buzz)} buzzing windows of {WIN_S} s (ripple > 0.15 rad/s, > 30 sign flips/s, joint moved < 0.5 deg)")
    by_joint = {}
    for t, j, c, hz in buzz:
        by_joint.setdefault(j, []).append(c)
    for j, cs in sorted(by_joint.items()):
        print(f"  J{j + 1}: {len(cs)} windows ({len(cs) * WIN_S:.1f} s), ripple median {np.median([c['ripple'] for c in cs]):.2f} rad/s, "
              f"flips {np.median([c['flips_s'] for c in cs]):.0f}/s, current ripple {np.median([c['cur_ripple'] for c in cs]):.2f} A, "
              f"mean |current| {np.median([c['cur'] for c in cs]):.2f} A, commanded travel {np.median([c['cmd_travel'] for c in cs]):.2f} deg")
    # Episodes: consecutive buzzing windows, with what the arm was doing.
    eps, last = [], None
    for t, j, c, hz in buzz:
        if eps and t - eps[-1]["t1"] <= WIN_S * 1.5:
            e = eps[-1]
            e["t1"] = t + WIN_S
            e["joints"].add(j)
            e["cmd_hz"].append(hz)
            e["cmd_travel"] = max(e["cmd_travel"], c["cmd_travel"])
        else:
            eps.append({"t0": t, "t1": t + WIN_S, "joints": {j}, "cmd_hz": [hz], "cmd_travel": c["cmd_travel"]})
    print(f"\n{len(eps)} episodes:")
    jt, jv = joints
    for e in eps[:show]:
        k = int(np.searchsorted(jt, e["t0"])) if len(jt) else 0
        q = jv[min(k, len(jv) - 1)] if len(jv) else []
        lab = ""
        for ts, s in steps:
            if ts <= e["t0"]:
                lab = s
        print(f"  {e['t0'] - (jt[0] if len(jt) else 0):7.1f}s +{e['t1'] - e['t0']:.2f}s  J{'/J'.join(str(j + 1) for j in sorted(e['joints']))}"
              f"  cmd {np.mean(e['cmd_hz']):.0f} Hz, commanded travel {e['cmd_travel']:.2f} deg"
              f"  q=[{', '.join(f'{v:.0f}' for v in q)}]  {lab}")
    return rows, buzz, eps


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    sub = ap.add_subparsers(dest="cmd", required=True)
    r = sub.add_parser("record")
    r.add_argument("path")
    a = sub.add_parser("analyze")
    a.add_argument("path")
    a.add_argument("--run")
    a.add_argument("--show", type=int, default=40)
    args = ap.parse_args()
    if args.cmd == "record":
        record(args.path)
    else:
        analyze(args.path, args.run, args.show)


if __name__ == "__main__":
    main()
