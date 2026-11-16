#!/usr/bin/env python3
"""Turn a folder of GPT Arm runs into the published results: raw logs, tidy tables and one JSON for the report page.

    python tools/export_results.py ~/gptarm-runs --out results [--site site/data/data.json]
                                   [--annotations results/annotations.json]

Reads every <runs>/<run id>/run.json and <runs>/bench-*/bench.json. Writes, under --out:

    raw/<run id>/run.json     the run log exactly as the runner wrote it (no pictures, no video)
    raw/<bench id>.json       each benchmark's own record
    runs.csv                  one row per run
    decisions.csv             one row per model decision (every call that chose an action)
    summary.json              per-model benchmark figures and the development-run tallies

and, with --site, a compact JSON with the same numbers plus each run's steps for the page's charts and replay.

Annotations (optional JSON: {run id: {"stage": 0-5, "failure_mode": ..., "summary": ..., ...}}) are judgements a
log can't hold: how far a run got on the stage rubric, and why it failed. They are merged into every output and
kept in their own file so the measured numbers and the judged ones stay apart. Nothing here talks to the arm.
"""
import argparse
import ast
import csv
import json
import shutil
import statistics as st
import time
from pathlib import Path

FULL_CYCLE = "pick up the hair gel bottle from the table and put it into the basket. then take it back out"
MOVES = ("move_tool", "move_by", "move_joints")
STAGES = ["No purposeful approach", "Gripper around the bottle, no grasp", "Grasped and lifted off the table",
          "Released inside the basket", "Re-grasped and lifted out of the basket", "Put down on the table (complete)"]


def as_dict(v):
    """A step's state: a dict, or in the first runs its Python repr."""
    if isinstance(v, dict):
        return v
    if isinstance(v, str):
        try:
            r = ast.literal_eval(v)
            return r if isinstance(r, dict) else {}
        except (ValueError, SyntaxError):
            return {}
    return {}


def kind_of(instruction):
    t = " ".join(instruction.lower().split())
    if t.startswith(FULL_CYCLE):
        return "full_cycle"
    if "wave" in t:
        return "wave"
    if t.startswith(("take the hair gel bottle out", "pick the hair gel bottle up out")):
        return "out_of_basket"
    if "stand it upright where it started" in t or "stand it upright on the table where it started" in t:
        return "cycle_upright"
    if "lying on its side" in t:
        return "stand_up"
    return "other"


def load(runs_dir):
    runs, benches = [], []
    for f in sorted(Path(runs_dir).expanduser().glob("*/run.json")):
        try:
            runs.append(json.loads(f.read_text()))
        except ValueError:
            continue
    for f in sorted(Path(runs_dir).expanduser().glob("bench-*/bench.json")):
        try:
            benches.append(json.loads(f.read_text()))
        except ValueError:
            continue
    return runs, benches


def decisions(run):
    """One record per decision, with times relative to the run's start. A step is logged when its motion ends:
    the motion ran over [t - motion_s, t] and the model was thinking for llm_s before that."""
    out = []
    for s in run.get("steps") or []:
        if s.get("kind") != "decision":
            continue
        state = as_dict(s.get("state"))
        saw = state.get("model_saw") if isinstance(state.get("model_saw"), dict) else {}
        tool = state.get("tool_xyz_mm") or saw.get("tool_point_mm")
        end = float(s["t"]) - float(run["started"])
        motion = float(s.get("motion_s") or 0.0)
        think = float(s.get("llm_s") or 0.0)
        detail = s.get("detail") or ""
        out.append({
            "run_id": run["id"], "model": run["model"], "n": s["n"], "action": s.get("skill"), "target": s.get("target") or "",
            "ok": bool(s.get("ok")), "t_end_s": round(end, 1), "think_s": round(think, 2), "motion_s": round(motion, 2),
            "tokens": int(s.get("tokens") or 0),
            "tool_x_mm": tool[0] if tool else None, "tool_y_mm": tool[1] if tool else None, "tool_z_mm": tool[2] if tool else None,
            "holding": bool(state.get("holding")), "gripper_mm": saw.get("gripper_mm"),
            "result": detail[:240], "note": (s.get("note") or "")[:600],
            "images": s.get("images") or {},
        })
    return out


