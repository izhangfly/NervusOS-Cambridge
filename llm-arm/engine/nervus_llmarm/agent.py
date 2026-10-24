"""One request from the user, run as a job: ground it, then decide skill by skill.

The design follows github.com/RobotKitAI/piper-astra-jev (Apache-2.0), tailored to this arm and its driver:

1. Ground. The model reads the request ("put the cube in the tray") and points at the object and the place
   in the scene image. The scene-camera calibration turns those pixels into table positions. This is done
   while the arm is out of the way, and remembered: once the gripper is close, the fingers hide the object.
2. Decide. At every step the model sees the arm's state as plain flags (over_target, object_held, ...),
   the camera images, the editable arm context and its own recent decisions, and picks ONE skill by name.
3. Govern. A skill whose preconditions don't hold is refused (the model is asked again). The skill itself
   is a bounded motion written here: straight lines inside the workspace box, above the table, at a capped
   speed, streamed to the driver as client "llm". The model never sends a coordinate to the arm.
4. Confirm by touch. Whether a grasp worked comes from the gripper (the fingers stopped part way), not the image.

Stopping is local only: STOP, E-STOP, or the app going quiet end the job at once.
"""
from __future__ import annotations

import math
import threading
import time
from pathlib import Path

from . import config as C
from .driver import DriverError

SKILLS: dict[str, str] = {
    "approach_target": "Move the open gripper above the target object. Only when over_target is false and "
                       "object_placed is false.",
    "descend_to_target": "Lower the open gripper onto the target, ready to grasp. Only when over_target is true, "
                         "at_grasp_height is false, gripper_closed is false and object_placed is false.",
    "close_gripper": "Close the gripper to grasp what is between the fingers. Only when over_target is true, "
                     "at_grasp_height is true, gripper_closed is false and object_placed is false.",
    "lift": "Raise the held object to the carry height. Only when object_held is true, at_carry_height is false "
            "and over_place_target is false.",
    "move_over_place": "Carry the held object above the place target. Only when object_held is true, "
                       "at_carry_height is true and over_place_target is false.",
    "lower_to_place": "Lower the held object onto or into the place target. Only when object_held is true, "
                      "over_place_target is true and at_release_height is false.",
    "open_gripper": "Open the gripper to let go. Only when object_held is true, over_place_target is true and "
                    "at_release_height is true; or when gripper_closed is true and object_held is false, after "
                    "a missed grasp.",
    "retreat": "Raise the open gripper clear of the scene. Only when at_clear_height is false and object_held "
               "is false.",
    "done": "Stop. When task_complete is true, or when the task can't be done (say why in the note).",
}
MOTION = set(SKILLS) - {"done"}

GROUND_SCHEMA = {
    "type": "object", "additionalProperties": False,
    "properties": {
        "found": {"type": "boolean", "description": "true if both the object and the place are visible"},
        "target_label": {"type": "string"}, "target_u": {"type": "number"}, "target_v": {"type": "number"},
        "place_label": {"type": "string"}, "place_u": {"type": "number"}, "place_v": {"type": "number"},
        "place_kind": {"type": "string", "enum": ["table", "on_object", "in_container"]},
        "note": {"type": "string"},
    },
    "required": ["found", "target_label", "target_u", "target_v", "place_label", "place_u", "place_v",
                 "place_kind", "note"],
}
GROUND_PROMPT = """{context}

First, find what the task is about in the scene camera image ({w}x{h} pixels, a labelled grid every 50 px).
Give the pixel at the centre of the object to move, where it rests on the table, and the pixel of the place
to put it: the centre of the spot on the table, of the object to stack it on, or of the container's inside.
If either one is not visible, set found to false and say what is missing in the note."""


def decision_schema() -> dict:
    return {"type": "object", "additionalProperties": False,
            "properties": {"skill": {"type": "string", "enum": list(SKILLS)}, "note": {"type": "string"}},
            "required": ["skill", "note"]}


DEFAULT_CONTEXT = (Path(__file__).with_name("context.md")).read_text()


class Stopped(Exception):
    pass


class ToolError(Exception):
    pass


class Job:
    def __init__(self, text: str):
        self.id = f"{int(time.time() * 1000) % 10**9}"
        self.text, self.state = text, "running"         # running | waiting | done | stopped | failed
        self.log: list[dict] = []
        self.pending: dict | None = None                # a step waiting for Approve
        self.decision = threading.Event()
        self.approved = False
        self.stop = threading.Event()
        self.lock = threading.Lock()

    def add(self, kind: str, text: str, data=None):
        with self.lock:
            self.log.append({"t": round(time.time(), 2), "kind": kind, "text": text, "data": data})

    def view(self) -> dict:
        with self.lock:
            return {"id": self.id, "text": self.text, "state": self.state, "pending": self.pending,
                    "log": list(self.log)}


