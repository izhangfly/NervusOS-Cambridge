#!/usr/bin/env python3
"""Turn GPT Arm run records into a benchmark report: REPORT.md, summary.json and figures.

    python3 gpt-arm/tools/make_report.py                        # newest bench under ~/gptarm-runs
    python3 gpt-arm/tools/make_report.py --bench bench-20261116-101500 --out /tmp/report
    python3 gpt-arm/tools/make_report.py --bench all            # every run folder, no bench file

It reads only the files described in gpt-arm/API.md ("Files on disk"): `<runs>/<id>/run.json` and
`<runs>/bench-<stamp>/bench.json`. It needs the Python standard library; with matplotlib it also draws
figures, and with Pillow it builds contact sheets of step images. Without them it writes the tables only.

What counts:
  * a run is one cycle: the bottle into the basket and back out, upright, at its start spot. It is a success
    only when `outcome.success` is true; a run that ended without a verdict (stopped, failed) is a failure.
    Runs still `running` and runs named with --exclude are listed but not counted.
  * every table is grouped by model AND mode. `mode` is `skills` (the model picks a named skill; the default
    when a run has no `mode`) or `direct` (the model outputs joint targets or tool moves itself).
  * steps per run = decision steps (`kind == "decision"`), refused ones included.
  * a refusal is a decision with `ok == false` whose `detail` (or `note`) says the governor refused it
    (words like "refused", "precondition", "governor", "not allowed"); other `ok == false` decisions are
    motions that failed. The backend should start a refusal's detail with "refused:" to keep this exact.
  * recoveries inside a cycle: a retry is a later decision that picks the same skill as an earlier failed
    one (a grasp tried again after it missed); a knock-over is a step where `bottle.upright` goes from true to
    false. A clean success had no refusal, no failed motion and no knock-over; any other success "needed a
    recovery".
  * wall time = `ended - started`; LLM time, motion time and tokens come from `usage`, or from the sum
    over steps when `usage` lacks them.
  * success rates carry Wilson 95% score intervals; pairs of groups are compared with Fisher's exact test.
"""
from __future__ import annotations

import argparse
import json
import math
import re
import statistics
import sys
import time
from pathlib import Path

try:  # figures are optional
    import matplotlib

    matplotlib.use("Agg")
    import matplotlib.pyplot as plt

    HAVE_MPL = True
except Exception:  # ImportError, or a broken install (matplotlib without Pillow raises ImportError too)
    HAVE_MPL = False

try:  # contact sheets are optional
    from PIL import Image, ImageDraw

    HAVE_PIL = True
except Exception:
    HAVE_PIL = False

Z95 = 1.959963984540054
DEFAULT_RUNS = Path.home() / "gptarm-runs"

MODEL_LABELS = {"gpt-6-astra": "GPT-6 Astra", "gpt-6.1-sol": "GPT-6.1 Sol", "gpt-5.6-sol": "GPT-5.6 Sol"}
# Categorical slots 1-3 of the default validated palette: a model keeps its colour in every figure.
MODEL_COLOURS = ["#2a78d6", "#eb6834", "#1baf7a", "#eda100", "#e87ba4", "#008300", "#6250d6", "#e34948"]
LLM_COLOUR, MOTION_COLOUR, REFUSED_COLOUR = "#2a78d6", "#eb6834", "#d03b3b"

# Failure categories, in the order a free-text reason is tested against them (first match wins).
FAILURE_CATEGORIES = [
    ("operator_stop", "Stopped by the operator or the emergency stop",
     r"\b(stopped by (the )?(operator|user|owner)|e-?stop|emergency stop|stop (button|pressed)|operator stop)"),
    ("llm_error", "The model call failed or its answer could not be used",
     r"\b(llm|api|codex|agentrouter|rate.?limit|http \d{3}|invalid json|could not parse|unparse|no answer|"
     r"model (error|timeout|failed))"),
    ("step_limit", "Ran out of decisions",
     r"(step (limit|budget)|out of steps|steps? (used up|exhausted)|max(imum)? (number of )?steps|no steps left|"
     r"decision (limit|budget))"),
    ("refusal_loop", "Kept choosing skills the governor refused",
     r"(refus\w* (\d+ times|repeatedly|again)|too many refus|refusal loop)"),
    ("perception", "Lost or misplaced the bottle or the basket in the camera images",
     r"(not seen|cannot see|can't see|lost sight|not found|no detection|misdetect|calibrat|perception)"),
    ("missed_grasp", "Closed the gripper on nothing",
     r"(missed|closed on nothing|held nothing|empty (gripper|grasp)|nothing (between|in) the (fingers|gripper)|"
     r"grasp (failed|missed))"),
    ("dropped", "Dropped the bottle while carrying it",
     r"(dropped|slipped|fell out|lost (its |the )?grip)"),
    ("not_upright", "The bottle ended not standing upright",
     r"(not upright|upright\W+(false|no)|tipped|toppled|knocked over|lying (down|on its side)|fell over)"),
    ("not_returned", "The bottle was not put back where it started",
     r"(start(ing)? spot|where it started|not returned|returned_mm|too far from)"),
    ("not_in_basket", "The bottle never went into the basket",
     r"(not (in|into) the basket|never entered|missed the basket|outside the basket|entered_basket\W+false)"),
    ("motion_error", "A move failed: unreachable, a joint limit, a driver guard or a collision",
     r"(unreachable|cannot reach|can't reach|\bik\b|joint limit|motion (failed|stopped)|driver|collision|guard|"
     r"deviation|workspace)"),
]
CATEGORY_LABELS = {c: label for c, label, _ in FAILURE_CATEGORIES}
CATEGORY_LABELS.update({"other": "Other or unexplained", "error": "The run ended with an error and no reason"})
_CATEGORY_RES = [(c, re.compile(rx, re.I)) for c, _, rx in FAILURE_CATEGORIES]
_CODE_RE = re.compile(r"^[a-z][a-z0-9_]*$")
REFUSAL_RE = re.compile(r"\b(refus\w*|precondition\w*|governor|not allowed|disallowed|rejected)\b", re.I)