def run_row(run, bench_of, ann):
    d = decisions(run)
    o, u = run.get("outcome") or {}, run.get("usage") or {}
    j = run.get("judge") or {}
    dur = (float(run["ended"]) - float(run["started"])) if run.get("ended") else None
    a = ann.get(run["id"], {})
    b = bench_of.get(run["id"])
    return {
        "run_id": run["id"], "model": run["model"], "mode": run.get("mode"),
        "task": kind_of(run.get("instruction", "")), "benchmark": b["id"] if b else "", "benchmark_index": b["index"] if b else "",
        "started_utc": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime(run["started"])),
        "state": run.get("state"), "success": bool(o.get("success")), "reason": o.get("reason") or "",
        "duration_s": round(dur, 1) if dur else "",
        "decisions": len(d), "moves": sum(1 for x in d if x["action"] in MOVES),
        "gripper_actions": sum(1 for x in d if x["action"] == "gripper"),
        "refused_or_failed": sum(1 for x in d if not x["ok"]),
        "model_calls": u.get("llm_calls", ""), "tokens": u.get("tokens", ""),
        "thinking_s": u.get("llm_s", ""), "moving_s": u.get("motion_s", ""),
        "checker": j.get("status") or "", "checker_confidence": j.get("confidence", ""),
        "lessons_used": run.get("lessons_used", 0),
        "stage": a.get("stage", ""), "failure_mode": a.get("failure_mode") or "", "cause": a.get("root_cause_side") or "",
        "instruction": " ".join(run.get("instruction", "").split()),
    }


def model_summary(rows):
    """Benchmark figures for one model's rows (finished runs only)."""
    done = [r for r in rows if r["state"] in ("done", "failed")]
    n = len(done)
    if not n:
        return {"runs": 0}
    ok = [r for r in done if r["success"]]
    med = lambda k: round(st.median([float(r[k]) for r in done if r[k] != ""]), 1)       # noqa: E731
    mean = lambda k: round(st.mean([float(r[k]) for r in done if r[k] != ""]), 1)       # noqa: E731
    stages = [int(r["stage"]) for r in done if r["stage"] != ""]
    think, move = sum(float(r["thinking_s"] or 0) for r in done), sum(float(r["moving_s"] or 0) for r in done)
    total = sum(float(r["duration_s"] or 0) for r in done)
    fails = {}
    for r in done:
        if not r["success"]:
            k = r["failure_mode"] or r["reason"] or "failed"
            fails[k] = fails.get(k, 0) + 1
    return {
        "runs": n, "successes": len(ok), "rate": round(len(ok) / n, 3),
        "mean_stage": round(st.mean(stages), 2) if stages else None,
        "stage_counts": [sum(1 for s in stages if s == k) for k in range(6)] if stages else None,
        "median_decisions": med("decisions"), "mean_decisions": mean("decisions"),
        "median_minutes": round(med("duration_s") / 60, 1), "mean_minutes": round(mean("duration_s") / 60, 1),
        "mean_tokens": round(mean("tokens")), "median_tokens": round(med("tokens")),
        "tokens_total": sum(int(r["tokens"] or 0) for r in done),
        "thinking_share": round(think / total, 3) if total else None, "moving_share": round(move / total, 3) if total else None,
        "refused_or_failed": sum(int(r["refused_or_failed"]) for r in done), "decisions_total": sum(int(r["decisions"]) for r in done),
        "minutes_per_success": round(total / 60 / len(ok), 1) if ok else None,
        "tokens_per_success": round(sum(int(r["tokens"] or 0) for r in done) / len(ok)) if ok else None,
        "failures": fails,
    }


