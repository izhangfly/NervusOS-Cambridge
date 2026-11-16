"""One run (a cycle driven by a model) and a benchmark (many runs across models), recorded as API.md says.

A run starts with nothing but the instruction and the context: a new World, an empty history. It ends when
the model says done, when the step budget is spent, on a stop, or on a fault; then the result is checked by
the cameras and by touch, not taken from the model. Everything is written to ~/gptarm-runs/<id>/ as it
happens: run.json and the pictures the model was shown.
"""
from __future__ import annotations

import json
import math
import os
import threading
import time
from pathlib import Path

from . import arm as A
from . import direct as D
from . import judge as J
from . import lessons as Lz
from .recorder import Recorder
from . import llm as L
from . import task as T
from .cameras import to_jpeg

RUNS = Path(os.environ.get("GPTARM_RUNS", "~/gptarm-runs")).expanduser()


def _write(path: Path, obj):
    tmp = path.with_suffix(".tmp")
    tmp.write_text(json.dumps(obj))
    tmp.replace(path)


def scripted_policy(prompt, _images, _schema):
    """The scripted stand-in for a model: reads the state out of the prompt and applies task.oracle.
    (Skills mode only: in direct mode it says done at once.)"""
    if "# State\n" not in prompt:
        return {"action": "done", "x": None, "y": None, "z": None, "lean": None, "yaw": None, "joints": None,
                "gripper_mm": None, "note": "the scripted stand-in only knows the skills"}
    text = prompt.split("# State\n", 1)[1].split("\n\n# What happened", 1)[0]
    skill, target, note = T.oracle(json.loads(text))
    return {"skill": skill, "target": target, "note": note}


def make_llm(model):
    return L.Scripted(scripted_policy, model="scripted", delay_s=0.3) if model == "scripted" else L.client(model)


class Run:
    """The record of one run. `on_update` is called whenever it changes (the service keeps the latest)."""

    def __init__(self, model, instruction, mode="skills", on_update=None, root=None):
        stamp = time.strftime("%Y%m%d-%H%M%S")
        self.dir = (root or RUNS) / f"{stamp}-{model}"
        self.dir.mkdir(parents=True, exist_ok=True)
        self.on_update = on_update
        self.d = {"id": self.dir.name, "model": model, "mode": mode, "instruction": instruction, "state": "running",
                  "started": time.time(), "ended": None,
                  "outcome": {"success": None, "reason": "", "declared_done": False,
                              "checks": {"entered_basket": None, "returned_mm": None, "upright": None}},
                  "usage": {"llm_calls": 0, "tokens": 0, "llm_s": 0.0, "motion_s": 0.0, "failed_skills": 0, "refusals": 0},
                  "pending": None, "steps": []}

    recorder = None

    def step(self, kind, **kw):
        s = {"n": len(self.d["steps"]), "t": time.time(), "kind": kind,
             **{k: v for k, v in kw.items() if v is not None or (k == "ok" and kind == "verdict")}}
        self.d["steps"].append(s)
        if self.recorder is not None:
            what = (s.get("skill") or kind) + (f" {s['target']}" if s.get("target") else "")
            text = s.get("note") or ""
            if kind == "decision" and not s.get("ok", True):
                text += f"  [{s.get('detail', '')}]"
            self.recorder.caption(f"{s['n']}  {what}  -  {text}")
        self.save()
        return s

    def pending(self, stage=None, **kw):
        self.d["pending"] = None if stage is None else {"stage": stage, "since": time.time(), **kw}
        if self.recorder is not None and stage == "motion":
            self.recorder.caption(f"{len(self.d['steps'])}  {kw.get('skill', '')} {kw.get('target', '')}  -  {kw.get('note', '')}")
        self.save(disk=False)

    def save(self, disk=True):
        if disk:
            _write(self.dir / "run.json", self.d)
        if self.on_update:
            self.on_update(self.d)

    def image(self, name, img, max_side):
        if img is None:
            return None, None
        data = to_jpeg(img, 80, max_side)
        (self.dir / name).write_bytes(data)
        return name, data


def is_task(instruction):
    """The bottle cycle, which is checked automatically; any other instruction is the model's to judge."""
    return " ".join(instruction.split()).lower() == " ".join(T.DEFAULT_INSTRUCTION.split()).lower()


