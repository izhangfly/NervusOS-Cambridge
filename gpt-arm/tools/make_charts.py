#!/usr/bin/env python3
"""Static charts of the published results, for the README and results/charts/ (SVG and PNG).

    python tools/make_charts.py [--results results]

Reads results/runs.csv, results/decisions.csv, results/summary.json and results/holds.csv (written by
tools/export_results.py and the buzz analysis). Colours: GPT-6.1 Sol blue #2a78d6, GPT-5.6 Sol orange #eb6834,
moving aqua #1baf7a (checked with a colour-vision validator on white). Needs matplotlib.
"""
import argparse
import csv
import json
from pathlib import Path

import matplotlib

matplotlib.use("Agg")
import matplotlib.pyplot as plt  # noqa: E402

C = {"gpt-6.1-sol": "#2a78d6", "gpt-5.6-sol": "#eb6834"}
NAME = {"gpt-6.1-sol": "GPT-6.1 Sol", "gpt-5.6-sol": "GPT-5.6 Sol"}
INK, INK3, RULE, THINK, MOVE, BAD = "#111111", "#767676", "#dcdcdc", "#c4c4c4", "#1baf7a", "#c8312b"
STAGE = ["0 approach", "1 around", "2 lifted", "3 in basket", "4 out", "5 complete"]
plt.rcParams.update({"font.family": "DejaVu Sans", "font.size": 10, "axes.edgecolor": RULE, "axes.labelcolor": INK3,
                     "xtick.color": INK3, "ytick.color": INK3, "axes.spines.top": False, "axes.spines.right": False,
                     "svg.fonttype": "none"})


def save(fig, out, name):
    out.mkdir(parents=True, exist_ok=True)
    for ext in ("svg", "png"):
        fig.savefig(out / f"{name}.{ext}", dpi=160, bbox_inches="tight", facecolor="white")
    plt.close(fig)


