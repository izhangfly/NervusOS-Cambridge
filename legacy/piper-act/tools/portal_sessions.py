#!/usr/bin/env python3
"""Export ALL post-training rounds to the training portal: sessions.json (index) + session_<N>.json (full metrics).

Runs as a persistent daemon. Each pass rebuilds every round's metrics and pushes the index plus per-round files.
The round that is training/gating is exported live; published rounds are exported from their surviving logs and
records (the merged dataset dir is deleted after publishing, so the dataset summary is rebuilt from the round record
and the `dataset.num_frames=` line in the train log).

    python tools/portal_sessions.py --once       build + push once
    python tools/portal_sessions.py --daemon     loop every 60 s
"""
import argparse
import json
import os
import re
import subprocess
import sys
import time
from datetime import datetime, timezone
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
from posttrain import config as C
from portal_export import Exporter, REMOTE, SSH_OPTS, VLA

REMOTE_DIR = "/opt/act-portal/www/data"
SESSIONS_REMOTE = f"{REMOTE_DIR}/sessions.json"


def discover_rounds() -> list[dict]:
    recs = []
    d = C.MAC_SHARED / "rounds"
    if d.is_dir():
        for p in sorted(d.glob("*.json")):
            try:
                recs.append(json.loads(p.read_text()))
            except Exception:
                pass
    return sorted(recs, key=lambda r: r["round"])


def round_config(n: int):
    sp = C.LOCAL / f"round{n}" / "supervisor.json"
    if sp.exists():
        return json.loads(sp.read_text())
    return None


def dataset_override(rec: dict) -> dict:
    task = None
    try:
        task = C.load_contract().get("task")
    except Exception:
        pass
    d = rec.get("dataset", {})
    frames = None
    log = C.LOCAL / f"round{rec['round']}" / "train.log"
    if log.exists():
        m = re.search(r"dataset\.num_frames=(\d+)", log.read_text(errors="replace"))
        if m:
            frames = int(m.group(1))
    base = d.get("base_episodes")
    human = d.get("human_train")
    policy = d.get("policy_train")
    return {"episodes": d.get("episodes_total"), "frames": frames, "fps": 15,
            "collection": (f"{base} base demos + {human} corrections + {policy} clean runs" if base is not None else None),
            "task": task}


def build_round(rec: dict):
    cfg = round_config(rec["round"])
    if cfg is None:
        return None
    ex = Exporter(cfg, run_name=f"round {rec['round']}", dataset_override=dataset_override(rec))
    return ex.build()


def current_round(recs: list[dict]) -> int | None:
    for rec in recs:
        if rec.get("phase") in ("training", "gating", "building"):
            return rec["round"]
    for rec in reversed(recs):
        if rec.get("phase") == "published":
            return rec["round"]
    return recs[-1]["round"] if recs else None


def sessions_index(recs: list[dict]) -> list[dict]:
    cur = current_round(recs)
    out = []
    for rec in recs:
        n = rec["round"]
        gate = rec.get("gate", {})
        entry = {
            "round": n, "phase": rec.get("phase"), "parent": rec.get("parent"),
            "started": rec.get("started"), "published": rec.get("published"),
            "reason": rec.get("reason"), "steps": rec.get("steps"),
            "current": n == cur,
            "gate": {k: (gate["metrics"][k] if "metrics" in gate else None) for k in ("A", "B")},
            "ranking": gate.get("ranking"),
        }
        out.append(entry)
    return out


def push(remote_path: str, data) -> int:
    if not REMOTE:
        raise RuntimeError("PIPER_PORTAL_REMOTE is not set (e.g. export PIPER_PORTAL_REMOTE=root@<server>)")
    body = json.dumps(data, separators=(",", ":")).encode()
    cmd = ["ssh", *SSH_OPTS, REMOTE, f"cat > {remote_path}.tmp && mv {remote_path}.tmp {remote_path}"]
    r = subprocess.run(cmd, input=body, capture_output=True, timeout=60)
    if r.returncode != 0:
        raise RuntimeError(r.stderr.decode()[-500:])
    return len(body)


def run_once(log) -> dict:
    recs = discover_rounds()
    if not recs:
        log("no rounds yet")
        return {"rounds": 0}
    index = sessions_index(recs)
    push(SESSIONS_REMOTE, index)
    n = 0
    for rec in recs:
        try:
            doc = build_round(rec)
            if doc is None:
                continue
            push(f"{REMOTE_DIR}/session_{rec['round']}.json", doc)
            n += 1
        except Exception as e:
            log(f"round {rec['round']}: {e}")
    # keep metrics.json as the current-session alias for the existing single-run page
    cur = current_round(recs)
    cur_cfg = round_config(cur)
    if cur_cfg is not None:
        try:
            doc = Exporter(cur_cfg, run_name=f"round {cur}", dataset_override=dataset_override(
                next(r for r in recs if r["round"] == cur))).build()
            push(f"{REMOTE_DIR}/metrics.json", doc)
        except Exception as e:
            log(f"metrics.json alias: {e}")
    log(f"pushed {n} round files + sessions index")
    return {"rounds": len(recs), "files": n, "current": cur}


def daemonize(logfile: str) -> None:
    if os.fork() > 0:
        os._exit(0)
    os.setsid()
    if os.fork() > 0:
        os._exit(0)
    fd = os.open(logfile, os.O_WRONLY | os.O_CREAT | os.O_APPEND, 0o644)
    os.dup2(fd, 1); os.dup2(fd, 2)
    os.dup2(os.open(os.devnull, os.O_RDONLY), 0)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--once", action="store_true")
    ap.add_argument("--daemon", action="store_true")
    ap.add_argument("--interval", type=int, default=60)
    a = ap.parse_args()

    logf = C.LOCAL / "portal_sessions.log"
    if a.daemon:
        daemonize(str(logf))
        (C.LOCAL / "portal_sessions.pid").write_text(str(os.getpid()))

    def log(msg):
        line = f"[{datetime.now(timezone.utc):%H:%M:%S}] {msg}"
        with open(logf, "a") as f:
            f.write(line + "\n")
        print(line, flush=True)

    if a.once:
        run_once(log)
        return
    log(f"portal_sessions daemon start pid={os.getpid()} interval={a.interval}s")
    while True:
        try:
            run_once(log)
        except Exception as e:
            log(f"pass failed: {e}")
        time.sleep(a.interval)


if __name__ == "__main__":
    main()
