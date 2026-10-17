"""Mac post-training orchestrator (AUTOMATED POST-TRAINING.md §5, contract v1).

State lives in two places:
  posttrain/mac/ (synced)   STATUS.json, ledger.json, CANDIDATE.json, rounds/<N>.json   <- the Dell can watch progress
  ~/NervusOS/robots/vla/posttrain/ (local)   base_trimmed, rollout_eps, round<N>/{data,rollouts,train}

Every round is a resumable state machine recorded in rounds/<N>.json:
  building -> training -> gating -> published | gate_failed | failed
"""
from __future__ import annotations

import csv
import os
import shutil
import subprocess
import sys
import time
import traceback
from pathlib import Path

import numpy as np

from . import config as C
from . import packages as P

LEDGER = C.MAC_SHARED / "ledger.json"
CANDIDATE = C.MAC_SHARED / "CANDIDATE.json"
ROUNDS = C.MAC_SHARED / "rounds"
LOCK = C.LOCAL / "pipeline.pid"
START_FLAG = C.LOCAL / "START_ROUND"


# ======================================================================================================================
# ledger
# ======================================================================================================================
def ledger() -> dict:
    return C.read_json(LEDGER, {"contract_version": C.SUPPORTED_CONTRACT, "runs": {}, "rollout_eps_count": 0})


def save_ledger(lg: dict) -> None:
    C.write_json_atomic(LEDGER, lg)


def accepted_episodes(lg: dict) -> list[dict]:
    eps = []
    for run in lg["runs"].values():
        if run.get("status") == "accepted":
            eps.extend(run.get("episodes", []))
    return sorted(eps, key=lambda e: (e.get("created", ""), e["id"]))


# ======================================================================================================================
# scan + convert (§5.2, §5.3)
# ======================================================================================================================
def scan_and_convert() -> dict:
    from . import lerodata as D
    lg = ledger()
    bounds = D.state_bounds()
    counts = {"new_accepted_runs": 0, "new_episodes": 0, "rejected": 0, "pending": 0}
    for d in P.scan_ready():
        run_id = d.name
        prev = lg["runs"].get(run_id)
        if prev and prev.get("status") in ("accepted", "rejected", "no_episodes", "error"):
            continue  # "error" = a Mac-side bug, not the package's fault: cleared by hand with `posttrain_ctl.py retry-errors`
        try:
            pkg = P.load_and_validate(d, bounds)
        except P.Pending as e:
            lg["runs"][run_id] = {"status": "pending", "reason": str(e), "scanned": C.utc_now()}
            counts["pending"] += 1
            continue
        except P.Rejected as e:
            lg["runs"][run_id] = {"status": "rejected", "reason": str(e), "scanned": C.utc_now()}
            C.log(f"rejected {run_id}: {e}")
            counts["rejected"] += 1
            save_ledger(lg)
            continue
        eps, notes = P.split_episodes(pkg)
        base = {"outcome": pkg.meta["outcome"], "case": pkg.meta.get("case"), "checkpoint": pkg.meta.get("checkpoint"),
                "interventions": pkg.meta.get("interventions"), "rows": len(pkg.rows), "notes": notes,
                "scanned": C.utc_now(), "first_round": None}
        if not eps:
            lg["runs"][run_id] = {"status": "no_episodes", **base, "episodes": []}
            save_ledger(lg)
            continue
        C.set_status("converting", f"{run_id}: {len(eps)} episode(s)")
        try:
            written = convert_package(pkg, eps, lg)
        except Exception as e:
            lg["runs"][run_id] = {"status": "error", "reason": f"Mac conversion error (package kept, not rejected): {e}", **base}
            C.log(f"conversion error {run_id}: {e}\n{traceback.format_exc()}")
            counts["errors"] = counts.get("errors", 0) + 1
            save_ledger(lg)
            continue
        lg["runs"][run_id] = {"status": "accepted", **base, "episodes": written}
        save_ledger(lg)
        counts["new_accepted_runs"] += 1
        counts["new_episodes"] += len(written)
        C.log(f"accepted {run_id}: {[(e['id'], e['kind'], e['frames']) for e in written]}")
    save_ledger(lg)
    return counts


