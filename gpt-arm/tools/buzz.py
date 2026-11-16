#!/usr/bin/env python3
"""Find motor buzz (high-frequency vibration with no visible motion) in a PiPER CAN log.

Record (read-only; candump only listens, nothing is sent to the arm):

    python tools/buzz.py record /tmp/can.log            # until Ctrl-C / SIGTERM
    python tools/buzz.py analyze /tmp/can.log [--run ~/gptarm-runs/<id>]
    XDG_RUNTIME_DIR=/run/user/1000 pw-record --rate 16000 --channels 1 /tmp/arm.wav   # the Dell's microphone
    python tools/buzz.py audio /tmp/arm.wav --can /tmp/can.log [--run ~/gptarm-runs/<id>]
    python tools/buzz.py holds /tmp/arm.wav --can /tmp/can.log --run ~/gptarm-runs/<id>    # loudest holds first

`holds` is the one that found it (2026-11-16): the arm stands still after each move while the model thinks,
so each hold is a clean sample of one pose. The loud ones had one wrist motor (J5 or J6) with a motor speed
std of 0.2-0.4 rad/s while it didn't move: hunting at its target (see hardware/piper/README.md, "Hunting").

What it looks at: each motor's own high-speed feedback (CAN 0x251-0x256, about 100 Hz each on this arm:
speed in 0.001 rad/s, current in 0.001 A), the joint angles (0x2A5-0x2A7) and the joint commands the
driver streams (0x155-0x157; on the Dell the adapter does not echo the driver's own frames, so these are
missing there). In each 0.25 s window and for each joint it measures how much the motor speed shakes around
its own mean (speed ripple), how often it changes sign, and how much the current shakes, next to how far the
joint actually moved. 100 Hz is too slow to see an audible buzz itself, hence `audio`: the microphone finds
the loud, tonal stretches and the CAN log says what each joint was doing then.
"""
import argparse
import json
import math
import os
import signal
import subprocess
import sys
import time
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


def run_decisions(run_dir):
    """A GPT Arm run's decisions: [{"n", "t_end" (wall-clock s, when its motion ended), "motion_s", "label",
    "state"}]. A step is written when its motion has ended; the arm then holds still while the model
    thinks about the next one."""
    try:
        d = json.load(open(Path(run_dir).expanduser() / "run.json"))
    except (OSError, ValueError):
        return []
    out = []
    for s in d.get("steps") or []:
        if s.get("kind") == "decision" and isinstance(s.get("t"), (int, float)):
            out.append({"n": s.get("n"), "t_end": float(s["t"]), "motion_s": float(s.get("motion_s") or 0.0),
                        "label": f"{s.get('skill')} {s.get('target') or ''}".strip(), "state": s.get("state") or {}})
    return out


def run_steps(run_dir):
    """[(t, label)]: when each decision's motion ended."""
    return [(r["t_end"], f"{r['n']} {r['label']}") for r in run_decisions(run_dir)]


def band_level(path, lo_hz=150.0, hi_hz=400.0, win=WIN_S):
    """[(t, dB)]: sound level in the band where the arm's tones are (150-400 Hz in the 2026-11-16 runs)."""
    t0, rate, x = load_wav(path)
    n = int(win * rate)
    f = np.fft.rfftfreq(n, 1.0 / rate)
    band = (f >= lo_hz) & (f <= hi_hz)
    hann = np.hanning(n)
    return [(t0 + k / rate, float(10 * np.log10((np.abs(np.fft.rfft(x[k:k + n] * hann))[band] ** 2).sum() + 1e-20)))
            for k in range(0, len(x) - n, n)]


def holds(wav, can_log, run_dir):
    """The sound while the arm stood still between moves, per pose: which poses buzz."""
    lv = band_level(wav)
    t = np.array([a for a, _ in lv])
    db = np.array([b for _, b in lv])
    speed, joints, cmds, _ = load(can_log)
    jt, jv = joints
    decs = run_decisions(run_dir)
    rows = []
    for a, b in zip(decs, decs[1:] + [None]):
        t0 = a["t_end"] + 1.0                                  # let it settle
        t1 = (b["t_end"] - b["motion_s"] - 0.5) if b else a["t_end"] + 8.0
        m = (t >= t0) & (t < t1)
        if m.sum() < 4:
            continue
        k = int(np.searchsorted(jt, (t0 + t1) / 2)) if len(jt) else 0
        q = jv[min(k, len(jv) - 1)] if len(jv) else [np.nan] * 6
        ripple = {}
        for j in range(6):
            s = speed[j]
            mm = (s[:, 0] >= t0) & (s[:, 0] < t1)
            ripple[j] = (float(np.std(s[mm, 1])), float(np.std(s[mm, 2]))) if mm.sum() > 5 else (0.0, 0.0)
        st = a["state"] if isinstance(a["state"], dict) else {}
        rows.append({"n": a["n"], "label": a["label"], "secs": t1 - t0, "db": float(np.median(db[m])),
                     "q": [round(float(v)) for v in q], "tool": st.get("tool_xyz_mm"), "holding": st.get("holding"),
                     "ripple": ripple})
    quiet = float(np.percentile([r["db"] for r in rows], 20)) if rows else 0.0
    print(f"band 150-400 Hz while holding still, relative to the quietest fifth of the holds ({len(rows)} holds):")
    for r in sorted(rows, key=lambda r: -r["db"]):
        rp = " ".join(f"J{j + 1} {r['ripple'][j][0]:.3f}/{r['ripple'][j][1]:.2f}" for j in range(6))
        print(f"  {r['db'] - quiet:+5.1f} dB  {r['secs']:4.0f}s  after #{r['n']} {r['label'][:38]:38s} q={r['q']} "
              f"tool={r['tool']} {'holding' if r['holding'] else ''}\n            speed/current std: {rp}")
    return rows


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


