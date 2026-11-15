#!/usr/bin/env python3
"""Drive one cycle on the real arm, recorded like any run.
    drive.py                       the scripted policy (no model, no tokens)
    drive.py --model gpt-5.6-sol   a model
    drive.py --steps 8             stop after 8 decisions (to try the first half)
    drive.py --sim                 on the pretend arm
"""
import argparse
import json
import sys
import threading
import time
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from gptarm import calib, direct, runner, sim, task
from gptarm.arm import Arm
from gptarm.cameras import Overhead, Wrist

ap = argparse.ArgumentParser()
ap.add_argument("--model", default="scripted")
ap.add_argument("--steps", type=int, default=0)
ap.add_argument("--speed", type=int, default=0)
ap.add_argument("--sim", action="store_true")
ap.add_argument("--instruction", default=task.DEFAULT_INSTRUCTION)
ap.add_argument("--mode", choices=("skills", "direct"), default="skills")
a = ap.parse_args()
cfg = task.Config.load()
if a.steps:
    cfg.max_steps = cfg.max_steps_direct = a.steps
if a.speed:
    cfg.speed = a.speed
stop = threading.Event()
wrist = None
if a.sim:
    t, cam = sim.Table(), sim.camera()
    world = task.World(sim.SimArm(t, stop_event=stop), sim.SimOverhead(t, cam), sim.SimWrist(), cam, cfg, perceive=sim.perceive(t))
else:
    cam = calib.Camera.load()
    if cam is None:
        sys.exit("no calibration: run python -m gptarm.calib first")
    wrist = Wrist()
    world = task.World(Arm(speed=cfg.speed, stop_event=stop), Overhead(), wrist, cam, cfg, log=print)
last = [0]


def show(d):
    for s in d["steps"][last[0]:]:
        tag = f"{s.get('skill', '')}{'(' + s['target'] + ')' if s.get('target') else ''}"
        print(f"{s['n']:3d} {s['kind']:10s} {tag:34s} {'' if s.get('ok', True) else 'NOT DONE '}{s.get('detail') or s.get('note') or ''}"[:260], flush=True)
        if s["kind"] == "decision" and s.get("note"):
            print(f"      model: {s['note']}"[:300], f"[{s.get('llm_s')} s, {s.get('tokens')} tok]", flush=True)
    last[0] = len(d["steps"])


run = runner.Run(a.model, a.instruction, a.mode, on_update=show)
context, _ = (direct.load_context() if a.mode == "direct" else task.load_context())
try:
    d = runner.run_cycle(world, runner.make_llm(a.model), a.instruction, context, run, stop, mode=a.mode)
except KeyboardInterrupt:
    stop.set()
    raise
finally:
    world.arm.hold()
    if wrist:
        wrist.close()
        time.sleep(0.5)
print(json.dumps({"id": d["id"], "state": d["state"], "outcome": d["outcome"], "usage": d["usage"]}))