def convert_package(pkg: P.Package, eps: list[P.EpisodeSpec], lg: dict) -> list[dict]:
    """Append each episode to the local rollout_eps dataset (h264), then verify it (§5.3)."""
    from . import lerodata as D
    task = C.load_contract()["task"]
    repo = "local/piper_rollout_eps"
    exists = (C.ROLLOUT_EPS / "meta" / "info.json").exists()
    ds = D.open_for_append(C.ROLLOUT_EPS, repo) if exists else D.create_dataset(C.ROLLOUT_EPS, repo)
    start_index = ds.meta.total_episodes
    mp4 = pkg.dir / "wrist.mp4"
    out, firsts = [], []
    for i, ep in enumerate(eps):
        states = np.array([r["state"] for r in pkg.rows[ep.row_start:ep.row_end]], dtype=np.float32)
        frames = list(P.iter_frames(mp4, ep.row_start, ep.row_end))
        n = D.write_episode(ds, states, frames, task)
        firsts.append((frames[0], states))
        out.append({**ep.to_dict(), "rollout_index": start_index + i, "frames": n})
    ds.finalize()
    for rec, (f0, states) in zip(out, firsts):
        rec["verify"] = D.verify_dataset_episode(C.ROLLOUT_EPS, repo, rec["rollout_index"], rec["frames"], f0,
                                                 expected_states=states)
    lg["rollout_eps_count"] = start_index + len(eps)
    return out


# ======================================================================================================================
# rounds
# ======================================================================================================================
def round_records() -> list[dict]:
    if not ROUNDS.is_dir():
        return []
    recs = [C.read_json(p) for p in ROUNDS.glob("*.json")]
    return sorted([r for r in recs if r], key=lambda r: r["round"])


def save_round(rec: dict) -> None:
    rec["updated"] = C.utc_now()
    C.write_json_atomic(ROUNDS / f"{rec['round']}.json", rec)


def current_policy() -> dict:
    cur = C.read_json(C.POLICIES / "CURRENT.json")
    if not cur or not (C.POLICIES / cur["name"] / "model.safetensors").is_file():
        raise RuntimeError(f"policies/CURRENT.json missing or points to a missing policy: {cur}")
    return cur


def trigger_reason(force: bool = False) -> str | None:
    recs = round_records()
    if any(r["phase"] in ("building", "training", "gating") for r in recs):
        return None
    if not recs:
        return "round 0: idle-trimmed base, fixes the frozen start (spec §5.5)"
    if force:
        return "started by hand"
    last = recs[-1]
    if C.since_hours(last.get("started")) < C.MIN_HOURS_BETWEEN_ROUNDS:
        return None
    used = set(last.get("dataset", {}).get("rollout_episode_ids", [])) | set(last.get("dataset", {}).get("held_out_ids", []))
    # episodes that entered any earlier round (training or held out) are not "new"
    for r in recs[:-1]:
        used |= set(r.get("dataset", {}).get("rollout_episode_ids", [])) | set(r.get("dataset", {}).get("held_out_ids", []))
    new = [e for e in accepted_episodes(ledger()) if e["id"] not in used]
    new_h = [e for e in new if e["kind"] == "human"]
    if len(new) >= C.TRIGGER_NEW_ACCEPTED:
        return f"{len(new)} new accepted episodes since round {last['round']}"
    if len(new_h) >= C.TRIGGER_NEW_HUMAN:
        return f"{len(new_h)} new human episodes since round {last['round']}"
    return None