def hhmm(rid):
    return f"{rid[9:11]}:{rid[11:13]}"


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--results", default="results")
    a = ap.parse_args()
    R = Path(a.results)
    out = R / "charts"
    runs = list(csv.DictReader(open(R / "runs.csv")))
    bench = sorted([r for r in runs if r["benchmark"]], key=lambda r: (r["model"] != "gpt-6.1-sol", r["run_id"]))
    decs = list(csv.DictReader(open(R / "decisions.csv")))
    summ = json.load(open(R / "summary.json"))["benchmark"]["models"]

    # 1. stage reached per benchmark run
    fig, ax = plt.subplots(figsize=(8, 3.1))
    for i, r in enumerate(reversed(bench)):
        s = int(r["stage"])
        ax.barh(i, 5, color="#f3f3f3", height=0.56, edgecolor=RULE)
        if s:
            ax.barh(i, s, color=C[r["model"]], height=0.56)
        lab = "complete" if r["success"] == "True" else r["failure_mode"].split("(")[0].strip()
        if s >= 3:
            ax.text(s - 0.08, i, lab, va="center", ha="right", color="white", fontsize=8.5, fontweight="bold")
        else:
            ax.text(s + 0.08, i, lab, va="center", ha="left", color=INK, fontsize=8.5)
    ax.set_yticks(range(len(bench)), [f"{NAME[r['model']]} · {hhmm(r['run_id'])}" for r in reversed(bench)])
    ax.set_xticks(range(6), STAGE, fontsize=8.5)
    ax.set_xlim(0, 5.02)
    ax.set_title("Stage reached in each benchmark run", loc="left", color=INK, fontsize=11)
    save(fig, out, "stages")

    # 2. stage against tokens per run
    fig, ax = plt.subplots(figsize=(8, 3.6))
    for r in bench:
        ax.scatter(int(r["tokens"]) / 1000, int(r["stage"]), s=46, color=C[r["model"]], alpha=0.45, edgecolor=C[r["model"]])
    for m, s in summ.items():
        ax.scatter(s["mean_tokens"] / 1000, s["mean_stage"], s=190, color=C[m], edgecolor="white", linewidth=1.5, zorder=3)
        ax.annotate(f"{NAME[m]}: {s['successes']}/{s['runs']} complete", (s["mean_tokens"] / 1000, s["mean_stage"]),
                    xytext=(10, -14 if m == "gpt-5.6-sol" else 8), textcoords="offset points", color=INK, fontsize=9.5)
    ax.set_yticks(range(6), STAGE, fontsize=8.5)
    ax.set_xlim(0, 900)
    ax.set_ylim(-0.3, 5.5)
    ax.set_xlabel("tokens per run (thousands)")
    ax.grid(axis="y", color="#ececec")
    ax.set_title("Stage reached against tokens per run (small dots runs, large dots means)", loc="left", color=INK, fontsize=11)
    save(fig, out, "outcome")

    # 3. run timelines: thinking and moving per decision
    order = sorted(bench, key=lambda r: r["run_id"])
    fig, ax = plt.subplots(figsize=(10, 3.8))
    for i, r in enumerate(order):
        y = len(order) - 1 - i
        for d in (x for x in decs if x["run_id"] == r["run_id"]):
            end, mv, th = float(d["t_end_s"]), float(d["motion_s"]), float(d["think_s"])
            ax.barh(y, th / 60, left=(end - mv - th) / 60, color=THINK, height=0.42)
            if mv > 0.2:
                ax.barh(y, mv / 60, left=(end - mv) / 60, color=MOVE, height=0.42)
            if d["action"] == "gripper":
                ax.plot(end / 60, y + 0.36, marker="v", color=INK, markersize=4)
            if d["ok"] == "False":
                ax.plot(end / 60, y - 0.36, marker="x", color=BAD, markersize=5)
        ax.text(float(r["duration_s"]) / 60 + 0.4, y, "✓" if r["success"] == "True" else "✗",
                color="#138a52" if r["success"] == "True" else BAD, va="center", fontsize=11)
    ax.set_yticks(range(len(order)), [f"{NAME[r['model']]} · {hhmm(r['run_id'])}" for r in reversed(order)])
    ax.set_xlabel("minutes since the run started")
    ax.set_title("Where the time goes: grey = model thinking, green = arm moving, ▼ gripper, × refused or failed",
                 loc="left", color=INK, fontsize=11)
    save(fig, out, "timeline")

    # 4. seconds per decision
    fig, ax = plt.subplots(figsize=(8, 2.6))
    for i, m in enumerate(["gpt-5.6-sol", "gpt-6.1-sol"]):
        ids = {r["run_id"] for r in bench if r["model"] == m}
        v = sorted(float(d["think_s"]) for d in decs if d["run_id"] in ids and float(d["think_s"]) > 0)
        ax.scatter(v, [i + ((k * 7919) % 21 - 10) / 60 for k in range(len(v))], s=10, color=C[m], alpha=0.55)
        med = v[len(v) // 2]
        ax.plot([med, med], [i - 0.3, i + 0.3], color=INK, linewidth=2)
        ax.text(med + 2, i + 0.3, f"median {med:.1f} s (n={len(v)})", fontsize=8.5, color=INK)
    ax.set_yticks([0, 1], [NAME["gpt-5.6-sol"], NAME["gpt-6.1-sol"]])
    ax.set_xlabel("seconds of model time per decision (benchmark)")
    save(fig, out, "latency")

    # 5. the buzz: worst motor jitter per hold, before and after the driver fix
    hp = R / "holds.csv"
    if hp.exists():
        holds = list(csv.DictReader(open(hp)))
        fig, ax = plt.subplots(figsize=(8, 2.6))
        for i, fixed in enumerate(["0", "1"]):
            w = [max(float(h[f"j{j}_motor_speed_std"] or 0) for j in range(1, 7)) for h in holds if h["fixed"] == fixed]
            ax.scatter([max(x, 0.002) for x in w], [1 - i + ((k * 7919) % 21 - 10) / 60 for k in range(len(w))], s=10,
                       color=[BAD if x > 0.08 else INK3 for x in w], alpha=0.7)
            ax.text(1.05, 1 - i, f"{sum(x > 0.15 for x in w)} of {len(w)} holds > 0.15", va="center", fontsize=8.5, color=INK)
        ax.axvline(0.08, color="#1a6dff", linestyle="--", linewidth=1)
        ax.set_xscale("log")
        ax.set_xlim(0.002, 3)
        ax.set_yticks([1, 0], ["before the fix (run 13:00)", "after (6 benchmark runs)"])
        ax.set_xlabel("worst motor's speed jitter while holding still (rad/s, log); dashed: the driver's nudge trigger")
        save(fig, out, "buzz")
    print("charts ->", out)


if __name__ == "__main__":
    main()