def write_csv(path, rows, drop=()):
    if not rows:
        return
    cols = [c for c in rows[0] if c not in drop]
    with open(path, "w", newline="") as f:
        w = csv.DictWriter(f, fieldnames=cols, extrasaction="ignore")
        w.writeheader()
        w.writerows(rows)


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("runs")
    ap.add_argument("--out", default="results")
    ap.add_argument("--site")
    ap.add_argument("--annotations")
    args = ap.parse_args()
    out = Path(args.out)
    (out / "raw").mkdir(parents=True, exist_ok=True)
    ann_path = Path(args.annotations) if args.annotations else out / "annotations.json"
    ann = json.loads(ann_path.read_text()) if ann_path.exists() else {}
    runs, benches = load(args.runs)
    bench_of = {}
    for b in benches:
        for i, rid in enumerate(b.get("run_ids") or []):
            bench_of[rid] = {"id": b["id"], "index": i + 1}
        if b.get("run_ids"):                              # a benchmark that never started a run isn't a result
            (out / "raw" / f"{b['id']}.json").write_text(json.dumps(b, indent=1))
    rows, decs = [], []
    for r in runs:
        (out / "raw" / r["id"]).mkdir(exist_ok=True)
        shutil.copyfile(Path(args.runs).expanduser() / r["id"] / "run.json", out / "raw" / r["id"] / "run.json")
        rows.append(run_row(r, bench_of, ann))
        decs += decisions(r)
    write_csv(out / "runs.csv", rows)
    write_csv(out / "decisions.csv", decs, drop=("images", "note"))
    main_bench = max((b for b in benches if b.get("run_ids")), key=lambda b: len(b["run_ids"]), default=None)
    brows = [r for r in rows if main_bench and r["benchmark"] == main_bench["id"]]
    models = list(dict.fromkeys(r["model"] for r in brows))
    think = [d["think_s"] for d in decs if d["think_s"] > 0]
    summary = {
        "generated_utc": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
        "benchmark": {"id": main_bench["id"], "state": main_bench.get("state"), "instruction": main_bench.get("instruction"),
                      "runs_per_model": main_bench.get("runs_per_model"), "lessons": main_bench.get("lessons"),
                      "models": {m: model_summary([r for r in brows if r["model"] == m]) for m in models}} if main_bench else None,
        "all_runs": {"runs": len(rows), "direct": sum(1 for r in rows if r["mode"] == "direct"),
                     "successes": sum(1 for r in rows if r["success"]),
                     "decisions": len(decs), "tokens": sum(int(r["tokens"] or 0) for r in rows),
                     "median_think_s": round(st.median(think), 1) if think else None,
                     "p90_think_s": round(sorted(think)[int(0.9 * len(think))], 1) if think else None,
                     "refused_or_failed": sum(1 for d in decs if not d["ok"])},
        "stages": STAGES,
    }
    (out / "summary.json").write_text(json.dumps(summary, indent=1))
    if args.site:
        site = Path(args.site)
        site.parent.mkdir(parents=True, exist_ok=True)
        by_run = {}
        for d in decs:
            by_run.setdefault(d["run_id"], []).append({k: v for k, v in d.items() if k not in ("run_id", "model")})
        full = {r["id"]: r for r in runs}
        payload = {**summary, "runs": []}
        for r in rows:
            src = full[r["run_id"]]
            j = src.get("judge") or {}
            payload["runs"].append({**r, **{k: v for k, v in ann.get(r["run_id"], {}).items() if k not in r or r[k] == ""},
                                    "checker_says": j.get("explanation") or "", "plan": src.get("plan") or [],
                                    "video_s": (src.get("video") or {}).get("seconds"),
                                    "steps": by_run.get(r["run_id"], [])})
        site.write_text(json.dumps(payload, separators=(",", ":")))
    print(f"{len(rows)} runs, {len(decs)} decisions -> {out}" + (f" and {args.site}" if args.site else ""))
    if main_bench:
        for m, s in summary["benchmark"]["models"].items():
            print(f"  {m}: {s.get('successes')}/{s.get('runs')} complete, mean stage {s.get('mean_stage')}, "
                  f"median {s.get('median_decisions')} decisions, {s.get('median_minutes')} min, {s.get('mean_tokens')} tokens/run")


if __name__ == "__main__":
    main()