def plan_dataset(n: int, trained_ids: set | None = None, holdout_fraction: float | None = None) -> dict:
    """Round dataset = base_trimmed + all accepted rollout episodes, minus the newest 5 human episodes NOT yet trained
    (set B, held out this round), with human episodes duplicated up to 15% of episodes when rarer (§5.6, §5.7).

    trained_ids: human episode ids already used in a PRIOR round's training set. Held-out corrections graduate into
    the next round (§5.7), so an already-trained human must not be held out again (review finding: the plain
    `humans[-5:]` held the same corrections out every round, so none ever entered training)."""
    eps = accepted_episodes(ledger())
    humans = [e for e in eps if e["kind"] == "human"]
    trained = set(trained_ids or ())
    untrained = [e for e in humans if e["id"] not in trained]
    frac = C.HOLDOUT_FRACTION if holdout_fraction is None else holdout_fraction
    n_hold = min(C.HOLDOUT_MAX, max(1, round(frac * len(untrained)))) if len(untrained) >= 2 else 0
    held = untrained[-n_hold:] if n_hold else []  # newest untrained corrections; the rest go into training
    held_ids = {e["id"] for e in held}
    train = [e for e in eps if e["id"] not in held_ids]
    base_eps = int(C.read_json(C.BASE_TRIMMED / "meta" / "info.json")["total_episodes"])
    train_h = [e for e in train if e["kind"] == "human"]
    dups = []
    total = base_eps + len(train)
    if train_h and len(train_h) / total < C.HUMAN_MIN_FRACTION:
        for e in train_h:  # write each human episode at most twice
            if (len(train_h) + len(dups)) / (total + len(dups)) >= C.HUMAN_MIN_FRACTION:
                break
            dups.append(e)
    return {"base_episodes": base_eps, "rollout_episode_ids": [e["id"] for e in train],
            "rollout_indices": [e["rollout_index"] for e in train] + [e["rollout_index"] for e in dups],
            "duplicated_human_ids": [e["id"] for e in dups], "held_out_ids": sorted(held_ids),
            "held_out_indices": [e["rollout_index"] for e in held],
            "human_train": len(train_h), "policy_train": len(train) - len(train_h),
            "episodes_total": total + len(dups),
            "human_fraction": round((len(train_h) + len(dups)) / (total + len(dups)), 4) if (total + len(dups)) else 0.0}


def rollout_trim(S: np.ndarray) -> tuple[int, int] | None:
    """Keep [a, b) of a rollout episode: drop stillness before the first and after the last change of more than
    ROLLOUT_TRIM_THRESHOLD on any of the 7 dims (the gripper counts, so a release at the end is kept), padded by
    ROLLOUT_TRIM_KEEP frames. Same idea as the §5.4 base trim: idle frames teach "stay". None = never moves."""
    d0 = np.abs(S - S[0]).max(axis=1) > C.ROLLOUT_TRIM_THRESHOLD
    d1 = np.abs(S - S[-1]).max(axis=1) > C.ROLLOUT_TRIM_THRESHOLD
    if not d0.any():
        return None
    a = max(0, int(np.argmax(d0)) - C.ROLLOUT_TRIM_KEEP)
    b = min(len(S), int(len(S) - 1 - np.argmax(d1[::-1])) + C.ROLLOUT_TRIM_KEEP + 1)
    return (a, b) if b - a >= 30 else None


def rollout_slices(indices: list[int]) -> dict[int, tuple[int, int] | None]:
    from . import lerodata as D
    src = D.load(C.ROLLOUT_EPS, "local/piper_rollout_eps")
    ranges = D.episode_ranges(src.meta)
    out = {}
    for idx in set(indices):
        lo, hi = ranges[idx]
        S = np.stack(src.hf_dataset.select(range(lo, hi)).with_format("numpy")["observation.state"])
        out[idx] = rollout_trim(S)
    return out