STAGES = ["no grasp", "grasped", "in basket", "out again", "complete"]


# --- statistics -----------------------------------------------------------------------------------------
def wilson(k: int, n: int, z: float = Z95) -> tuple:
    """Wilson score interval for k successes in n trials. (nan, nan) when n is 0."""
    if n <= 0:
        return (float("nan"), float("nan"))
    p = k / n
    denom = 1.0 + z * z / n
    centre = (p + z * z / (2 * n)) / denom
    half = z * math.sqrt(p * (1 - p) / n + z * z / (4 * n * n)) / denom
    return (max(0.0, centre - half), min(1.0, centre + half))


def fisher_exact(k1: int, n1: int, k2: int, n2: int) -> float:
    """Two-sided Fisher exact p-value for the 2x2 table [[k1, n1-k1], [k2, n2-k2]]."""
    total, succ = n1 + n2, k1 + k2
    if n1 == 0 or n2 == 0:
        return float("nan")
    lo, hi = max(0, succ - n2), min(succ, n1)

    def prob(a: int) -> float:
        return math.comb(n1, a) * math.comb(n2, succ - a) / math.comb(total, succ)

    p_obs = prob(k1)
    return min(1.0, sum(prob(a) for a in range(lo, hi + 1) if prob(a) <= p_obs * (1 + 1e-9)))


def describe(values: list) -> dict:
    vals = [float(v) for v in values if v is not None and not (isinstance(v, float) and math.isnan(v))]
    if not vals:
        return {"n": 0, "median": None, "min": None, "max": None}
    return {"n": len(vals), "median": statistics.median(vals), "min": min(vals), "max": max(vals)}


# --- formatting -----------------------------------------------------------------------------------------
def num(x, digits: int = 1) -> str:
    if x is None or (isinstance(x, float) and math.isnan(x)):
        return "n/a"
    if float(x).is_integer() and digits <= 1:
        return str(int(x))
    return f"{x:.{digits}f}"


def tokens(x) -> str:
    if x is None:
        return "n/a"
    return f"{x / 1000:.1f}k" if x >= 1000 else str(int(x))


def pct(x) -> str:
    return "n/a" if x is None or (isinstance(x, float) and math.isnan(x)) else f"{100 * x:.0f}%"


def med_range(d: dict, fmt=num) -> str:
    if not d["n"]:
        return "n/a"
    return f"{fmt(d['median'])} [{fmt(d['min'])}, {fmt(d['max'])}]"


MODES = ("skills", "direct")


def label(model: str) -> str:
    return MODEL_LABELS.get(model, model)


def group_of(model: str, mode: str) -> str:
    return f"{model}|{mode}"


def glabel(group: str) -> str:
    """'GPT-6 Astra (skills)' for the group 'gpt-6-astra|skills'."""
    model, _, mode = group.partition("|")
    return f"{label(model)} ({mode})" if mode else label(model)


def gslug(group: str) -> str:
    return re.sub(r"[^A-Za-z0-9.]+", "-", group)


def md_cell(text) -> str:
    return str(text).replace("|", "\\|").replace("\n", " ")


def table(header: list, rows: list) -> str:
    out = ["| " + " | ".join(header) + " |", "|" + "|".join("---" for _ in header) + "|"]
    out += ["| " + " | ".join(md_cell(c) for c in r) + " |" for r in rows]
    return "\n".join(out)


# --- loading --------------------------------------------------------------------------------------------
def find_bench(runs_dir: Path, bench: str | None):
    """The bench.json to report on, or None for every run folder."""
    if bench == "all":
        return None
    if bench:
        for name in (bench, "bench-" + bench):
            p = runs_dir / name / "bench.json"
            if p.exists():
                return p
        raise SystemExit(f"no bench.json for {bench!r} under {runs_dir}")
    found = sorted(runs_dir.glob("bench-*/bench.json"), key=lambda p: (p.parent.name, p.stat().st_mtime))
    return found[-1] if found else None


def load_json(p: Path):
    with open(p, encoding="utf-8") as f:
        return json.load(f)


TASK = "Put the hair gel bottle into the basket. Then take it out again and stand it upright on the table where it started."


def is_cycle(run: dict) -> bool:
    """A run of the bottle cycle (the benchmark's task), not a free instruction typed in the portal."""
    if "checked" in run:
        return bool(run["checked"])
    return " ".join((run.get("instruction") or "").split()).lower() == " ".join(TASK.split()).lower()


def load_runs(runs_dir: Path, bench_path):
    """(bench or None, [run dicts], [missing ids]). Each run gets `_dir`."""
    bench, missing, runs = None, [], []
    if bench_path is not None:
        bench = load_json(bench_path)
        ids = list(dict.fromkeys(bench.get("run_ids") or []))
    else:
        ids = sorted(p.parent.name for p in runs_dir.glob("*/run.json") if not p.parent.name.startswith("bench-"))
    for rid in ids:
        p = runs_dir / rid / "run.json"
        if not p.exists():
            missing.append(rid)
            continue
        r = load_json(p)
        r["_dir"] = str(p.parent)
        r.setdefault("id", rid)
        runs.append(r)
    runs.sort(key=lambda r: (r.get("started") or 0, r.get("id")))
    return bench, runs, missing