def run_cycle(world, llm, instruction, context, run, stop_event, mode="skills", lessons_on=False, learn=None, record=True):
    """One run. `world` must be new. Returns run.d.
    mode "skills": the model picks a skill; "direct": the model gives the motion itself (direct.py).
    lessons_on: the model reads the lessons from earlier runs; learn (default: lessons_on): afterwards it
    writes lessons from this one. record: a video of both cameras goes in the run's folder."""
    d = run.d
    items = Lz.shown() if lessons_on else []
    d["lessons_used"] = len(items)
    if items:
        context = context.rstrip() + "\n\n" + Lz.as_context(items)
    rec = None
    if record:
        try:
            rec = Recorder(world.overhead, world.wrist, run.dir / "video.mp4", title=f"{d['model']}  {mode}  {d['id']}",
                           depth=getattr(world, "depth", None))
            run.recorder = rec
            d["video"] = rec.info()
        except Exception:                                    # noqa: BLE001 (no video is no reason to stop)
            rec = None
    try:
        _cycle(world, llm, instruction, context, run, stop_event, mode)
    finally:
        if rec is not None:
            run.recorder = None
            d["video"] = rec.stop()
            run.save()
    if (lessons_on if learn is None else learn) and d["state"] in ("done", "failed") and not isinstance(llm, L.Scripted) \
            and any(s["kind"] == "decision" for s in d["steps"]) and not stop_event.is_set():
        run.pending("llm", note="writing lessons from this run")
        try:
            said, use = Lz.learn(llm, d)
            d["usage"]["tokens"] += int(use.get("tokens", 0))
            d["usage"]["llm_s"] = round(d["usage"]["llm_s"] + float(use.get("seconds", 0.0)), 2)
            run.step("lesson", note=said, tokens=int(use.get("tokens", 0)), llm_s=round(float(use.get("seconds", 0.0)), 2))
        except L.LLMError as e:
            run.step("info", note=f"No lessons written: {e}")
        d["pending"] = None
        run.save()
    return d