def build_round_dataset(n: int, plan: dict) -> Path:
    """No rollout episodes -> train on base_trimmed directly. Otherwise re-encode the chosen rollout episodes into a
    small h264 dataset and aggregate it with base_trimmed (same codec: video files are concatenated, not re-encoded)."""
    from . import lerodata as D
    if not plan["rollout_indices"]:
        return C.BASE_TRIMMED
    rdir = C.LOCAL / f"round{n}"
    sel_root = rdir / "rollouts"
    data_root = rdir / "data"
    shutil.rmtree(sel_root, ignore_errors=True)
    shutil.rmtree(data_root, ignore_errors=True)
    task = C.load_contract()["task"]
    src = D.load(C.ROLLOUT_EPS, "local/piper_rollout_eps")
    ranges = D.episode_ranges(src.meta)
    sel = D.create_dataset(sel_root, f"local/piper_pt{n}_rollouts")
    slices = rollout_slices(plan["rollout_indices"])
    plan["rollout_trim"] = {}
    written = 0
    for idx in plan["rollout_indices"]:
        lo, hi = ranges[idx]
        sl = slices[idx]
        if sl is None:
            plan["rollout_trim"][str(idx)] = "skipped: no motion or < 30 frames after trim"
            continue
        a, b = sl
        plan["rollout_trim"][str(idx)] = {"frames": hi - lo, "kept": [a, b], "removed": (hi - lo) - (b - a)}
        cols = src.hf_dataset.select(range(lo + a, lo + b)).with_format("numpy")
        S = np.stack(cols["observation.state"]); A = np.stack(cols["action"])  # contiguous slice keeps action[t]=state[t+1]
        for i, gi in enumerate(range(lo + a, lo + b)):
            sel.add_frame({"observation.state": S[i], "action": A[i], C.IMAGE_KEY: D.to_uint8_hwc(src[gi][C.IMAGE_KEY]), "task": task})
        sel.save_episode()
        written += 1
        C.set_status("converting", f"round {n}: building dataset, rollout episode {written}/{len(plan['rollout_indices'])}", round=n)
    sel.finalize()
    from lerobot.datasets.aggregate import aggregate_datasets
    aggregate_datasets(repo_ids=["local/piper_pick_place_trimmed", f"local/piper_pt{n}_rollouts"],
                       aggr_repo_id=f"local/piper_pick_place_pt{n}", roots=[C.BASE_TRIMMED, sel_root], aggr_root=data_root)
    merged = D.load(data_root, f"local/piper_pick_place_pt{n}")
    base = D.load(C.BASE_TRIMMED, "local/piper_pick_place_trimmed")
    sel = D.load(sel_root, f"local/piper_pt{n}_rollouts")  # reload: the writer object has no episode metadata after finalize
    want_eps = base.meta.total_episodes + sel.meta.total_episodes
    want_frames = base.meta.total_frames + sel.meta.total_frames
    if merged.meta.total_episodes != want_eps or merged.meta.total_frames != want_frames:
        raise AssertionError(f"aggregate: {merged.meta.total_episodes} eps / {merged.meta.total_frames} frames, "
                             f"want {want_eps} / {want_frames}")
    # spot-check that concatenated video decodes to the source frames at both ends of the base/rollout seam
    import torch
    for mi, (sds, si) in {base.meta.total_frames - 1: (base, base.meta.total_frames - 1),
                          base.meta.total_frames: (sel, 0)}.items():
        mad = float(torch.abs(merged[mi][C.IMAGE_KEY] - sds[si][C.IMAGE_KEY]).mean())
        if mad > 1 / 255:
            raise AssertionError(f"aggregate frame {mi} differs from source (MAD {mad:.4f})")
    return data_root


def supervisor_config(n: int, data_root: Path, parent: str, steps: int, save_freq: int) -> Path:
    rdir = C.LOCAL / f"round{n}"
    cfg = {
        "_why": f"Post-training round {n} (AUTOMATED POST-TRAINING.md §5.6), fine-tuned from {parent}.",
        "output_dir": str(rdir / "train"),
        "steps": steps,
        "args": [
            f"--policy.path={C.POLICIES / parent}",
            "--policy.device=mps",
            "--policy.push_to_hub=false",
            f"--dataset.repo_id={'local/piper_pick_place_trimmed' if data_root == C.BASE_TRIMMED else f'local/piper_pick_place_pt{n}'}",
            f"--dataset.root={data_root}",
            "--dataset.image_transforms.enable=true",
            "--batch_size=8",
            f"--save_freq={save_freq}",
            "--log_freq=200",
            f"--job_name=act_piper_pick_place_pt{n}",
            "--wandb.enable=false",
        ],
        "poll_s": 30, "hang_timeout_s": 900, "backoff_base_s": 30, "max_restarts": 12, "min_free_gb": 6,
    }
    path = rdir / "supervisor.json"
    C.write_json_atomic(path, cfg)
    return path


def supervisor_alive(n: int) -> bool:
    pidf = C.LOCAL / f"round{n}" / "train.supervisor.pid"
    try:
        pid = int(pidf.read_text())
        os.kill(pid, 0)
        return True
    except (FileNotFoundError, ValueError, ProcessLookupError, PermissionError):
        return False


def launch_training(n: int) -> None:
    cfg = C.LOCAL / f"round{n}" / "supervisor.json"
    subprocess.run([str(C.VENV_PY), str(C.VLA / "tools" / "train_supervisor.py"), "--config", str(cfg), "--daemon"],
                   check=True, cwd=C.VLA)
    time.sleep(3)


def trained_human_ids() -> set[str]:
    """Human episode ids present in any PRIOR round's training set (so they graduate out of set B)."""
    out: set[str] = set()
    for r in round_records():
        out |= {e for e in r.get("dataset", {}).get("rollout_episode_ids", []) if "#h" in e}
    return out