# --- one run --------------------------------------------------------------------------------------------
def is_refusal(step: dict) -> bool:
    return step.get("ok") is False and bool(REFUSAL_RE.search(f"{step.get('detail') or ''} {step.get('note') or ''}"))


def categorise_text(text: str):
    for code, rx in _CATEGORY_RES:
        if rx.search(text):
            return code
    return None


def classify(run: dict) -> str:
    """'success' or a failure category: the reason's code, its keywords, the checks, the last steps, the state."""
    outcome = run.get("outcome") or {}
    if outcome.get("success") is True:
        return "success"
    reason = (outcome.get("reason") or "").strip()
    if reason and _CODE_RE.match(reason):
        return reason
    if reason:
        c = categorise_text(reason)
        if c:
            return c
    checks = outcome.get("checks") or {}
    if checks.get("entered_basket") is False:
        return "not_in_basket"
    if checks.get("entered_basket") and checks.get("upright") is False:
        return "not_upright"
    if run.get("state") == "stopped":
        return "operator_stop"
    tail = [s for s in (run.get("steps") or []) if s.get("kind") in ("error", "verdict", "info", "decision")][-3:]
    for s in reversed(tail):
        if s.get("kind") == "decision" and s.get("ok") is not False:
            continue
        c = categorise_text(f"{s.get('note') or ''} {s.get('detail') or ''}")
        if c:
            return c
    if checks.get("entered_basket") and checks.get("upright") is True:
        return "not_returned"
    if run.get("state") == "failed":
        return "error"
    return "other"


def furthest_stage(run: dict) -> int:
    """0 no grasp, 1 grasped, 2 in the basket, 3 taken out again, 4 complete (the verdict's success)."""
    if (run.get("outcome") or {}).get("success") is True:
        return 4
    stage = 0
    if (run.get("outcome") or {}).get("checks", {}).get("entered_basket"):
        stage = 2
    for s in run.get("steps") or []:
        st = s.get("state") or {}
        where = (st.get("bottle") or {}).get("where")
        if stage < 1 and (st.get("holding") or where == "gripper"):
            stage = 1
        if stage < 2 and where == "basket":
            stage = 2
        if stage >= 2 and (st.get("holding") or where == "gripper") and where != "basket":
            stage = max(stage, 3)
    return stage


def recoveries(steps: list) -> dict:
    """Failed decisions split by kind, retries after a failure, and knock-overs, from one run's steps."""
    decisions = [s for s in steps if s.get("kind") == "decision"]
    refused = [s for s in decisions if is_refusal(s)]
    failed = [s for s in decisions if s.get("ok") is False and not is_refusal(s)]
    retries = 0
    for i, s in enumerate(decisions):
        if s.get("ok") is False and any(later.get("skill") == s.get("skill") for later in decisions[i + 1:]):
            retries += 1
    knocked, upright_before = 0, None
    for s in steps:
        up = ((s.get("state") or {}).get("bottle") or {}).get("upright")
        if up is None:
            continue
        if upright_before is True and up is False:
            knocked += 1
        upright_before = up
    return {"refused": len(refused), "failed_motions": len(failed), "retries": retries, "knock_overs": knocked}


def summarise_run(run: dict) -> dict:
    steps = run.get("steps") or []
    decisions = [s for s in steps if s.get("kind") == "decision"]
    usage = run.get("usage") or {}
    rec = recoveries(steps)
    mode = run.get("mode") or "skills"

    def from_usage(key: str):
        v = usage.get(key)
        if v is not None:
            return float(v)
        vals = [s.get(key) for s in steps if s.get(key) is not None]
        return float(sum(vals)) if vals else None

    started, ended = run.get("started"), run.get("ended")
    if started is not None and ended is None and steps and steps[-1].get("t") is not None:
        ended = steps[-1]["t"]
    outcome = run.get("outcome") or {}
    model = run.get("model") or "unknown"
    success = outcome.get("success") is True
    incidents = rec["refused"] + rec["failed_motions"] + rec["knock_overs"]
    return {
        "id": run.get("id"), "model": model, "mode": mode, "group": group_of(model, mode),
        "state": run.get("state"),
        "started": started, "success": success, "verdict": outcome.get("success"),
        "clean": success and incidents == 0, "recovered": success and incidents > 0,
        "reason": outcome.get("reason") or "", "category": classify(run), "stage": furthest_stage(run),
        "decisions": len(decisions), "refused": rec["refused"],
        "failed_decisions": rec["failed_motions"], "retries": rec["retries"], "knock_overs": rec["knock_overs"],
        "wall_s": None if started is None or ended is None else float(ended) - float(started),
        "llm_s": from_usage("llm_s"), "motion_s": from_usage("motion_s"), "tokens": from_usage("tokens"),
        "llm_calls": usage.get("llm_calls"),
    }


# --- per model ------------------------------------------------------------------------------------------
def model_order(bench, rows: list) -> list:
    order = []
    for r in (bench or {}).get("results") or []:
        if r.get("model") and r["model"] not in order:
            order.append(r["model"])
    for r in rows:
        if r["model"] not in order:
            order.append(r["model"])
    return order


def group_order(models: list, rows: list) -> list:
    """Model by model, skills before direct, then any other mode; only groups that have runs."""
    present = {r["group"] for r in rows}
    out = []
    for m in models:
        modes = list(MODES) + sorted({r["mode"] for r in rows if r["model"] == m} - set(MODES))
        out += [group_of(m, md) for md in modes if group_of(m, md) in present]
    return out


