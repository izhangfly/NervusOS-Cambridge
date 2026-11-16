"""Learning from experience: a short list of lessons the model writes after each run and reads before the next.

The mechanism is ExpeL's insight list (Zhao et al., "ExpeL: LLM Agents Are Experiential Learners", AAAI 2024,
github.com/LeapLabTHU/ExpeL, Apache-2.0), cut down: after a run the model sees the run's log (text only:
the instruction, each motion, what happened, the verdict) and the current list, and answers with operations:
ADD a new lesson, AGREE with one (it held again), DISAGREE with one (this run contradicts it), or EDIT one.
Each lesson has a score: ADD starts at 2, AGREE +1, EDIT +1, DISAGREE -1; at 0 it is dropped. Only the
best-scored lessons are shown to the model (a fixed token budget), and a run adds at most two.

Benchmarks run without lessons unless asked: runs must not share context to be comparable.
"""
from __future__ import annotations

import json
import os
import re
import threading
import time
from pathlib import Path

HOME = Path(os.environ.get("GPTARM_HOME", "~/.config/gptarm")).expanduser()
PATH = HOME / "lessons.json"
SHOW = 12              # lessons shown to the model
KEEP = 20              # lessons kept (ExpeL's max_num_rules)
_BAD = re.compile(r"\(\s*-?\d+\s*,\s*-?\d+|\bstep\s*\d+|\brun\s*\d|\d{8}-\d{6}")   # coordinates, step or run numbers


def lint(text):
    """Why a proposed lesson is not general knowledge, or None. (Coordinates and run or step numbers belong
    to one run, not to the arm.)"""
    t = text.strip()
    if len(t) < 15:
        return "too short"
    if len(t.split()) > 45:
        return "too long"
    if _BAD.search(t):
        return "it names coordinates or a run or step number"
    return None
LOCK = threading.Lock()

SCHEMA = {"type": "object", "additionalProperties": False, "required": ["operations"],
          "properties": {"operations": {"type": "array", "items": {
              "type": "object", "additionalProperties": False, "required": ["op", "index", "text"],
              "properties": {"op": {"type": "string", "enum": ["ADD", "AGREE", "DISAGREE", "EDIT"]},
                             "index": {"anyOf": [{"type": "integer"}, {"type": "null"}]},
                             "text": {"anyOf": [{"type": "string"}, {"type": "null"}]}}}}}}

PROMPT = """\
You drove a robot arm through one run. Below are the run's log and the lessons collected from earlier runs.
Your job now is to improve the lessons, so that the next run goes better.

A lesson is one short sentence of general, reusable knowledge about driving THIS arm with THESE cameras on
THIS table: how the arm, the gripper, the cameras or the objects actually behave, or which kind of motion
works or fails and why. Not a summary of this run, not about this run's exact coordinates, not obvious.

Answer with operations on the list (use the numbers shown):
- ADD: a new lesson (at most two per run). index null.
- AGREE: this run confirms lesson [index]. text null.
- DISAGREE: this run contradicts lesson [index], or it is wrong or useless. text null.
- EDIT: rewrite lesson [index] to be more accurate or more general. text = the new sentence.
If nothing in this run teaches anything new, answer with an empty list or only AGREE/DISAGREE.

# Lessons so far
{lessons}

# The run
{log}
"""


def _load():
    try:
        d = json.loads(PATH.read_text())
        d.setdefault("enabled", True)
        d.setdefault("items", [])
        return d
    except (OSError, ValueError):
        return {"enabled": True, "items": []}


def _save(d):
    HOME.mkdir(parents=True, exist_ok=True)
    tmp = PATH.with_suffix(".tmp")
    tmp.write_text(json.dumps(d, indent=1))
    tmp.replace(PATH)


def state():
    with LOCK:
        d = _load()
    return {"enabled": d["enabled"], "count": len(d["items"]), "items": d["items"],
            "text": "\n".join(i["text"] for i in d["items"])}


def set_enabled(on):
    with LOCK:
        d = _load()
        d["enabled"] = bool(on)
        _save(d)