def _build_and_launch(rec: dict, reason: str) -> None:
    n = rec["round"]
    parent = rec["parent"]
    plan = plan_dataset(n, trained_human_ids(), rec.get("holdout_fraction"))
    data_root = build_round_dataset(n, plan)
    rec["dataset"] = {**plan, "root": str(data_root)}
    if n == 0:
        rep = C.read_json(C.BASE_TRIMMED / "TRIM_REPORT.json")
        rec["trim"] = {"frames_removed": rep["frames_removed"], "frames_total": rep["frames_total"],
                       "episodes_losing_over_25": rep["episodes_losing_over_25"],
                       "per_episode": [{k: e[k] for k in ("src_episode", "frames_before", "removed", "frames_after")}
                                       for e in rep["episodes"]]}
    cfg = supervisor_config(n, data_root, parent, int(rec.get("steps", C.FINETUNE_STEPS)), int(rec.get("save_freq", C.SAVE_FREQ)))
    rec["command"] = C.read_json(cfg)
    rec["phase"] = "training"
    save_round(rec)
    if not supervisor_alive(n):
        launch_training(n)
    C.set_status("training", f"round {n} fine-tuning from {parent}", round=n)
    C.log(f"round {n} {'resumed build and' if reason.startswith('resume') else 'started'}: {reason}")


def start_round(reason: str, parent: str | None = None, steps: int | None = None, save_freq: int | None = None,
                holdout_fraction: float | None = None) -> dict:
    recs = round_records()
    n = recs[-1]["round"] + 1 if recs else 0
    parent = parent or current_policy()["name"]
    if not (C.POLICIES / parent / "model.safetensors").is_file():
        raise RuntimeError(f"parent policy {parent} not found in policies/")
    rec = {"round": n, "phase": "building", "reason": reason, "started": C.utc_now(), "parent": parent,
           "current_at_start": current_policy()["name"], "steps": int(steps or C.FINETUNE_STEPS),
           "save_freq": int(save_freq or C.SAVE_FREQ), "holdout_fraction": holdout_fraction}
    save_round(rec)
    C.set_status("converting", f"round {n}: building dataset", round=n)
    try:
        _build_and_launch(rec, reason)
    except Exception as e:
        rec["phase"], rec["error"] = "failed", f"{e}"
        save_round(rec)
        C.set_status("blocked", f"round {n} failed while building: {e}", round=n)
        C.log(f"round {n} failed: {e}\n{traceback.format_exc()}")
    return rec


def resume_build(rec: dict) -> None:
    """A round left in phase='building' by a crash/reboot: re-run the build (idempotent, it wipes its output dirs) and
    launch. Fixes the 'stuck in building forever' review finding."""
    C.set_status("converting", f"round {rec['round']}: resuming interrupted build", round=rec["round"])
    try:
        _build_and_launch(rec, "resume")
    except Exception as e:
        rec["phase"], rec["error"] = "failed", f"resume build: {e}"
        save_round(rec)
        C.set_status("blocked", f"round {rec['round']} resume build failed: {e}", round=rec["round"])
        C.log(f"round {rec['round']} resume build failed: {e}\n{traceback.format_exc()}")


def progress_round(rec: dict) -> None:
    """Advance a round that is training or gating (called every loop; safe after a crash or reboot)."""
    n = rec["round"]
    rdir = C.LOCAL / f"round{n}"
    if rec["phase"] == "training":
        st = C.read_json(rdir / "train.status.json", {})
        if st.get("state") == "finished":
            rec["phase"] = "gating"
            rec["training"] = {k: st.get(k) for k in ("step", "metrics", "restarts", "history", "time")}
            save_round(rec)
        elif st.get("state", "").startswith(("fatal", "gave_up")):
            rec["phase"], rec["error"] = "failed", f"training {st['state']}"
            save_round(rec)
            C.set_status("blocked", f"round {n} training {st['state']}: see {rdir}/train.supervisor.log", round=n)
            return
        elif not supervisor_alive(n):
            C.log(f"round {n}: supervisor not running and not finished -> relaunching (it resumes from the last checkpoint)")
            launch_training(n)
            return
        else:
            C.set_status("training", f"round {n}: step {st.get('step')}/{st.get('target')}, ETA {st.get('eta_hours')} h",
                         round=n)
            return
    if rec["phase"] == "gating":
        gate_and_publish(rec)