def aggregate(rows: list, groups: list) -> dict:
    out = {}
    for g in groups:
        rs = [r for r in rows if r["group"] == g]
        k, n = sum(r["success"] for r in rs), len(rs)
        lo, hi = wilson(k, n)
        dec, ref = sum(r["decisions"] for r in rs), sum(r["refused"] for r in rs)
        failures = {}
        for r in rs:
            if not r["success"]:
                failures[r["category"]] = failures.get(r["category"], 0) + 1
        clean = sum(r["clean"] for r in rs)
        clo, chi = wilson(clean, n)
        out[g] = {
            "model": g.partition("|")[0], "mode": g.partition("|")[2],
            "runs": n, "successes": k, "rate": k / n if n else None, "wilson95": [lo, hi],
            "clean_successes": clean, "recovered_successes": sum(r["recovered"] for r in rs),
            "clean_rate": clean / n if n else None, "clean_wilson95": [clo, chi],
            "retries_total": sum(r["retries"] for r in rs), "knock_overs_total": sum(r["knock_overs"] for r in rs),
            "failed_motions": describe([r["failed_decisions"] for r in rs]),
            "refused_per_run": describe([r["refused"] for r in rs]),
            "retries": describe([r["retries"] for r in rs]),
            "decisions": describe([r["decisions"] for r in rs]), "wall_s": describe([r["wall_s"] for r in rs]),
            "llm_s": describe([r["llm_s"] for r in rs]), "motion_s": describe([r["motion_s"] for r in rs]),
            "tokens": describe([r["tokens"] for r in rs]),
            "decisions_total": dec, "refused_total": ref, "refusal_rate": ref / dec if dec else None,
            "failed_decisions_total": sum(r["failed_decisions"] for r in rs),
            "runs_with_refusal": sum(1 for r in rs if r["refused"]),
            "stages": [sum(1 for r in rs if r["stage"] == i) for i in range(len(STAGES))],
            "failures": dict(sorted(failures.items(), key=lambda kv: (-kv[1], kv[0]))),
        }
    return out


def typical(rows: list, success: bool = True):
    """The run nearest the median number of decisions among those given (earliest on a tie)."""
    rs = [r for r in rows if r["success"] == success]
    if not rs:
        return None
    med = statistics.median(r["decisions"] for r in rs)
    return min(rs, key=lambda r: (abs(r["decisions"] - med), r["started"] or 0))


# --- step sequences -------------------------------------------------------------------------------------
def step_word(s: dict) -> str:
    w = s.get("skill") or "?"
    if s.get("target"):
        w += f"({s['target']})"
    if s.get("ok") is False:
        w = ("REFUSED " if is_refusal(s) else "FAILED ") + w
    return w


def sequence_text(run: dict) -> str:
    steps = run.get("steps") or []
    started = run.get("started") or (steps[0].get("t") if steps else 0) or 0
    words = [step_word(s) for s in steps if s.get("kind") == "decision"]
    verdict = next((s for s in reversed(steps) if s.get("kind") == "verdict"), None)
    lines = [" -> ".join(words) + (f" => {'SUCCESS' if verdict.get('ok') else 'FAIL'}" if verdict else ""), ""]
    lines.append(f"{'n':>3} {'t(s)':>7}  {'kind':<9} {'skill':<26} {'llm_s':>6} {'mot_s':>6}  note")
    for s in steps:
        if s.get("kind") not in ("decision", "verdict", "error"):
            continue
        t = "" if s.get("t") is None else f"{s['t'] - started:7.1f}"
        skill = step_word(s) if s.get("kind") == "decision" else ""
        note = (s.get("note") or "")
        if s.get("ok") is False and s.get("detail"):
            note += f" [{s['detail']}]"
        lines.append(f"{s.get('n', ''):>3} {t:>7}  {s.get('kind', ''):<9} {skill[:26]:<26} "
                     f"{num(s.get('llm_s')):>6} {num(s.get('motion_s')):>6}  {note[:110]}")
    return "\n".join(lines)


# --- figures --------------------------------------------------------------------------------------------
def _style_axes(ax) -> None:
    for side in ("top", "right"):
        ax.spines[side].set_visible(False)
    ax.spines["left"].set_color("#8a8984")
    ax.spines["bottom"].set_color("#8a8984")
    ax.tick_params(colors="#52514e", labelsize=8)
    ax.grid(axis="x", color="#e6e5e0", linewidth=0.6)
    ax.set_axisbelow(True)


def fig_success(agg: dict, models: list, colours: dict, path: Path) -> None:
    fig, ax = plt.subplots(figsize=(6.4, 0.6 + 0.55 * len(models)))
    ys = list(range(len(models)))[::-1]
    for y, m in zip(ys, models):
        a = agg[m]
        if not a["runs"]:
            continue
        lo, hi = a["wilson95"]
        ax.plot([100 * lo, 100 * hi], [y, y], color=colours[m], linewidth=2, solid_capstyle="round",
                linestyle="--" if m.endswith("|direct") else "-")
        ax.plot([100 * a["rate"]], [y], "o", markersize=8, color=colours[m], markeredgecolor="white", markeredgewidth=2)
        ax.text(100 * hi + 2, y, f"{a['successes']}/{a['runs']}", va="center", fontsize=8, color="#0b0b0b")
    ax.set_yticks(ys)
    ax.set_yticklabels([glabel(m) for m in models], fontsize=9)
    ax.set_xlim(-3, 112)
    ax.set_ylim(-0.6, len(models) - 0.4)
    ax.set_xlabel("success rate (%), dot = observed, line = Wilson 95% interval", fontsize=8, color="#52514e")
    _style_axes(ax)
    fig.tight_layout()
    fig.savefig(path, dpi=160)
    plt.close(fig)