class Task:
    """Where the object and the place are, and what has happened to them."""

    def __init__(self, target_xy, place_xy, place_kind, labels):
        self.target_xy, self.place_xy, self.place_kind, self.labels = target_xy, place_xy, place_kind, labels
        self.released = False
        self.missed = 0


class Agent:
    def __init__(self, arm, frames, calib, settings, model_factory, context_path: Path | None = None,
                 sim: bool = False):
        self.arm, self.frames, self.calib, self.settings = arm, frames, calib, settings
        self.model_factory = model_factory           # () -> model with .ask(), or raises with the key message
        self.context_path = context_path
        self.sim = sim
        self.job: Job | None = None
        self.task: Task | None = None
        self.grip_cmd = 95.0                         # last commanded opening, mm
        self.last_app_beat = 0.0
        self.lock = threading.Lock()

    # ---- the editable context -------------------------------------------------------------------------------
    def context(self) -> str:
        if self.context_path and self.context_path.exists():
            try:
                return self.context_path.read_text()
            except OSError:
                pass
        return DEFAULT_CONTEXT

    def set_context(self, text: str | None):
        if not self.context_path:
            raise ValueError("no context file in this mode")
        if text is None:
            self.context_path.unlink(missing_ok=True)
        else:
            if "{task}" not in text:
                raise ValueError("the context must keep the {task} placeholder")
            self.context_path.parent.mkdir(parents=True, exist_ok=True)
            self.context_path.write_text(text[:20000])

    # ---- heights, relative to the calibrated table -----------------------------------------------------------
    def heights(self) -> dict:
        tz = self.calib.table_z_mm
        oh = float(self.settings["object_height_mm"])
        travel = float(self.settings["travel_height_mm"])
        t = self.task
        kind = t.place_kind if t else "table"
        if kind == "on_object":
            release = oh * 1.5 + 5.0
        elif kind == "in_container":
            release = float(self.settings["container_rim_mm"]) + 15.0
        else:
            release = oh * 0.5 + 5.0
        carry = max(travel, release + oh)
        return {"grasp": tz + oh * 0.5, "carry": tz + carry, "release": tz + release, "clear": tz + travel}

    def limits(self) -> dict:
        box = dict(C.WORKSPACE_MM)
        floor = (self.calib.table_z_mm + C.TABLE_CLEARANCE_MM) if self.calib.ready else C.UNCALIBRATED_Z_FLOOR_MM
        return {"x": box["x"], "y": box["y"], "z": (floor, box["z"][1])}

    # ---- what the model sees --------------------------------------------------------------------------------
    def observe(self) -> dict:
        s = self.arm.state()
        pose = s.get("pose") or [0.0] * 6
        t, h = self.task, self.heights()
        z = pose[2]
        closed = self.grip_cmd < 5.0
        held = closed and (s.get("gripper_mm") or 0.0) > 3.0
        tgt = None if (t is None or t.released) else t.target_xy
        place = None if t is None else t.place_xy
        clear = z > h["clear"] - 10.0
        return {
            "tool_xyz_mm": [round(v) for v in pose[:3]],
            "target_xy_mm": None if tgt is None else [round(v) for v in tgt],
            "place_target_xy_mm": None if place is None else [round(v) for v in place],
            "over_target": bool(tgt is not None and math.dist(pose[:2], tgt) < 15.0),
            "over_place_target": bool(place is not None and math.dist(pose[:2], place) < 20.0),
            "at_grasp_height": bool(tgt is not None and abs(z - h["grasp"]) < 8.0),
            "at_carry_height": bool(z > h["carry"] - 10.0),
            "at_release_height": bool(z < h["release"] + 10.0),
            "at_clear_height": bool(clear),
            "gripper_closed": closed,
            "gripper_opening_mm": round(s.get("gripper_mm") or 0.0, 1),
            "object_held": bool(held),
            "object_placed": bool(t is not None and t.released),
            "task_complete": bool(t is not None and t.released and not held and clear),
        }

    @staticmethod
    def precondition(skill: str, s: dict) -> bool:
        held, placed, closed = s["object_held"], s["object_placed"], s["gripper_closed"]
        rules = {
            "approach_target": s["target_xy_mm"] is not None and not held and not placed,
            "descend_to_target": s["target_xy_mm"] is not None and s["over_target"] and not held and not closed
                                 and not placed,
            "close_gripper": s["over_target"] and s["at_grasp_height"] and not closed and not placed,
            "lift": held,
            "move_over_place": held and s["place_target_xy_mm"] is not None,
            "lower_to_place": held and s["over_place_target"],
            "open_gripper": closed,
            "retreat": not held,
            "done": True,
        }
        return rules.get(skill, False)

    def target(self, skill: str, pose: list) -> tuple:
        """(x, y, z, gripper mm or None) for a skill; z None = stay."""
        t, h = self.task, self.heights()
        x, y, z = pose[:3]
        if skill == "approach_target":
            return (*t.target_xy, h["clear"], float(self.settings["open_width_mm"]))
        if skill == "descend_to_target":
            return (*t.target_xy, h["grasp"], None)
        if skill == "close_gripper":
            return (x, y, None, 0.0)
        if skill == "lift":
            return (x, y, h["carry"], None)
        if skill == "move_over_place":
            return (*t.place_xy, h["carry"], None)
        if skill == "lower_to_place":
            return (*t.place_xy, h["release"], None)
        if skill == "open_gripper":
            return (x, y, None, float(self.settings["open_width_mm"]))
        if skill == "retreat":
            return (x, y, max(h["clear"], z), None)
        return (x, y, None, None)

    def after(self, skill: str, before: dict):
        t = self.task
        if skill == "open_gripper":
            if not before["object_held"]:
                t.missed += 1           # nothing to let go: the grasp missed or the part slipped out
            elif before["over_place_target"]:
                t.released = True

    # ---- motion ---------------------------------------------------------------------------------------------
    def _check(self, job: Job):
        if job.stop.is_set():
            raise Stopped("stopped")
        if time.time() - self.last_app_beat > C.HEARTBEAT_TIMEOUT_S:
            raise Stopped("the app went quiet, so the arm stopped")

    def _pose(self) -> list:
        s = self.arm.state()
        if not s.get("reachable") or not s.get("pose"):
            raise ToolError("the arm is not reachable")
        if not s.get("enabled"):
            self.arm.enable()
            s = self.arm.state()
        if not s.get("fresh", True):
            raise ToolError("the arm's feedback is stale")
        return list(s["pose"])

    def _validate(self, x, y, z):
        lim = self.limits()
        for name, v in (("x", x), ("y", y), ("z", z)):
            if not isinstance(v, (int, float)) or not math.isfinite(v):
                raise ToolError(f"{name} must be a number")
            lo, hi = lim[name]
            if not lo - 0.5 <= v <= hi + 0.5:
                raise ToolError(f"{name}={v:.0f} mm is outside {lo:.0f}..{hi:.0f}")

    def move_line(self, job: Job, x, y, z):
        self._validate(x, y, z)
        start = self._pose()
        goal = [float(x), float(y), float(z)]
        dist = math.dist(start[:3], goal)
        dur = max(dist / float(self.settings["speed_mm_s"]), 0.1)
        t0 = time.time()
        deadline = t0 + dur + 6.0
        while True:
            self._check(job)
            w = min(1.0, (time.time() - t0) / dur)
            p = [start[i] + (goal[i] - start[i]) * w for i in range(3)]
            try:
                self.arm.servo(p + start[3:6])
            except DriverError as e:
                raise ToolError(str(e))
            if w >= 1.0:
                now = self._pose()
                if math.dist(now[:3], goal) < 4.0:
                    return now
                if time.time() > deadline:
                    raise ToolError(f"the arm stopped {math.dist(now[:3], goal):.0f} mm short of the target")
            time.sleep(0.1)

    def grip(self, job: Job, width: float):
        self._check(job)
        try:
            self.arm.gripper(width, int(self.settings["grip_effort"]))
        except DriverError as e:
            raise ToolError(str(e))
        self.grip_cmd = float(width)
        t_end = time.time() + (0.3 if self.sim else 1.5)      # let the jaws travel
        while time.time() < t_end:
            self._check(job)
            time.sleep(0.05)

    def run_skill(self, job: Job, skill: str):
        pose = self._pose()
        x, y, z, g = self.target(skill, pose)
        if g is not None and g > self.grip_cmd:
            self.grip(job, g)                   # open before moving
        if z is not None:
            if skill in ("approach_target", "move_over_place") and pose[2] < z - 10:
                self.move_line(job, pose[0], pose[1], z)        # up first, then across
            self.move_line(job, x, y, z)
        if g is not None and g <= self.grip_cmd and g != self.grip_cmd:
            self.grip(job, g)

    # ---- the job --------------------------------------------------------------------------------------------
    def _images(self, grid: bool = False) -> dict:
        from .vision import with_grid
        out = {}
        if not self.settings["images"]:
            return out
        for role in ("scene", "wrist"):
            jpg, _, _ = self.frames.get(role)
            if jpg:
                out[role] = with_grid(jpg) if (grid and role == "scene") else jpg
        return out

    def start(self, text: str) -> Job:
        with self.lock:
            if self.job and self.job.state in ("running", "waiting"):
                raise ValueError("a request is still running; stop it first")
            job = self.job = Job(text)
        self.last_app_beat = time.time()
        threading.Thread(target=self._run, args=(job,), daemon=True, name="job").start()
        return job

    def _approve(self, job: Job, skill: str, note: str) -> bool:
        if not self.settings["confirm_moves"] or skill not in MOTION:
            return True
        job.pending, job.approved = {"skill": skill, "note": note}, False
        job.decision.clear()
        job.state = "waiting"
        while not job.decision.wait(0.2):
            if job.stop.is_set():
                raise Stopped("stopped")
        job.pending, job.state = None, "running"
        self.last_app_beat = time.time()
        return job.approved

    def ground(self, job: Job, model, context: str):
        from .vision import decode
        if not self.calib.ready:
            raise ToolError("calibrate the scene camera and the table height first (Calibrate tab)")
        jpg, _, _ = self.frames.get("scene")
        if jpg is None:
            raise ToolError("no scene camera picture: the scene camera is needed to find the object")
        h, w = decode(jpg).shape[:2]
        a = model.ask(GROUND_PROMPT.format(context=context, w=w, h=h), self._images(grid=True), GROUND_SCHEMA,
                      "grounding")
        job.add("ground", a.get("note") or "", a)
        if not a.get("found"):
            raise ToolError("the model could not find it: " + (a.get("note") or "no reason given"))
        for k in ("target_u", "target_v", "place_u", "place_v"):
            v = a.get(k)
            if not isinstance(v, (int, float)) or not 0 <= v <= max(w, h):
                raise ToolError(f"the model gave an impossible pixel ({k}={v})")
        target = self.calib.to_table(a["target_u"], a["target_v"])
        place = self.calib.to_table(a["place_u"], a["place_v"])
        lim = self.limits()
        for name, (x, y) in (("object", target), ("place", place)):
            if not (lim["x"][0] <= x <= lim["x"][1] and lim["y"][0] <= y <= lim["y"][1]):
                raise ToolError(f"the {name} is at x {x:.0f}, y {y:.0f} mm, outside the arm's workspace")
        kind = a.get("place_kind") if a.get("place_kind") in ("table", "on_object", "in_container") else "table"
        self.task = Task(target, place, kind, {"target": a.get("target_label"), "place": a.get("place_label"),
                                               "target_px": [a["target_u"], a["target_v"]],
                                               "place_px": [a["place_u"], a["place_v"]]})
        job.add("info", f"{a.get('target_label')} at x {target[0]:.0f}, y {target[1]:.0f} mm; "
                        f"{a.get('place_label')} ({kind}) at x {place[0]:.0f}, y {place[1]:.0f} mm")

    def _run(self, job: Job):
        job.add("user", job.text)
        self.task, history = None, []
        try:
            model = self.model_factory()
            self.frames.now()
            time.sleep(0.4)
            context = self.context().replace("{task}", job.text)
            self.ground(job, model, context)
            for _ in range(C.MAX_STEPS):
                self._check(job)
                state = self.observe()
                text = (context + "\n\n## Skills\n" + "\n".join(f"- {k}: {v}" for k, v in SKILLS.items())
                        + "\n\n## State\n" + "\n".join(f"- {k}: {v}" for k, v in state.items())
                        + "\n\n## Recent decisions\n" + ("\n".join(history[-6:]) or "(none yet)")
                        + "\n\nPick one skill.")
                d = model.ask(text, self._images(), decision_schema(), "decision")
                skill, note = str(d.get("skill")), str(d.get("note") or "")
                history.append(f"{skill}: {note}")
                job.add("decision", skill, {"note": note, "state": state})
                if skill not in SKILLS:
                    job.add("refused", f"{skill} is not a skill")
                    continue
                if skill == "done":
                    break
                if not self.precondition(skill, state):
                    job.add("refused", f"the conditions for {skill} don't hold")
                    history.append(f"(refused {skill}: its conditions don't hold)")
                    continue
                if not self._approve(job, skill, note):
                    job.add("refused", f"you rejected {skill}")
                    history.append(f"(the user rejected {skill})")
                    continue
                try:
                    self.run_skill(job, skill)
                    self.after(skill, state)
                except ToolError as e:
                    job.add("error", str(e))
                    history.append(f"({skill} failed: {e})")
                if self.task.missed >= 2:
                    raise ToolError(f"the gripper held nothing after {self.task.missed} grasps")
            else:
                job.add("info", f"stopped after {C.MAX_STEPS} decisions")
            job.state = "done"
        except Stopped as e:
            job.add("info", str(e))
            job.state = "stopped"
        except Exception as e:
            job.add("error", str(e))
            job.state = "failed"
        finally:
            try:
                self.arm.release()
            except DriverError:
                pass

    def approve(self, ok: bool):
        j = self.job
        if not j or not j.pending:
            raise ValueError("nothing is waiting for approval")
        j.approved = bool(ok)
        j.decision.set()

    def stop(self):
        j = self.job
        if j:
            j.stop.set()
            j.decision.set()
        try:
            self.arm.stop()
        except DriverError:
            pass