def gate_and_publish(rec: dict) -> None:
    from . import gate as G
    n = rec["round"]
    C.set_status("gating", f"round {n}: offline gate", round=n)
    ckdir = C.LOCAL / f"round{n}" / "train" / "checkpoints"
    # every saved checkpoint competes (a 100k run saves 10); skip the first save when there are several
    steps_saved = sorted(p.name for p in ckdir.iterdir() if p.name.isdigit()
                         and (p / "pretrained_model" / "model.safetensors").is_file())
    if len(steps_saved) > 2:
        steps_saved = steps_saved[1:]
    cands = {f"act_pick_place_pt{n}_{s}": ckdir / s / "pretrained_model" for s in steps_saved}
    parent = rec["parent"]
    current = current_policy()["name"]
    policies = {current: C.POLICIES / current, **({parent: C.POLICIES / parent} if parent != current else {}), **cands}
    held = rec["dataset"].get("held_out_indices", [])
    b_slices = {i: sl for i, sl in rollout_slices(held).items() if sl is not None} if held else {}
    # Set A runs on the TRIMMED base: evaluating "no forgetting" against the untrimmed idle prefix would
    # structurally penalize the round-0 candidate for not reproducing the frozen start (review finding).
    sets = {"A": (C.BASE_TRIMMED, "local/piper_pick_place_trimmed", C.GATE_SET_A),
            "B": (C.ROLLOUT_EPS, "local/piper_rollout_eps", [i for i in held if i in b_slices], b_slices)}
    metrics = G.evaluate(policies, sets, device="cpu", log=C.log)
    decisions = {name: G.decide(metrics, name, current) for name in cands}
    rec["gate"] = {"metrics": metrics, "decisions": decisions, "current": current, "parent": parent,
                   "reference": "CURRENT 100000 = 1.55 / hold-still 6.99 on TRIMMED set A (156 samples), measured on the Mac; "
                                "the gate compares candidate and CURRENT on identical samples, so only the ratio matters"}
    passing = [nm for nm, d in decisions.items() if d["pass"]]
    if not passing:
        rec["phase"] = "gate_failed"
        rec["cleanup"] = cleanup_round(n)
        save_round(rec)
        C.set_status("idle", f"round {n}: no checkpoint passed the offline gate; nothing published", round=n)
        append_update(rec, published=[])
        return
    # primary = best passing checkpoint by A and B relative to CURRENT; also publish the most-trained passing one
    ranked = sorted(passing, key=lambda nm: G.score(metrics, nm, current))
    primary = ranked[0]
    to_publish = [primary]
    last = max(passing, key=lambda nm: nm.rsplit("_", 1)[1])
    if last != primary:
        to_publish.append(last)
    rec["gate"]["ranking"] = [(nm, round(G.score(metrics, nm, current), 4)) for nm in ranked]
    for name in to_publish:
        publish_policy(cands[name], name)
    prev = C.read_json(CANDIDATE)
    if prev and prev.get("status") == "candidate":
        prev["status"] = "superseded"
        prev["superseded_by"] = primary
        C.write_json_atomic(C.MAC_SHARED / "history" / f"candidate_{prev['name']}.json", prev)
    C.write_json_atomic(CANDIDATE, {
        "name": primary, "also_published": [x for x in to_publish if x != primary], "parent": parent, "current": current, "round": n,
        "status": "candidate", "published": C.utc_now(),
        "dataset": {k: rec["dataset"][k] for k in ("base_episodes", "human_train", "policy_train", "episodes_total",
                                                   "human_fraction", "held_out_ids", "duplicated_human_ids")},
        "gate": {"metrics": metrics, "decisions": {k: decisions[k] for k in to_publish}},
        "promotion_rule": "needs >= 10 labelled trials in deploy/runs/results.csv with success rate >= CURRENT's most recent 10"})
    rec["phase"], rec["published"] = "published", to_publish
    rec["cleanup"] = cleanup_round(n)
    save_round(rec)
    C.set_status("published", f"round {n}: published {', '.join(to_publish)}; waiting for >= 10 real-arm trials", round=n)
    append_update(rec, published=to_publish)
    C.log(f"round {n} published {to_publish}")


def cleanup_round(n: int) -> dict:
    """After gating: drop what can be rebuilt (merged data, re-encoded rollouts, optimizer state). Keeps every
    pretrained_model so the round stays auditable. Disk on this Mac is ~20 GB free; one round leaves ~3 GB otherwise."""
    rdir = C.LOCAL / f"round{n}"
    freed = 0
    targets = [rdir / "data", rdir / "rollouts"] + sorted((rdir / "train" / "checkpoints").glob("*/training_state"))
    for t in targets:
        if t.is_dir() and not t.is_symlink():
            freed += sum(f.stat().st_size for f in t.rglob("*") if f.is_file())
            shutil.rmtree(t, ignore_errors=True)
    return {"freed_mb": round(freed / 1e6), "removed": [str(t.relative_to(rdir)) for t in targets]}