def fig_effort(rows: list, models: list, colours: dict, path: Path) -> None:
    metrics = [("decisions", "decisions"), ("wall_s", "wall time (s)"), ("llm_s", "LLM time (s)"),
               ("motion_s", "motion time (s)"), ("tokens", "tokens (k)")]
    fig, axes = plt.subplots(1, len(metrics), figsize=(11, 0.9 + 0.5 * len(models)), sharey=True)
    ys = {m: i for i, m in enumerate(models[::-1])}
    for ax, (key, title) in zip(axes, metrics):
        for m in models:
            vals = [r[key] for r in rows if r["group"] == m and r[key] is not None]
            if key == "tokens":
                vals = [v / 1000 for v in vals]
            for r, v in zip([r for r in rows if r["group"] == m and r[key] is not None], vals):
                ax.plot(v, ys[m] + (0.12 if r["success"] else -0.12), "o" if r["success"] else "x", markersize=5,
                        color=colours[m], alpha=0.85, markeredgewidth=1.2)
            if vals:
                med = statistics.median(vals)
                ax.plot([med, med], [ys[m] - 0.3, ys[m] + 0.3], color="#0b0b0b", linewidth=1.5)
        ax.set_title(title, fontsize=9, color="#0b0b0b")
        _style_axes(ax)
    axes[0].set_yticks(list(ys.values()))
    axes[0].set_yticklabels([glabel(m) for m in ys], fontsize=9)
    axes[0].set_ylim(-0.6, len(models) - 0.4)
    fig.text(0.01, 0.01, "o success, x failure, bar = median", fontsize=7, color="#52514e")
    fig.tight_layout(rect=(0, 0.04, 1, 1))
    fig.savefig(path, dpi=160)
    plt.close(fig)


def fig_timelines(picks: list, path: Path) -> None:
    """One row per run: LLM time before each decision, motion after it, refusals marked. Skill names are
    written under the bars when a run has few enough decisions to read them; failed ones are always named."""
    gap = 2.0  # vertical space per run: title above the bar, skill names below it
    fig, ax = plt.subplots(figsize=(11, 0.9 + 0.95 * len(picks)))
    for row, (title, run) in enumerate(picks[::-1]):
        y = row * gap
        steps = run.get("steps") or []
        t0 = run.get("started") or (steps[0].get("t") if steps else 0) or 0
        n_dec = sum(1 for s in steps if s.get("kind") == "decision")
        for s in steps:
            if s.get("t") is None:
                continue
            t = s["t"] - t0
            if s.get("kind") == "decision":
                llm, mot = s.get("llm_s") or 0, s.get("motion_s") or 0
                if llm:
                    ax.barh(y, llm, left=t - llm, height=0.5, color=LLM_COLOUR, edgecolor="white", linewidth=1)
                if mot:
                    ax.barh(y, mot, left=t, height=0.5, color=MOTION_COLOUR, edgecolor="white", linewidth=1)
                if s.get("ok") is False:
                    ax.plot(t, y + 0.42, "v", color=REFUSED_COLOUR, markersize=6)
                if n_dec <= 16 or s.get("ok") is False:
                    ax.text(t, y - 0.35, (s.get("skill") or "?")[:14], fontsize=5.5, rotation=45, ha="right",
                            va="top", color=REFUSED_COLOUR if s.get("ok") is False else "#52514e")
            elif s.get("kind") == "verdict":
                ax.plot(t, y, "D", color="#0b0b0b" if s.get("ok") else REFUSED_COLOUR, markersize=6)
        ax.text(0, y + 0.5, title, fontsize=7.5, color="#0b0b0b", va="bottom")
    ax.set_yticks([])
    ax.set_ylim(-1.4, (len(picks) - 1) * gap + 1.1)
    ax.set_xlabel("seconds since the run started (blue = LLM, orange = motion, red triangle = refused or failed, "
                  "diamond = verdict)", fontsize=8, color="#52514e")
    _style_axes(ax)
    ax.spines["left"].set_visible(False)
    fig.tight_layout()
    fig.savefig(path, dpi=160)
    plt.close(fig)