def _cycle(world, llm, instruction, context, run, stop_event, mode):
    d, cfg = run.d, world.cfg
    checked = is_task(instruction)
    d["checked"] = checked
    d["mode"] = mode
    direct = mode == "direct"
    if direct:
        world.arm.floor_z_mm = cfg.floor_direct
    max_steps = cfg.max_steps_direct if direct else cfg.max_steps
    history = []
    run.step("user", note=instruction)

    def finish(state, reason=None):
        d["state"], d["ended"], d["pending"] = state, time.time(), None
        if reason is not None:
            d["outcome"]["reason"] = reason
            d["outcome"]["success"] = False if state != "stopped" else None
        run.save()
        return d

    try:
        # start from a known place: gripper open and empty, arm up, one look
        t0 = time.time()
        run.pending("perception", note="looking at the table")
        if world.arm.state()["gripper_mm"] is not None and world.arm.state()["gripper_mm"] < A.GRIPPER_OPEN_MM - 8:
            world.arm.grip(A.GRIPPER_OPEN_MM)
        tool, _ = world.tool()
        if tool[2] < cfg.travel_z - 15.0:
            world._go(tool[0], tool[1], cfg.travel_z, min_tip_z=tool[2] - 30.0)
        note = world._look()
        run.before = to_jpeg(world.last_frames["overhead"], 80, 1024) if world.last_frames.get("overhead") is not None else None
        d["usage"]["motion_s"] += time.time() - t0
        ann, wrist = world.frames()
        names = {"overhead": run.image("000-overhead.jpg", ann, 1024)[0], "wrist": run.image("000-wrist.jpg", wrist, 768)[0]}
        run.step("perception", note=note, state=world.state(), images=names)
        if checked and (not world.bottle or world.bottle["where"] != "table"):
            run.step("error", note="The run cannot start: the camera cannot see the bottle on the table.")
            return finish("failed", "scene_not_ready")
        if checked and not world.basket:
            run.step("error", note="The run cannot start: the basket is not in view.")
            return finish("failed", "scene_not_ready")
        if world.bottle and world.bottle.get("xy") and world.bottle["where"] == "table":
            world.start_xy = list(world.bottle["xy"])
        plan = None
        if direct and not isinstance(llm, L.Scripted):
            run.pending("llm", note=f"{d['model']} is writing a plan")
            ann, wrist = world.frames(grid=True)
            imgs = {k: run.image(f"000-plan-{k}.jpg", im, 1024 if k == "overhead" else 768)[1]
                    for k, im in (("overhead", ann), ("wrist", wrist)) if im is not None}
            try:
                ans, use = llm.ask(D.plan_prompt(instruction, context), imgs, D.PLAN_SCHEMA)
                plan = ans.get("stages") or None
                d["plan"] = plan
                d["usage"]["llm_calls"] += 1
                d["usage"]["tokens"] += int(use.get("tokens", 0))
                d["usage"]["llm_s"] = round(d["usage"]["llm_s"] + float(use.get("seconds", 0.0)), 2)
                run.step("info", note="Plan: " + " | ".join(f"{n}. {s['goal']}" for n, s in enumerate(plan or [], 1)),
                         detail=D.plan_text(plan), tokens=int(use.get("tokens", 0)), llm_s=round(float(use.get("seconds", 0.0)), 2))
            except L.LLMError as e:
                run.step("error", note=f"The model could not be asked: {e}")
                return finish("failed", "model_unavailable")

        declared, rechecked = False, False
        while world.steps_used < max_steps:
            if stop_event.is_set():
                raise A.Stop("stopped")
            if not _arm_connected(world, run, stop_event):
                run.step("error", note="The arm's connection did not come back within 2 minutes (USB or CAN).")
                return finish("failed", "arm_fault")
            if direct:
                try:
                    world.observe()
                except Exception:                           # noqa: BLE001 (the model still gets the pictures)
                    pass
                state = D.state(world)
            else:
                state = world.state()
            ann, wrist = world.frames(grid=direct)
            n = len(d["steps"])
            (oname, odata), (wname, wdata) = run.image(f"{n:03d}-overhead.jpg", ann, 1024), run.image(f"{n:03d}-wrist.jpg", wrist, 768)
            images = {k: v for k, v in (("overhead", odata), ("wrist", wdata)) if v}
            dname = None
            if direct:
                dname, ddata = run.image(f"{n:03d}-depth.jpg", world.depth_view(), 960)
                if ddata:
                    images["depth"] = ddata
            run.pending("llm", note=f"waiting for {d['model']}")
            try:
                if direct:
                    ans, use = llm.ask(D.prompt(instruction, context, state, history, plan, depth_image="depth" in images),
                                       images, D.SCHEMA)
                else:
                    ans, use = llm.ask(T.prompt(instruction, context, state, history), images, T.SCHEMA)
            except L.LLMError as e:
                run.step("error", note=f"The model could not be asked: {e}")
                return finish("failed", "model_unavailable")
            d["usage"]["llm_calls"] += 1
            d["usage"]["tokens"] += int(use.get("tokens", 0))
            d["usage"]["llm_s"] = round(d["usage"]["llm_s"] + float(use.get("seconds", 0.0)), 2)
            if direct:
                skill, note = str(ans.get("action")), str(ans.get("note") or "")
                target = D.describe(ans).split(" ", 1)[1] if " " in D.describe(ans) else "none"
            else:
                skill, target, note = str(ans.get("skill")), str(ans.get("target") or "none"), str(ans.get("note") or "")
            run.pending("motion", skill=skill, target=target, note=note)
            t0 = time.time()
            ok, detail = True, ""
            try:
                detail = D.execute(world, ans) if direct else world.do(skill, target)
            except (T.Refused, D.Refused) as e:
                ok, detail = False, f"refused: {e}"
                d["usage"]["refusals"] += 1
            except A.ArmError as e:
                ok, detail = False, f"failed: {e}"
                d["usage"]["failed_skills"] += 1
                world.arm.hold()
            dt = time.time() - t0
            world.steps_used += 1
            d["usage"]["motion_s"] = round(d["usage"]["motion_s"] + dt, 2)
            history.append((D.describe(ans), ok, detail, ans.get("expect")) if direct else (skill, target, note, ok, detail))
            rec = state
            if direct:                    # the standard State (API.md), and exactly what the model was given
                rec = {**world.state(), "model_saw": state}
            if direct and ans.get("expect"):
                note = f"{note} (expects: {ans['expect']})"
            run.step("decision", skill=skill, target=None if target == "none" else target, note=note, ok=ok, detail=detail,
                     llm_s=round(float(use.get("seconds", 0.0)), 2), tokens=int(use.get("tokens", 0)), motion_s=round(dt, 2),
                     state=rec, images={"overhead": oname, "wrist": wname, **({"depth": dname} if dname else {})})
            st = world.arm.state()
            if not st["enabled"] or (st.get("guard") and float(st.get("guard_time") or 0.0) >= t0 - 1.0):
                run.step("error", note=f"The arm stopped itself ({st.get('guard') or 'not enabled'}).")
                return finish("failed", "arm_fault")
            if skill == "done" and ok:
                if not checked and cfg.judge and not isinstance(llm, L.Scripted):
                    j = _judge(world, run, llm, instruction)
                    if j and j.get("status") == "fail" and not rechecked and world.steps_used < max_steps:
                        rechecked = True               # one more go, with the checker's critique (Voyager's critic loop)
                        said = (f"A checker compared the table before and after and says it is NOT done: "
                                f"{j.get('explanation', '')} {j.get('critique') or ''}").strip()
                        history.append(("check", False, said) if direct else ("check", "none", "", False, said))
                        continue
                declared = True
                break
        d["outcome"]["declared_done"] = declared
        if not checked:
            return _free_verdict(world, run, declared)
        return _verdict(world, run, declared, finish)
    except A.Stop:
        world.arm.hold()
        run.step("info", note="Stopped.")
        return finish("stopped", "stopped")
    except A.ArmError as e:
        world.arm.hold()
        run.step("error", note=f"The arm could not go on: {e}")
        return finish("failed", "arm_fault")
    except Exception as e:                                   # noqa: BLE001 (a bug must not leave a run "running")
        world.arm.hold()
        run.step("error", note=f"Internal error: {type(e).__name__}: {e}")
        return finish("failed", "internal_error")