def publish_policy(src: Path, name: str) -> None:
    """Atomic: copy to policies/.tmp_<name> (hidden from the portal), then rename."""
    dest = C.POLICIES / name
    if (dest / "model.safetensors").is_file():
        return
    tmp = C.POLICIES / f".tmp_{name}"
    shutil.rmtree(tmp, ignore_errors=True)
    shutil.copytree(src, tmp)
    os.replace(tmp, dest)


def append_update(rec: dict, published: list[str]) -> None:
    n, g = rec["round"], rec.get("gate", {})
    m = g.get("metrics", {})
    names = list(dict.fromkeys([g.get("current", rec["parent"]), rec["parent"]]
                               + [k for k in m.get("A", {}) if k.startswith(f"act_pick_place_pt{n}_")]))
    lines = [f"\n---\n## [MAC] {time.strftime('%Y-%m-%d %H:%M', time.gmtime())} UTC — POSTTRAIN round {n}",
             f"**Re:** {rec['reason']}. Parent `{rec['parent']}`. "
             + (f"Published: {', '.join(f'`{p}`' for p in published)} (candidate, NOT CURRENT)." if published
                else "**Nothing published** (offline gate failed)."), ""]
    ds = rec.get("dataset", {})
    lines.append(f"- Dataset: base_trimmed {ds.get('base_episodes')} episodes + {ds.get('policy_train', 0)} policy + "
                 f"{ds.get('human_train', 0)} human (+{len(ds.get('duplicated_human_ids', []))} duplicated), "
                 f"human fraction {ds.get('human_fraction')}; held out (set B): {len(ds.get('held_out_ids', []))}")
    if rec.get("trim"):
        t = rec["trim"]
        lines.append(f"- Round 0 idle trim: {t['frames_removed']} frames removed, {t['episodes_losing_over_25']} episodes lost > 25 frames")
    tr = rec.get("training") or {}
    lines.append(f"- Training: {tr.get('step')} steps, restarts {tr.get('restarts')}, last metrics `{tr.get('metrics')}`")
    lines += ["", "| policy | set A MAE | set B MAE |", "|---|---|---|"]
    for nm in names:
        a = m.get("A", {}).get(nm); b = m.get("B", {}).get(nm, "—")
        lines.append(f"| `{nm}` | {a} | {b} |")
    lines.append(f"| hold still | {m.get('A', {}).get('hold_still')} | {m.get('B', {}).get('hold_still', '—')} |")
    for nm, d in g.get("decisions", {}).items():
        lines.append(f"- `{nm}`: {'PASS' if d['pass'] else 'FAIL'} — A: {d['A_rule']}; B: {d['B_rule']}")
    if published:
        lines.append(f"- **Dell:** run >= 10 labelled trials of `{published[0]}` (portal lists it). "
                     "The Mac promotes it to CURRENT only if its success rate >= CURRENT's most recent 10 (§5.9).")
    with open(C.UPDATE_MD, "a", encoding="utf-8") as f:
        f.write("\n".join(lines) + "\n")


# ======================================================================================================================
# promotion (§5.9)
# ======================================================================================================================
def read_results() -> list[dict]:
    if not C.RESULTS_CSV.exists():
        return []
    with open(C.RESULTS_CSV, newline="", encoding="utf-8") as f:
        rows = [r for r in csv.DictReader(f) if r.get("outcome") in ("success", "fail")]
    # The Dell portal appends a row on EVERY Success/Fail click; count one trial per run (last label wins).
    by_run = {}
    for r in rows:
        rid = r.get("run_id")
        if rid:
            by_run[rid] = r
    return list(by_run.values())