def load_wav(path):
    """-> (t0 wall-clock s, rate, mono float samples). pw-record writes the header when it starts and the
    file's mtime when it stops, so the start is the mtime minus the length."""
    import wave
    with wave.open(str(path)) as w:
        rate, n, ch, width = w.getframerate(), w.getnframes(), w.getnchannels(), w.getsampwidth()
        raw = w.readframes(n)
    x = np.frombuffer(raw, {2: np.int16, 4: np.int32}[width]).astype(np.float32)
    x = x.reshape(-1, ch).mean(axis=1) / float(2 ** (8 * width - 1))
    return os.path.getmtime(path) - len(x) / rate, rate, x


def audio_windows(path, win=WIN_S, lo_hz=150.0):
    """Per window: level (dBFS) above lo_hz, the strongest frequency and how far it stands above the
    spectrum's median (dB): a buzz is loud AND tonal (a motor's whine or a PWM tone), unlike speech or
    footsteps."""
    t0, rate, x = load_wav(path)
    n = int(win * rate)
    f = np.fft.rfftfreq(n, 1.0 / rate)
    band = f >= lo_hz
    hann = np.hanning(n)
    rows = []
    for k in range(0, len(x) - n, n):
        s = np.abs(np.fft.rfft(x[k:k + n] * hann)) + 1e-12
        p = s[band] ** 2
        level = 10 * np.log10(p.sum() / n + 1e-20)
        i = int(np.argmax(p))
        tonal = 10 * np.log10(p[i] / (np.median(p) + 1e-20))
        rows.append({"t": t0 + k / rate, "level_db": float(level), "peak_hz": float(f[band][i]), "tonal_db": float(tonal)})
    return rows


def analyze_audio(wav, can_log=None, run_dir=None, loud_db=12.0, tonal_db=25.0, show=40):
    rows = audio_windows(wav)
    floor = float(np.percentile([r["level_db"] for r in rows], 30))
    print(f"{wav}: {len(rows) * WIN_S:.0f} s, background level {floor:.0f} dBFS (30th percentile above 150 Hz)")
    loud = [r for r in rows if r["level_db"] > floor + loud_db and r["tonal_db"] > tonal_db]
    can = None
    if can_log:
        speed, joints, cmds, _ = load(can_log)
        can = {round(r["t"] / WIN_S): r for r in windows(speed, joints, cmds)}
    steps = run_steps(run_dir) if run_dir else []
    eps = []
    for r in loud:
        if eps and r["t"] - eps[-1]["t1"] <= WIN_S * 1.5:
            eps[-1]["t1"] = r["t"] + WIN_S
            eps[-1]["rows"].append(r)
        else:
            eps.append({"t0": r["t"], "t1": r["t"] + WIN_S, "rows": [r]})
    print(f"{len(loud)} loud tonal windows (> {loud_db:.0f} dB over background, peak > {tonal_db:.0f} dB over the "
          f"spectrum's median) in {len(eps)} episodes:")
    for e in eps[:show]:
        hz = np.median([r["peak_hz"] for r in e["rows"]])
        lv = max(r["level_db"] for r in e["rows"]) - floor
        line = f"  {time.strftime('%H:%M:%S', time.localtime(e['t0']))} +{e['t1'] - e['t0']:4.2f}s  {hz:6.0f} Hz  +{lv:.0f} dB"
        if can:
            c = can.get(round(e["t0"] / WIN_S)) or can.get(round(e["t0"] / WIN_S) + 1) or {}
            moving = [f"J{j + 1} {c[j]['travel']:.1f}deg" for j in range(6) if j in c and c[j]["travel"] > 0.3]
            shaky = [f"J{j + 1}" for j in range(6) if j in c and c[j]["flips_s"] >= 8]
            cur = [f"J{j + 1} {c[j]['cur']:.1f}A" for j in range(6) if j in c]
            line += f"  moving: {', '.join(moving) or 'none'}; speed sign flipping: {', '.join(shaky) or 'none'}; |I| {' '.join(cur)}"
        lab = ""
        for ts, s in steps:
            if ts <= e["t0"]:
                lab = s
        print(line + (f"  [{lab}]" if lab else ""))
    return eps


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    sub = ap.add_subparsers(dest="cmd", required=True)
    r = sub.add_parser("record")
    r.add_argument("path")
    a = sub.add_parser("analyze")
    a.add_argument("path")
    a.add_argument("--run")
    a.add_argument("--show", type=int, default=40)
    s = sub.add_parser("audio", help="loud tonal stretches in a microphone recording (pw-record), with the CAN log")
    s.add_argument("wav")
    s.add_argument("--can")
    s.add_argument("--run")
    s.add_argument("--show", type=int, default=40)
    h = sub.add_parser("holds", help="the sound while the arm stood still after each move of a run, loudest first")
    h.add_argument("wav")
    h.add_argument("--can", required=True)
    h.add_argument("--run", required=True)
    args = ap.parse_args()
    if args.cmd == "record":
        record(args.path)
    elif args.cmd == "holds":
        holds(args.wav, args.can, args.run)
    elif args.cmd == "audio":
        analyze_audio(args.wav, args.can, args.run, show=args.show)
    else:
        analyze(args.path, args.run, args.show)


if __name__ == "__main__":
    main()