LIFTED_Z_MM = 120.0


def release_views(run, limit=3):
    """Overhead pictures from DURING the run, one for each time the fingers let go of something: where it was
    put. Before and after alone can't show a there-and-back task (into the basket and out again looks like
    nothing happened). Each decision's pictures were taken before it, so the picture after a release is the
    next decision's; the first one taken with the arm lifted again (tool z >= LIFTED_Z_MM, within two more
    decisions) hides less. -> [(step number of the release, JPEG bytes)], the last `limit`."""
    steps = [s for s in run.d["steps"] if s.get("kind") == "decision"]
    out = []
    for i, s in enumerate(steps):
        if i == 0 or s.get("skill") != "gripper" or not s.get("ok"):
            continue
        was, now = steps[i - 1].get("state"), s.get("state")
        if not (isinstance(was, dict) and isinstance(now, dict) and was.get("holding") and not now.get("holding")):
            continue
        later = range(i + 1, min(i + 4, len(steps)))
        pick = next((j for j in later if ((steps[j - 1].get("state") or {}).get("tool_xyz_mm") or [0, 0, 0])[2]
                     >= LIFTED_Z_MM), later[0] if len(later) else None)
        name = ((steps[pick].get("images") or {}).get("overhead") if pick is not None else None)
        if name and (run.dir / name).exists():
            out.append((s.get("n"), (run.dir / name).read_bytes()))
    return out[-limit:]