def check_promotion() -> str | None:
    cand = C.read_json(CANDIDATE)
    if not cand or cand.get("status") != "candidate":
        return None
    cur = current_policy()
    rows = read_results()
    names = [cand["name"]] + list(cand.get("also_published", []))
    best = None
    for nm in names:
        trials = [r for r in rows if r["checkpoint"] == nm]
        if len(trials) < 10:
            continue
        rate = sum(r["outcome"] == "success" for r in trials) / len(trials)
        if best is None or rate > best[1]:
            best = (nm, rate, len(trials))
    if best is None:
        return None
    cur_trials = [r for r in rows if r["checkpoint"] == cur["name"]][-10:]
    if not cur_trials:
        return None  # no baseline: refuse to promote against an untested CURRENT (review finding)
    cur_rate = sum(r["outcome"] == "success" for r in cur_trials) / len(cur_trials)
    nm, rate, count = best
    evidence = {"candidate": nm, "candidate_trials": count, "candidate_success_rate": round(rate, 3),
                "current": cur["name"], "current_trials_considered": len(cur_trials), "current_success_rate": round(cur_rate, 3),
                "rule": ">= 10 labelled trials and success rate >= CURRENT's most recent 10"}
    if rate >= cur_rate:
        C.write_json_atomic(C.POLICIES / "CURRENT.json", {"name": nm, "since": C.utc_now(),
                            "reason": f"Promoted by the Mac: {rate:.0%} over {count} real-arm trials vs {cur['name']} "
                                      f"{cur_rate:.0%} over its last {len(cur_trials)}", "evidence": evidence,
                            "previous": cur["name"]})
        cand.update(status="promoted", decided=C.utc_now(), evidence=evidence)
        verdict = f"promoted `{nm}` to CURRENT"
    else:
        cand.update(status="rejected", decided=C.utc_now(), evidence=evidence)
        verdict = f"rejected `{nm}` (episodes stay in the ledger and are still used)"
    C.write_json_atomic(CANDIDATE, cand)
    with open(C.UPDATE_MD, "a", encoding="utf-8") as f:
        f.write(f"\n---\n## [MAC] {time.strftime('%Y-%m-%d %H:%M', time.gmtime())} UTC — POSTTRAIN promotion\n"
                f"**Re:** {verdict}.\n\n- Evidence: `{evidence}`\n")
    C.log(f"promotion: {verdict} {evidence}")
    return verdict


# ======================================================================================================================
# daemon
# ======================================================================================================================
def tick(force: bool = False) -> None:
    import fcntl
    lockf = open(LOCK, "a+")
    try:
        fcntl.flock(lockf, fcntl.LOCK_EX | fcntl.LOCK_NB)
    except BlockingIOError:
        C.log("tick skipped: another tick is running")
        return
    try:
        _tick(force)
    finally:
        fcntl.flock(lockf, fcntl.LOCK_UN)
        lockf.close()


def _tick(force: bool = False) -> None:
    try:
        C.load_contract()
    except C.ContractError as e:
        C.set_status("blocked", f"refusing to run: {e}")
        C.log(f"blocked: {e}")
        return
    C.MAC_SHARED.mkdir(parents=True, exist_ok=True)
    counts = scan_and_convert()
    check_promotion()
    for rec in round_records():
        if rec["phase"] == "building":
            resume_build(rec)
            return
        if rec["phase"] in ("training", "gating"):
            progress_round(rec)
            return
    force = force or START_FLAG.exists()
    reason = trigger_reason(force=force)
    if reason:
        START_FLAG.unlink(missing_ok=True)
        start_round(reason)
        return
    recs = round_records()
    last = recs[-1] if recs else None
    detail = (f"waiting for data: {counts} ; last round {last['round']} {last['phase']}" if last else "no rounds yet")
    cand = C.read_json(CANDIDATE)
    if cand and cand.get("status") == "candidate":
        C.set_status("published", f"candidate {cand['name']} waiting for >= 10 real-arm trials; {detail}")
    else:
        C.set_status("idle", detail)


def daemon(interval_s: int = 300) -> None:
    C.LOCAL.mkdir(parents=True, exist_ok=True)
    try:
        other = int(LOCK.read_text())
        os.kill(other, 0)
        raise SystemExit(f"pipeline already running (pid {other})")
    except (FileNotFoundError, ValueError, ProcessLookupError):
        pass
    LOCK.write_text(str(os.getpid()))
    C.log(f"post-training daemon start pid={os.getpid()} interval={interval_s}s")
    while True:
        try:
            tick()
        except Exception as e:
            C.log(f"tick error: {e}\n{traceback.format_exc()}")
            try:
                C.set_status("blocked", f"pipeline error: {e}")
            except Exception:
                pass
        time.sleep(interval_s)