def replace(text):
    """The owner's edit: one lesson per line (scores kept for lines that did not change)."""
    with LOCK:
        d = _load()
        old = {i["text"]: i for i in d["items"]}
        items = []
        for line in (ln.strip(" -\t") for ln in (text or "").splitlines()):
            if line:
                items.append(old.get(line) or {"text": line, "source_run": "edited", "when": time.time(), "score": 3})
        d["items"] = items[:KEEP]
        _save(d)


def clear():
    with LOCK:
        d = _load()
        d["items"] = []
        _save(d)


def shown():
    """The lessons given to the model: the best-scored ones."""
    with LOCK:
        items = _load()["items"]
    return sorted(items, key=lambda i: -i["score"])[:SHOW]


def as_context(items):
    if not items:
        return ""
    lines = ["# Lessons from earlier runs", "Written by the model after earlier runs on this arm. Most are useful; "
             "some may be wrong. Use your judgement."]
    lines += [f"- {i['text']}" + ("" if i["score"] > 2 else " (unconfirmed: from one run only)") for i in items]
    return "\n".join(lines)


def run_log(d, limit=40):
    """A run, as text for the model: no pictures."""
    out = [f"Instruction: {d['instruction']}", f"Mode: {d.get('mode', 'skills')}"]
    for s in d["steps"]:
        k = s["kind"]
        if k == "decision":
            what = s.get("skill", "") + (f" {s['target']}" if s.get("target") else "")
            res = "done" if s.get("ok") else "NOT DONE"
            out.append(f"{s['n']}. {what}: {res}. {s.get('detail') or ''} | your note: {(s.get('note') or '')[:200]}")
        elif k in ("perception", "error", "info"):
            out.append(f"{s['n']}. ({k}) {s.get('note') or ''}")
        elif k == "verdict":
            out.append(f"Result: {s.get('note') or ''} {s.get('detail') or ''}")
    if len(out) > limit + 3:
        out = out[:3] + ["..."] + out[-limit:]
    o = d.get("outcome") or {}
    out.append(f"Outcome: success={o.get('success')} reason={o.get('reason') or '-'}")
    return "\n".join(out)


def learn(llm, d):
    """Ask the model for operations on the list and apply them. Returns (summary sentence, usage)."""
    with LOCK:
        items = list(_load()["items"])
    listing = "\n".join(f"[{n}] (score {i['score']}) {i['text']}" for n, i in enumerate(items)) or "(none yet)"
    ans, use = llm.ask(PROMPT.format(lessons=listing, log=run_log(d)), {}, SCHEMA)
    ops = ans.get("operations") or []
    added, said = 0, []
    with LOCK:
        cur = _load()
        items = cur["items"]
        for op in ops:
            kind, idx, text = op.get("op"), op.get("index"), (op.get("text") or "").strip()
            if kind == "ADD" and text and added < 2 and text not in [i["text"] for i in items]:
                why = lint(text)
                if why:
                    said.append(f"not kept ({why}): {text}")
                    continue
                if len(items) >= KEEP:                     # full: the weakest lesson pays for the new one
                    weakest = min(items, key=lambda i: i["score"])
                    weakest["score"] -= 3
                items.append({"text": text, "source_run": d["id"], "when": time.time(), "score": 2})
                added += 1
                said.append(f"new: {text}")
            elif idx is not None and 0 <= idx < len(items):
                it = items[idx]
                if kind == "AGREE":
                    it["score"] += 1
                    said.append(f"confirmed: {it['text']}")
                elif kind == "DISAGREE":
                    it["score"] -= 1
                    said.append(f"doubted: {it['text']}")
                elif kind == "EDIT" and text and not lint(text):
                    it.update(text=text, score=it["score"] + 1, source_run=d["id"], when=time.time())
                    said.append(f"rewritten: {text}")
        cur["items"] = sorted([i for i in items if i["score"] > 0], key=lambda i: -i["score"])[:KEEP]
        _save(cur)
    return ("; ".join(said) if said else "nothing new to learn from this run"), use