def _judge(world, run, llm, instruction):
    """Park the arm, photograph the table and ask the judge. Returns its verdict, or None if it could not be asked."""
    d = run.d
    run.pending("perception", note="checking the result")
    try:
        tool, _ = world.tool()
        if tool[2] < world.cfg.travel_z - 15.0:
            world._go(tool[0], tool[1], world.cfg.travel_z, min_tip_z=tool[2] - 30.0)
        world._look()
    except A.ArmError as e:
        run.step("error", note=f"Could not park the arm to check the result: {e}")
        return None
    after = world.last_frames.get("overhead")
    try:
        wrist = world.wrist.frame() if world.wrist else None
    except Exception:                                        # noqa: BLE001
        wrist = None
    claim = next((s.get("note") for s in reversed(d["steps"]) if s["kind"] == "decision"), "")
    imgs = {"before": getattr(run, "before", None), "after": to_jpeg(after, 80, 1024) if after is not None else None,
            "wrist": to_jpeg(wrist, 80, 768) if wrist is not None else None,
            "during": release_views(run)}
    run.pending("llm", note="the checker is comparing before and after")
    try:
        j, use = J.judge(llm, instruction, Lz.run_log(d), claim, imgs)
    except L.LLMError as e:
        run.step("info", note=f"The checker could not be asked: {e}")
        return None
    d["usage"]["llm_calls"] += 1
    d["usage"]["tokens"] += int(use.get("tokens", 0))
    d["usage"]["llm_s"] = round(d["usage"]["llm_s"] + float(use.get("seconds", 0.0)), 2)
    d["judge"] = j
    n = len(d["steps"])
    names = {"overhead": run.image(f"{n:03d}-after.jpg", after, 1024)[0], "wrist": run.image(f"{n:03d}-wrist.jpg", wrist, 768)[0]}
    run.step("perception", note=f"Checker: {j.get('status')}. {j.get('explanation', '')}",
             detail=f"Before: {j.get('before', '')} After: {j.get('after', '')}" + (f" Fix: {j['critique']}" if j.get("critique") else ""),
             tokens=int(use.get("tokens", 0)), llm_s=round(float(use.get("seconds", 0.0)), 2), images=names)
    return j


def _arm_connected(world, run, stop_event, wait_s=120.0):
    """True when the arm answers with fresh feedback. If it does not (a USB or CAN dropout: the driver restarts
    itself), wait for it rather than spend the model's turns on moves that cannot happen."""
    def fresh():
        try:
            return world.arm.state()["fresh"]
        except A.ArmError:
            return False
    if fresh():
        return True
    run.step("info", note="The arm's connection dropped (USB or CAN); waiting for the driver to come back.")
    t_end = time.time() + wait_s
    while time.time() < t_end and not stop_event.is_set():
        time.sleep(2.0)
        if fresh():
            time.sleep(2.0)                                  # let the driver settle after its restart
            run.step("info", note="The arm is back.")
            return True
    return False


def _free_verdict(world, run, declared):
    """Any instruction but the bottle cycle: the checker's verdict if there is one, else the model's own word.
    The model's last note is its answer (for questions)."""
    d = run.d
    last = next((s for s in reversed(d["steps"]) if s["kind"] == "decision"), {})
    ann, wrist = world.frames(grid=d.get("mode") == "direct")
    n = len(d["steps"])
    names = {"overhead": run.image(f"{n:03d}-overhead.jpg", ann, 1024)[0], "wrist": run.image(f"{n:03d}-wrist.jpg", wrist, 768)[0]}
    d["outcome"]["checks"] = {}
    j = d.get("judge")
    if declared and j:
        ok = j.get("status") == "success"
        d["outcome"].update(success=ok, reason="" if ok else ("impossible" if j.get("status") == "impossible"
                                                              else (j.get("failure_kind") or "judged_failed")))
        d["outcome"]["checks"] = {"judge": j.get("status"), "judge_confidence": j.get("confidence")}
        run.step("verdict", ok=ok, note=("Done, and the checker agrees: " if ok else "The checker says it is not done: ")
                 + (j.get("explanation") or ""), detail=last.get("note") or "", images=names)
    elif declared:
        d["outcome"].update(success=None, reason="not_checked")
        run.step("verdict", ok=None, note="The model says it is done. Free instructions are not checked automatically.",
                 detail=last.get("note") or "", images=names)
    else:
        d["outcome"].update(success=False, reason="step_limit")
        run.step("verdict", ok=False, note="The step budget ran out before the model said done.", images=names)
    d["state"], d["ended"], d["pending"] = "done", time.time(), None
    run.save()
    return d


