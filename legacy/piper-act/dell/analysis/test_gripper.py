"""Offline test of the new clamp + gripper latch + ensembling against simple simulated grippers."""
import sys
from pathlib import Path
from types import SimpleNamespace
import numpy as np

sys.path.insert(0, r"C:\Users\zhy10\Shared\Piper Arm\deploy")
import piper_policy_portal as portal

args = SimpleNamespace(dataset_root=r"C:\Users\zhy10\Shared\Piper Arm\datasets\piper_pick_place", mock_arm=True)
ctl = portal.Controller.__new__(portal.Controller)
ctl.args, ctl.params = args, dict(portal.DEFAULT_PARAMS)
ctl.envelope = {j: (-100.0, 100.0) for j in portal.JOINTS}; ctl.envelope["gripper"] = (0.0, 100.0)


class Gripper:
    """Dead time 8 steps, then moves 12 units/step toward the command; blocked by a cube of width `cube`."""
    def __init__(self, cube=None, start=0.0):
        self.pos, self.cube, self.q = start, cube, [start] * 8
    def step(self, cmd):
        self.q.append(cmd); c = self.q.pop(0)
        self.pos += max(-12, min(12, c - self.pos))
        if self.cube is not None and self.pos < self.cube:
            self.pos = self.cube
        return self.pos


def sim(plan, cube=None, start=0.0, label=""):
    gr, g, last, out = Gripper(cube, start), {}, None, []
    for k, pg in enumerate(plan):
        meas = gr.pos
        tgt = ctl._clamp({**{j: 0.0 for j in portal.JOINTS[:6]}, "gripper": pg}, {j: 0.0 for j in portal.JOINTS[:6]} | {"gripper": meas}, last)
        tgt["gripper"], mode = ctl._gripper(tgt["gripper"], meas, g)
        last = tgt
        gr.step(tgt["gripper"])
        out.append((pg, round(tgt["gripper"], 1), round(meas, 1), mode))
    return out


# 1) open from closed: plan jumps to 100 -> how fast does the gripper get > 90 ?
o = sim([100.0] * 40)
t90 = next(i for i, r in enumerate(o) if r[2] > 90)
print(f"open 0->100: measured >90 after {t90} steps ({t90/15:.2f} s)  [old code: stalls, 1.4 s to never]")

# 2) grasp a cube (width 77): plan closes to 80, then flickers open/closed for single plans, then really releases
plan = [100.0] * 10 + [80.0] * 30 + [100.0] * 3 + [80.0] * 10 + [0.0] * 10 + [80.0] * 10 + [100.0] * 20
o = sim(plan, cube=77.0, start=100.0)
modes = [r[3] for r in o]
first_hold = modes.index("hold")
print(f"grasp: first 'hold' at step {first_hold}; commanded while holding min {min(r[1] for r in o if r[3]=='hold'):.1f} "
      f"(cube 77, squeeze {ctl.params['gripper_squeeze']})")
flick = o[40:43]
print("  3-step 'open' flicker during hold ->", [(r[0], r[1], r[3]) for r in flick])
zero = o[53:56]
print("  stale 'close to 0' during hold    ->", [(r[0], r[1], r[3]) for r in zero])
rel = [i for i, r in enumerate(o) if r[3] == "release"]
print(f"  real release requested from step 73 -> released at step {rel[0] if rel else None}; final measured {o[-1][2]}")

# 3) free close with no cube (missed grasp): must reach the plan, never latch
o = sim([100.0] * 5 + [80.0] * 30 + [0.0] * 30, cube=None, start=100.0)
print(f"missed grasp: modes {sorted(set(r[3] for r in o))}; final measured {o[-1][2]}")

# 4) ensembling: two plans offset by 2 units -> mean
pl = portal.Planner.__new__(portal.Planner)
pl.plan, pl.history, pl.starved, pl.ens_n = {}, portal.deque(maxlen=3), 0, 1
a = np.zeros((30, 7)); b = np.ones((30, 7)) * 2; b[:, 6] = 100
pl.history.extend([(0, a), (10, b)])
pl.plan = {12: b[2]}
v = pl.take(12, 2)
print(f"ensemble at k=12: joints {v[:6].tolist()} (expect 1.0), gripper {v[6]} (expect 100, newest plan), n={pl.ens_n}")