def contact_sheet(run: dict, path: Path, cols: int = 6, width: int = 300) -> bool:
    """Every step image of one run in a grid, overhead above wrist, labelled with the step. False if none."""
    cells = []
    for s in run.get("steps") or []:
        imgs = s.get("images") or {}
        files = [Path(run["_dir"]) / imgs[k] for k in ("overhead", "wrist") if imgs.get(k)]
        files = [f for f in files if f.exists()]
        if files:
            cells.append((f"{s.get('n', '')} {s.get('skill') or s.get('kind') or ''}"[:40], files))
    if not cells:
        return False
    cells = cells[:30]
    tiles = []
    for text, files in cells:
        parts = []
        for f in files:
            try:
                im = Image.open(f).convert("RGB")
            except Exception:
                continue
            im = im.resize((width, max(1, int(im.height * width / im.width))))
            parts.append(im)
        if not parts:
            continue
        h = 18 + sum(p.height for p in parts)
        tile = Image.new("RGB", (width, h), "white")
        ImageDraw.Draw(tile).text((4, 3), text, fill=(11, 11, 11))
        y = 18
        for p in parts:
            tile.paste(p, (0, y))
            y += p.height
        tiles.append(tile)
    if not tiles:
        return False
    th = max(t.height for t in tiles)
    rows = math.ceil(len(tiles) / cols)
    sheet = Image.new("RGB", (cols * (width + 4) + 4, rows * (th + 4) + 4), (230, 229, 224))
    for i, t in enumerate(tiles):
        sheet.paste(t, (4 + (i % cols) * (width + 4), 4 + (i // cols) * (th + 4)))
    sheet.save(path, quality=85)
    return True


# --- the report -----------------------------------------------------------------------------------------
def build(runs_dir: Path, bench_arg, out_dir, exclude: list, figures: bool = True) -> dict:
    bench_path = find_bench(runs_dir, bench_arg)
    bench, runs, missing = load_runs(runs_dir, bench_path)
    bench_id = bench_path.parent.name if bench_path else "all-runs"
    out = Path(out_dir) if out_dir else (runs_dir / bench_id / "report" if bench_path else runs_dir / "report-all")
    out.mkdir(parents=True, exist_ok=True)
    figdir = out / "figures"

    counted, skipped = [], []
    for r in runs:
        if r.get("id") in exclude:
            skipped.append((r, "excluded on the command line"))
        elif bench is None and not is_cycle(r):          # with a bench, its instruction is the task
            skipped.append((r, "a free instruction, not the bottle cycle"))
        elif r.get("state") == "running":
            skipped.append((r, "still running"))
        else:
            counted.append(r)
    rows = [summarise_run(r) for r in counted]
    by_id = {r.get("id"): r for r in counted}
    models = model_order(bench, rows)
    groups = group_order(models, rows)
    colours = {g: MODEL_COLOURS[models.index(g.partition("|")[0]) % len(MODEL_COLOURS)] for g in groups}
    agg = aggregate(rows, groups)
    modes_present = sorted({r["mode"] for r in rows}, key=lambda m: (MODES.index(m) if m in MODES else 9, m))

    draw = figures and HAVE_MPL
    sheets = figures and HAVE_PIL
    if draw or sheets:
        figdir.mkdir(exist_ok=True)

    L = []  # the Markdown, line by line
    L.append("# GPT Arm benchmark report\n")
    when = time.strftime("%Y-%m-%d %H:%M", time.localtime())
    src = f"bench `{bench_id}`" if bench_path else "every run folder (no bench file)"
    L.append(f"Generated {when} by `gpt-arm/tools/make_report.py` from {src} in `{runs_dir}`.\n")
    if bench:
        L.append(f"- Instruction: \"{bench.get('instruction', '')}\"")
        L.append(f"- Runs per model planned: {bench.get('runs_per_model', 'n/a')}; bench state: "
                 f"{bench.get('state', 'n/a')}")
        if bench.get("started"):
            L.append(f"- Started: {time.strftime('%Y-%m-%d %H:%M', time.localtime(bench['started']))}")
    L.append(f"- Runs counted: {len(rows)}" + (f"; not counted: {len(skipped)}" if skipped else "")
             + (f"; listed in the bench but missing on disk: {len(missing)}" if missing else ""))
    L.append(f"- Groups (model and mode): {len(groups)}; modes present: {', '.join(modes_present) or 'none'}")
    L.append("- A run is one cycle: the bottle into the basket and back out, upright, at its start spot.")
    if not draw:
        L.append("- Figures: not drawn (" + ("matplotlib is not importable" if figures else "--no-figures") + ")")
    L.append("")

    # 1. success
    L.append("## 1. Success rate\n")
    L.append("A run succeeds only when the automatic verdict says so. Intervals are Wilson 95% score intervals; "
             "with this few trials they are wide, and overlapping intervals mean the data cannot order the groups. "
             "A clean success needed no recovery (no refusal, no failed motion, no knock-over).\n")
    L.append(table(["Model (mode)", "Runs", "Successes", "Rate", "Wilson 95% CI", "Clean", "Needed a recovery",
                    "Clean rate (95% CI)"],
                   [[glabel(g), a["runs"], a["successes"], pct(a["rate"]),
                     f"{pct(a['wilson95'][0])} to {pct(a['wilson95'][1])}" if a["runs"] else "n/a",
                     a["clean_successes"], a["recovered_successes"],
                     f"{pct(a['clean_rate'])} ({pct(a['clean_wilson95'][0])} to {pct(a['clean_wilson95'][1])})"
                     if a["runs"] else "n/a"]
                    for g, a in agg.items()]))
    pairs = []
    for md in modes_present:
        gs = [g for g in groups if agg[g]["mode"] == md]
        for i, g1 in enumerate(gs):
            for g2 in gs[i + 1:]:
                a1, a2 = agg[g1], agg[g2]
                pairs.append([f"{glabel(g1)} vs {glabel(g2)}", f"{a1['successes']}/{a1['runs']} vs "
                              f"{a2['successes']}/{a2['runs']}",
                              f"{fisher_exact(a1['successes'], a1['runs'], a2['successes'], a2['runs']):.3f}"])
    if pairs:
        L.append("\nPairwise within a mode, two-sided Fisher exact test (no correction for multiple comparisons; "
                 "p above 0.05 means \"not shown to differ\", not \"equal\"):\n")
        L.append(table(["Pair", "Successes", "p"], pairs))
    if draw and rows:
        fig_success(agg, groups, colours, figdir / "success_rate.png")
        L.append("\n![Success rate with Wilson 95% intervals (dashed = direct mode)](figures/success_rate.png)")
    L.append("")

    # 2. stages
    L.append("## 2. How far each run got\n")
    L.append("Furthest stage reached, from the logged state (holding, `bottle.where`) and the verdict: "
             + ", ".join(f"{i} {s}" for i, s in enumerate(STAGES)) + ".\n")
    L.append(table(["Model (mode)"] + [f"{i} {s}" for i, s in enumerate(STAGES)] + ["Mean stage"],
                   [[glabel(g)] + a["stages"] + [num(sum(i * c for i, c in enumerate(a["stages"])) / a["runs"], 2)
                                                 if a["runs"] else "n/a"] for g, a in agg.items()]))
    L.append("")

    # 3. effort
    L.append("## 3. Steps, time and tokens per run\n")
    L.append("Median [min, max] over all counted runs of each group, successes and failures together.\n")
    L.append(table(["Model (mode)", "Decisions", "Wall time (s)", "LLM time (s)", "Motion time (s)", "Tokens"],
                   [[glabel(g), med_range(a["decisions"]), med_range(a["wall_s"]), med_range(a["llm_s"]),
                     med_range(a["motion_s"]), med_range(a["tokens"], tokens)] for g, a in agg.items()]))
    succ_rows = [[glabel(g), med_range(describe([r["decisions"] for r in rows if r["group"] == g and r["success"]])),
                  med_range(describe([r["wall_s"] for r in rows if r["group"] == g and r["success"]])),
                  med_range(describe([r["tokens"] for r in rows if r["group"] == g and r["success"]]), tokens)]
                 for g in groups]
    L.append("\nSuccessful runs only:\n")
    L.append(table(["Model (mode)", "Decisions", "Wall time (s)", "Tokens"], succ_rows))
    if draw and rows:
        fig_effort(rows, groups, colours, figdir / "effort.png")
        L.append("\n![Decisions, time and tokens per run](figures/effort.png)")
    L.append("")

    # 4. refusals and recoveries
    L.append("## 4. Refusals, failed motions and recoveries\n")
    L.append("Refusal rate = decisions the governor refused / all decisions. Decisions within a run are not "
             "independent, so no interval is given for this pooled rate; the runs-with-a-refusal column is the "
             "per-run view. A failed motion is a decision that was allowed but did not complete (its `detail` "
             "does not read as a refusal). A retry is a later decision that picks the same skill as an earlier "
             "failed one; a knock-over is the bottle's `upright` flag going from true to false.\n")
    L.append(table(["Model (mode)", "Decisions", "Refused", "Refusal rate", "Runs with a refusal", "Failed motions",
                    "Retries", "Knock-overs"],
                   [[glabel(g), a["decisions_total"], a["refused_total"], pct(a["refusal_rate"]),
                     f"{a['runs_with_refusal']}/{a['runs']}", a["failed_decisions_total"], a["retries_total"],
                     a["knock_overs_total"]] for g, a in agg.items()]))
    L.append("\nPer run, median [min, max]:\n")
    L.append(table(["Model (mode)", "Refused", "Failed motions", "Retries"],
                   [[glabel(g), med_range(a["refused_per_run"]), med_range(a["failed_motions"]),
                     med_range(a["retries"])] for g, a in agg.items()]))
    L.append("")

    # 5. skills vs direct
    L.append("## 5. Skills versus direct control\n")
    L.append("In `skills` mode the model picks a named skill and code carries it out; in `direct` mode the model "
             "outputs joint targets or tool moves itself. Same task, same verdict.\n")
    both = [m for m in models if group_of(m, "skills") in agg and group_of(m, "direct") in agg]
    if both:
        rows5 = []
        for m in both:
            s, d = agg[group_of(m, "skills")], agg[group_of(m, "direct")]
            for name, a in (("skills", s), ("direct", d)):
                rows5.append([label(m), name, f"{a['successes']}/{a['runs']}", pct(a["rate"]),
                              f"{pct(a['wilson95'][0])} to {pct(a['wilson95'][1])}", a["clean_successes"],
                              med_range(a["decisions"]), med_range(a["wall_s"]), med_range(a["tokens"], tokens),
                              pct(a["refusal_rate"]), a["failed_decisions_total"],
                              f"{fisher_exact(s['successes'], s['runs'], d['successes'], d['runs']):.3f}"
                              if name == "direct" else ""])
        L.append(table(["Model", "Mode", "Successes", "Rate", "Wilson 95% CI", "Clean", "Decisions", "Wall (s)",
                        "Tokens", "Refusal rate", "Failed motions", "p (skills vs direct)"], rows5))
        ks = sum(agg[group_of(m, "skills")]["successes"] for m in both)
        ns = sum(agg[group_of(m, "skills")]["runs"] for m in both)
        kd = sum(agg[group_of(m, "direct")]["successes"] for m in both)
        nd = sum(agg[group_of(m, "direct")]["runs"] for m in both)
        ls, hs = wilson(ks, ns)
        ld, hd = wilson(kd, nd)
        L.append(f"\nPooled over the {len(both)} model(s) run in both modes: skills {ks}/{ns} ({pct(ks / ns)}, "
                 f"{pct(ls)} to {pct(hs)}), direct {kd}/{nd} ({pct(kd / nd)}, {pct(ld)} to {pct(hd)}), Fisher "
                 f"p = {fisher_exact(ks, ns, kd, nd):.3f}. Pooling mixes models; read the per-model rows first.")
    else:
        L.append("No model has runs in both modes yet" + (f" (modes present: {', '.join(modes_present)})."
                                                          if modes_present else "."))
    L.append("")

    # 6. failures
    L.append("## 6. Failure taxonomy\n")
    L.append("Each failed run gets one category: the verdict's reason when it is a code, else keywords in the "
             "reason, then the end checks, then the last error or refused steps, then the run state.\n")
    cats = sorted({r["category"] for r in rows if not r["success"]},
                  key=lambda c: (-sum(1 for r in rows if r["category"] == c), c))
    if cats:
        L.append(table(["Category", "Meaning"] + [glabel(g) for g in groups] + ["Total", "Example run"],
                       [[c, CATEGORY_LABELS.get(c, "(reason code from the run)")]
                        + [agg[g]["failures"].get(c, 0) for g in groups]
                        + [sum(1 for r in rows if r["category"] == c),
                           next(r["id"] for r in rows if r["category"] == c)] for c in cats]))
    else:
        L.append("No failed runs.")
    L.append("")

    # 7. sequences
    L.append("## 7. Step sequences\n")
    L.append("Decisions in order (REFUSED = the governor refused it, FAILED = it ran and failed), then the step "
             "log with seconds since the start.\n")
    picks = []
    t = typical([r for r in rows if r["clean"]], True) or typical(rows, True)
    if t:
        picks.append((f"Typical success: {glabel(t['group'])}, {t['decisions']} decisions", by_id[t["id"]]))
    t = typical([r for r in rows if r["recovered"]], True)
    if t:
        picks.append((f"Success after a recovery: {glabel(t['group'])}, {t['decisions']} decisions, "
                      f"{t['refused']} refused, {t['failed_decisions']} failed motions", by_id[t["id"]]))
    for c in cats:
        r = typical([x for x in rows if x["category"] == c], False)
        picks.append((f"Failure, {c}: {glabel(r['group'])}, {r['decisions']} decisions", by_id[r["id"]]))
    for title, run in picks:
        L.append(f"### {title} (`{run.get('id')}`)\n")
        L.append("```\n" + sequence_text(run) + "\n```\n")
    if draw and picks:
        fig_timelines(picks, figdir / "timelines.png")
        L.append("![Timelines of the runs above](figures/timelines.png)\n")

    # 8. contact sheets
    L.append("## 8. Contact sheets\n")
    sheet_names = []
    if sheets:
        for g in groups:
            gr = [r for r in rows if r["group"] == g]
            t = typical([r for r in gr if r["clean"]], True) or typical(gr, True)
            if t and contact_sheet(by_id[t["id"]], figdir / f"contact-{gslug(g)}.jpg"):
                sheet_names.append(g)
                L.append(f"{glabel(g)}, run `{t['id']}`:\n\n![{glabel(g)} contact sheet]"
                         f"(figures/contact-{gslug(g)}.jpg)\n")
        if not sheet_names:
            L.append("No successful run with step images.\n")
    else:
        L.append("Skipped (" + ("Pillow is not importable" if figures else "--no-figures") + ").\n")

    # 9. all runs
    L.append("## 9. All runs\n")
    L.append(table(["Run", "Model", "Mode", "Result", "Category", "Stage", "Decisions", "Refused", "Failed motions",
                    "Retries", "Wall (s)", "LLM (s)", "Motion (s)", "Tokens", "Reason"],
                   [[r["id"], label(r["model"]), r["mode"],
                     ("success" + ("" if r["clean"] else ", recovered")) if r["success"] else "fail",
                     r["category"], r["stage"], r["decisions"], r["refused"], r["failed_decisions"], r["retries"],
                     num(r["wall_s"]), num(r["llm_s"]), num(r["motion_s"]), tokens(r["tokens"]),
                     (r["reason"] or "")[:80]] for r in rows]))
    if skipped or missing:
        L.append("\nNot counted:\n")
        L += [f"- `{r.get('id')}` ({label(r.get('model') or '?')}): {why}" for r, why in skipped]
        L += [f"- `{rid}`: listed in the bench, no run.json on disk" for rid in missing]
    L.append("")

    (out / "REPORT.md").write_text("\n".join(L), encoding="utf-8")
    summary = {"bench": bench_id, "runs_dir": str(runs_dir), "generated": when, "models": models,
               "groups": groups, "per_group": agg, "runs": rows,
               "skipped": [{"id": r.get("id"), "why": w} for r, w in skipped],
               "missing": missing, "figures": draw, "contact_sheets": sheet_names}
    (out / "summary.json").write_text(json.dumps(summary, indent=1), encoding="utf-8")
    return {"out": str(out), "summary": summary}


def main(argv=None) -> int:
    ap = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    ap.add_argument("--runs", default=str(DEFAULT_RUNS), help="folder of run folders (default ~/gptarm-runs)")
    ap.add_argument("--bench", default=None, help="bench folder name, e.g. bench-20261116-101500 (default: newest; "
                                                  "'all' = every run folder)")
    ap.add_argument("--out", default=None, help="output folder (default <runs>/<bench>/report)")
    ap.add_argument("--exclude", action="append", default=[], metavar="RUN_ID",
                    help="leave a run out of the numbers (it is still listed); repeatable")
    ap.add_argument("--no-figures", action="store_true", help="tables only")
    a = ap.parse_args(argv)
    runs_dir = Path(a.runs).expanduser()
    if not runs_dir.is_dir():
        print(f"no runs folder at {runs_dir}", file=sys.stderr)
        return 2
    res = build(runs_dir, a.bench, a.out, a.exclude, figures=not a.no_figures)
    s = res["summary"]
    print(f"wrote {res['out']}/REPORT.md ({len(s['runs'])} runs, {len(s['groups'])} model/mode groups"
          + (", figures" if s["figures"] else ", no figures") + ")")
    return 0


if __name__ == "__main__":
    sys.exit(main())