def _verdict(world, run, declared, finish):
    """Check the result with the cameras and by touch."""
    d, cfg = run.d, world.cfg
    run.pending("perception", note="checking the result")
    t0 = time.time()
    checks = d["outcome"]["checks"]
    checks["entered_basket"] = bool(world.entered_basket)
    note = ""
    if world.holding:
        note = "The bottle is still in the gripper."
    else:
        tool, _ = world.tool()
        if tool[2] < cfg.travel_z - 15.0:
            world._go(tool[0], tool[1], cfg.travel_z, min_tip_z=tool[2] - 30.0)
        note = world._look()
        b = world.bottle
        if b and b["where"] == "table" and b.get("xy") and world.start_xy:
            checks["returned_mm"] = round(math.dist(b["xy"], world.start_xy), 1)
            checks["upright"] = world.feel_upright() if cfg.feel else b.get("upright")
    d["usage"]["motion_s"] = round(d["usage"]["motion_s"] + time.time() - t0, 2)
    b = world.bottle or {}
    if not declared:
        reason = "step_limit"
    elif world.holding:
        reason = "still_holding"
    elif not checks["entered_basket"]:
        reason = "never_entered"
    elif b.get("where") == "basket":
        reason = "left_in_basket"
    elif b.get("where") != "table" or checks["returned_mm"] is None:
        reason = "bottle_lost"
    elif checks["upright"] is False:
        reason = "tipped_over"
    elif checks["returned_mm"] > cfg.return_tol:
        reason = "not_returned"
    elif checks["upright"] is None:
        reason = "upright_unknown"
    else:
        reason = ""
    ok = reason == ""
    limit = cfg.max_steps_direct if d.get("mode") == "direct" else cfg.max_steps
    say = {"": "The bottle went into the basket and is back upright where it started.",
           "step_limit": f"The step budget ({limit}) ran out before the model said done.",
           "still_holding": "The model said done with the bottle still in the gripper.",
           "never_entered": "The bottle never went into the basket.",
           "left_in_basket": "The bottle is still in the basket.",
           "bottle_lost": "The bottle was not found on the table at the end.",
           "tipped_over": "The bottle is back on the table but not standing.",
           "not_returned": f"The bottle stands {checks['returned_mm']} mm from where it started (allowed: {cfg.return_tol:.0f} mm).",
           "upright_unknown": "The bottle is back, but whether it stands could not be checked."}[reason]
    ann, wrist = world.frames()
    n = len(d["steps"])
    names = {"overhead": run.image(f"{n:03d}-overhead.jpg", ann, 1024)[0], "wrist": run.image(f"{n:03d}-wrist.jpg", wrist, 768)[0]}
    d["outcome"]["success"], d["outcome"]["reason"] = ok, reason
    run.step("verdict", ok=ok, note=say, detail=note, state=world.state(), images=names)
    d["state"], d["ended"], d["pending"] = "done", time.time(), None
    run.save()
    return d


def tidy(world, stop_event, log, limit=30):
    """Put the scene back with no model: the scripted policy takes the bottle out of the basket (or sets a
    held one down). Returns True when the bottle stands on the table afterwards. A fallen bottle is left."""
    try:
        tool, _ = world.tool()
        if tool[2] < world.cfg.travel_z - 15.0:
            world._go(tool[0], tool[1], world.cfg.travel_z, min_tip_z=tool[2] - 30.0)
        world._look()
        if world.bottle and world.bottle["where"] == "table":
            return True
        if not world.bottle or world.bottle["where"] != "basket" or not world.basket:
            return False
        world.entered_basket = True
        world.basket_spot = world.basket_spot or (world.bottle["xy"][0], world.bottle["xy"][1], world.cfg.basket_floor + world.cfg.grasp_z)
        world.start_xy = world.start_xy or [300.0, 180.0]
        for _ in range(limit):
            if stop_event.is_set():
                return False
            skill, target, _ = T.oracle(world.state())
            if skill == "done":
                break
            log(f"tidying: {skill} {target if target != 'none' else ''}")
            world.do(skill, target)
        return bool(world.bottle and world.bottle["where"] == "table")
    except (T.Refused, A.ArmError) as e:
        log(f"tidying stopped: {e}")
        world.arm.hold()
        return False


class Bench:
    """N runs per model, interleaved, each from a clean start. Pauses when the scene needs a hand."""

    def __init__(self, models, runs_per_model, instruction, mode="skills", root=None, lessons=False):  # noqa: D107
        self.root = root or RUNS
        self.dir = self.root / f"bench-{time.strftime('%Y%m%d-%H%M%S')}"
        self.dir.mkdir(parents=True, exist_ok=True)
        self.resume_event = threading.Event()
        self.d = {"id": self.dir.name, "state": "running", "instruction": instruction, "mode": mode, "models": list(models),
                  "runs_per_model": int(runs_per_model), "lessons": bool(lessons), "started": time.time(), "ended": None, "current": None,
                  "message": "", "results": [], "run_ids": [], "skipped": {}}
        self.runs = []
        self._summarise()

    def save(self):
        _write(self.dir / "bench.json", self.d)

    def _summarise(self):
        out = []
        for m in self.d["models"]:
            rs = [r for r in self.runs if r["model"] == m and r["state"] in ("done", "failed")]
            ok = [r for r in rs if r["outcome"]["success"]]
            fails = {}
            for r in rs:
                if not r["outcome"]["success"]:
                    fails[r["outcome"]["reason"] or "unknown"] = fails.get(r["outcome"]["reason"] or "unknown", 0) + 1
            n = len(rs)
            out.append({"model": m, "runs": n, "successes": len(ok), "rate": round(len(ok) / n, 3) if n else None,
                        "mean_steps": round(sum(r["usage"]["llm_calls"] for r in rs) / n, 1) if n else None,
                        "mean_s": round(sum((r["ended"] or r["started"]) - r["started"] for r in rs) / n, 1) if n else None,
                        "tokens": sum(r["usage"]["tokens"] for r in rs), "failures": fails})
        self.d["results"] = out
        self.save()

    def run(self, make_world, context, stop_event, on_run_update, log):
        d = self.d
        order = [(m, i + 1) for i in range(d["runs_per_model"]) for m in d["models"]]
        need_check = True
        # The bottle cycle starts (and, done right, ends) with the bottle standing. Another instruction may
        # leave it lying ("standing or lying are both fine"), so it starts from the bottle anywhere on the
        # table, standing or not; each run records which (start_upright).
        upright_start = is_task(d["instruction"])
        try:
            for model, index in order:
                if stop_event.is_set():
                    break
                if model in d["skipped"]:
                    continue
                # a clean start: the bottle standing on the table
                while True:
                    world = make_world()
                    if not need_check:
                        ready = True
                    elif world.cfg.auto_tidy:
                        ready = tidy(world, stop_event, log)
                    else:                                    # just look: is the bottle (standing) on the table?
                        tool, _ = world.tool()
                        if tool[2] < world.cfg.travel_z - 15.0:
                            world._go(tool[0], tool[1], world.cfg.travel_z, min_tip_z=tool[2] - 30.0)
                        world._look()
                        ready = bool(world.bottle and world.bottle["where"] == "table"
                                     and (world.bottle.get("upright") is not False or not upright_start))
                    if ready and need_check and world.cfg.feel and upright_start:
                        ready = world.feel_upright() is True
                    if ready or stop_event.is_set():
                        break
                    d["state"], d["message"] = "paused", (
                        "The scene needs a hand: " + ("stand the bottle upright on the table" if upright_start else
                                                      "put the bottle on the table") + ", clear of the basket, then press Resume.")
                    self.save()
                    self.resume_event.clear()
                    while not self.resume_event.wait(0.5):
                        if stop_event.is_set():
                            break
                    d["state"], d["message"] = "running", ""
                if stop_event.is_set():
                    break
                run = Run(model, d["instruction"], d["mode"], on_update=on_run_update, root=self.root)
                d["current"] = {"model": model, "index": index, "run_id": run.d["id"]}
                d["run_ids"].append(run.d["id"])
                self.save()
                try:
                    llm = make_llm(model)
                except L.LLMError as e:
                    d["skipped"][model] = str(e)
                    continue
                r = run_cycle(make_world(), llm, d["instruction"], context, run, stop_event, mode=d["mode"],
                              lessons_on=d["lessons"])
                self.runs.append(r)
                self._summarise()
                need_check = not r["outcome"]["success"]
                if r["outcome"]["reason"] == "model_unavailable":
                    d["skipped"][model] = next((s["note"] for s in reversed(r["steps"]) if s["kind"] == "error"), "unavailable")
                    d["message"] = f"{model} is unavailable and was skipped: {d['skipped'][model]}"
                if r["outcome"]["reason"] in ("arm_fault", "internal_error"):
                    d["state"], d["message"] = "stopped", f"Stopped after run {r['id']}: {r['outcome']['reason']}."
                    break
        finally:
            d["current"], d["ended"] = None, time.time()
            if d["state"] == "running":
                d["state"] = "stopped" if stop_event.is_set() else "done"
            self._summarise()
        return d
