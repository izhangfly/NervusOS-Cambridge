"""One request from the user, run as a job: the model proposes tool calls, the executor checks and runs them.

The model never moves the arm itself. Every tool call is validated here (workspace box, table floor, step size,
known objects) and motions run as straight lines at a capped speed, streamed to the driver at 10 Hz. Stopping is
local only: there is no stop tool for the model, and STOP, E-STOP or a silent app end the job at once.
"""
from __future__ import annotations

import base64
import json
import math
import threading
import time

from . import config as C
from .driver import DriverError

SYSTEM_PROMPT = """You control a 6-axis robot arm (AgileX PiPER) with a parallel gripper, over a table.
Units: millimetres and degrees, in the arm's base frame: +x forward from the base, +y to the arm's left, +z up.
You see two cameras: "scene" looks at the table from the side or above, "wrist" looks out of the gripper.
Workspace: x {x0}..{x1}, y {y0}..{y1}, z {z0}..{z1}. Objects are listed by get_scene with ids such as "red-1".
Plan with the tools; one tool call per step is fine. Prefer pick and place_on/place_at over raw moves.
If the request is unclear, an object isn't visible, or a step looks unsafe, ask instead of moving.
Reply briefly in plain text when you are done."""

TOOLS = [
    {"type": "function", "function": {"name": "get_scene", "description":
        "The arm's pose and gripper, and the objects the cameras see (ids, colour, table position in mm).",
        "parameters": {"type": "object", "properties": {}}}},
    {"type": "function", "function": {"name": "pick", "description": "Open, go above an object, grasp it, lift it.",
        "parameters": {"type": "object", "properties": {"object_id": {"type": "string"}}, "required": ["object_id"]}}},
    {"type": "function", "function": {"name": "place_on", "description": "Put the held object on top of another object.",
        "parameters": {"type": "object", "properties": {"object_id": {"type": "string"}}, "required": ["object_id"]}}},
    {"type": "function", "function": {"name": "place_at", "description": "Put the held object on the table at x, y (mm).",
        "parameters": {"type": "object", "properties": {"x_mm": {"type": "number"}, "y_mm": {"type": "number"}},
                       "required": ["x_mm", "y_mm"]}}},
    {"type": "function", "function": {"name": "move_to", "description": "Move the gripper in a straight line to x, y, z (mm), optionally turning it to yaw (deg).",
        "parameters": {"type": "object", "properties": {"x_mm": {"type": "number"}, "y_mm": {"type": "number"},
                                                         "z_mm": {"type": "number"}, "yaw_deg": {"type": "number"}},
                       "required": ["x_mm", "y_mm", "z_mm"]}}},
    {"type": "function", "function": {"name": "move_relative", "description": f"Nudge the gripper by dx, dy, dz mm (each at most {C.MAX_RELATIVE_MM:.0f}).",
        "parameters": {"type": "object", "properties": {"dx_mm": {"type": "number"}, "dy_mm": {"type": "number"},
                                                         "dz_mm": {"type": "number"}}}}},
    {"type": "function", "function": {"name": "set_gripper", "description": "Open the gripper to a width in mm (0 closed, 95 fully open).",
        "parameters": {"type": "object", "properties": {"width_mm": {"type": "number"}}, "required": ["width_mm"]}}},
    {"type": "function", "function": {"name": "go_home", "description": "Move to the safe home position above the middle of the table.",
        "parameters": {"type": "object", "properties": {}}}},
]
MOTION_TOOLS = {"pick", "place_on", "place_at", "move_to", "move_relative", "set_gripper", "go_home"}
HOME_XY = (250.0, 0.0)


class Stopped(Exception):
    pass


class ToolError(Exception):
    pass


class Job:
    def __init__(self, text: str):
        self.id = f"{int(time.time() * 1000) % 10**9}"
        self.text, self.state = text, "running"         # running | waiting | done | stopped | failed
        self.log: list[dict] = []
        self.pending: dict | None = None                # a motion waiting for Approve
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


class Agent:
    def __init__(self, arm, frames, calib, settings, llm_factory, sim: bool = False):
        self.arm, self.frames, self.calib, self.settings = arm, frames, calib, settings
        self.llm_factory = llm_factory            # () -> chat client, or raises with the key status message
        self.sim = sim
        self.history: list[dict] = []             # text-only conversation kept between requests
        self.job: Job | None = None
        self.last_app_beat = time.time()
        self.lock = threading.Lock()
        self._scene_objects: dict[str, dict] = {}

    # ---- scene ----------------------------------------------------------------------------------------------
    def limits(self) -> dict:
        box = dict(C.WORKSPACE_MM)
        if self.calib.ready:
            floor = self.calib.table_z_mm + C.TABLE_CLEARANCE_MM
        else:
            floor = C.UNCALIBRATED_Z_FLOOR_MM
        return {"x": box["x"], "y": box["y"], "z": (floor, box["z"][1])}

    def scene(self) -> dict:
        from .vision import decode, detect
        objs, now = {}, time.time()
        jpg, t, err = self.frames.get("scene")
        if jpg is not None:
            counts: dict[str, int] = {}
            for o in detect(decode(jpg)):
                counts[o["colour"]] = counts.get(o["colour"], 0) + 1
                oid = f"{o['colour']}-{counts[o['colour']]}"
                item = {"id": oid, "colour": o["colour"], "pixel": [round(o["u"]), round(o["v"])],
                        "age_s": round(now - t, 1)}
                if self.calib.ready:
                    x, y = self.calib.to_table(o["u"], o["v"])
                    item.update(x_mm=round(x), y_mm=round(y))
                objs[oid] = item
            self._scene_objects = {k: dict(v, seen=t) for k, v in objs.items()}
        try:
            arm = self.arm.state()
        except DriverError as e:
            arm = {"reachable": False, "error": str(e)}
        return {"arm": {k: arm.get(k) for k in ("reachable", "enabled", "pose", "gripper_mm", "status", "owner", "error")
                        if k in arm},
                "objects": list(objs.values()), "calibrated": self.calib.ready,
                "scene_camera": "ok" if jpg is not None else (err or "no frame"),
                "workspace_mm": {k: [round(v[0]), round(v[1])] for k, v in self.limits().items()}}

    def _object(self, oid: str) -> dict:
        o = self._scene_objects.get(str(oid))
        if not o:
            raise ToolError(f"no object {oid!r} in the last scene; call get_scene")
        if time.time() - o["seen"] > C.OBJECT_MAX_AGE_S:
            raise ToolError(f"{oid} was last seen too long ago; call get_scene")
        if "x_mm" not in o:
            raise ToolError("the scene camera is not calibrated, so object positions are unknown")
        return o

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
            if not lo <= v <= hi:
                raise ToolError(f"{name}={v:.0f} mm is outside {lo:.0f}..{hi:.0f}")

    def move_line(self, job: Job, x, y, z, yaw=None):
        self._validate(x, y, z)
        start = self._pose()
        goal = [float(x), float(y), float(z)]
        dist = math.dist(start[:3], goal)
        speed = float(self.settings["speed_mm_s"])
        yaw0 = start[5]
        yaw1 = yaw0 if yaw is None else float(yaw)
        dyaw = (yaw1 - yaw0 + 180.0) % 360.0 - 180.0
        dur = max(dist / speed, abs(dyaw) / C.YAW_RATE_DEG_S, 0.1)
        t0 = time.time()
        deadline = t0 + dur + 6.0
        while True:
            self._check(job)
            w = min(1.0, (time.time() - t0) / dur)
            p = [start[i] + (goal[i] - start[i]) * w for i in range(3)]
            target = p + [start[3], start[4], yaw0 + dyaw * w]
            try:
                self.arm.servo(target)
            except DriverError as e:
                raise ToolError(str(e))
            if w >= 1.0:
                now = self._pose()
                if math.dist(now[:3], goal) < 4.0:
                    return now
                if time.time() > deadline:
                    raise ToolError(f"the arm stopped {math.dist(now[:3], goal):.0f} mm short of the target")
            time.sleep(0.1)

    def grip(self, job: Job, width):
        self._check(job)
        if not isinstance(width, (int, float)) or not 0 <= width <= 95:
            raise ToolError("width_mm must be 0..95")
        try:
            self.arm.gripper(width, int(self.settings["grip_effort"]))
        except DriverError as e:
            raise ToolError(str(e))
        t_end = time.time() + (0.3 if self.sim else 1.2)      # let the jaws travel
        while time.time() < t_end:
            self._check(job)
            time.sleep(0.05)

    # ---- tools ----------------------------------------------------------------------------------------------
    def run_tool(self, job: Job, name: str, a: dict):
        tz = self.calib.table_z_mm if self.calib.ready else None
        oh, travel = float(self.settings["object_height_mm"]), float(self.settings["travel_height_mm"])
        if name == "get_scene":
            return self.scene()
        if name in ("pick", "place_on", "place_at") and tz is None:
            raise ToolError("calibrate the scene camera and table height first")
        if name == "pick":
            o = self._object(a.get("object_id"))
            self.grip(job, self.settings["open_width_mm"])
            self.move_line(job, o["x_mm"], o["y_mm"], tz + travel)
            self.move_line(job, o["x_mm"], o["y_mm"], tz + oh * 0.5)
            self.grip(job, 0)
            self.move_line(job, o["x_mm"], o["y_mm"], tz + travel)
            return {"ok": True, "picked": o["id"]}
        if name in ("place_on", "place_at"):
            if name == "place_on":
                o = self._object(a.get("object_id"))
                x, y, z = o["x_mm"], o["y_mm"], tz + oh * 1.5 + 5.0
            else:
                x, y, z = a.get("x_mm"), a.get("y_mm"), tz + oh * 0.5 + 5.0
            self.move_line(job, x, y, tz + travel)
            self.move_line(job, x, y, z)
            self.grip(job, self.settings["open_width_mm"])
            self.move_line(job, x, y, tz + travel)
            return {"ok": True, "placed_at": [round(x), round(y)]}
        if name == "move_to":
            p = self.move_line(job, a.get("x_mm"), a.get("y_mm"), a.get("z_mm"), a.get("yaw_deg"))
            return {"ok": True, "pose": [round(v, 1) for v in p]}
        if name == "move_relative":
            d = [float(a.get(k) or 0.0) for k in ("dx_mm", "dy_mm", "dz_mm")]
            if any(abs(v) > C.MAX_RELATIVE_MM for v in d):
                raise ToolError(f"each step must be at most {C.MAX_RELATIVE_MM:.0f} mm")
            p = self._pose()
            p = self.move_line(job, p[0] + d[0], p[1] + d[1], p[2] + d[2])
            return {"ok": True, "pose": [round(v, 1) for v in p]}
        if name == "set_gripper":
            self.grip(job, a.get("width_mm"))
            return {"ok": True}
        if name == "go_home":
            z = (tz + travel) if tz is not None else max(C.UNCALIBRATED_Z_FLOOR_MM, 300.0)
            p = self.move_line(job, HOME_XY[0], HOME_XY[1], z)
            return {"ok": True, "pose": [round(v, 1) for v in p]}
        raise ToolError(f"unknown tool {name}")

    def _approve(self, job: Job, name: str, args: dict) -> bool:
        if not self.settings["confirm_moves"] or name not in MOTION_TOOLS:
            return True
        job.pending, job.approved = {"tool": name, "args": args}, False
        job.decision.clear()
        job.state = "waiting"
        while not job.decision.wait(0.2):
            if job.stop.is_set():
                raise Stopped("stopped")
        job.pending, job.state = None, "running"
        self.last_app_beat = time.time()    # the user just answered: the app is alive
        return job.approved

    # ---- the job --------------------------------------------------------------------------------------------
    def _images(self) -> list:
        if not self.settings["images"]:
            return []
        parts = []
        for role in ("scene", "wrist"):
            jpg, _, _ = self.frames.get(role)
            if jpg:
                url = "data:image/jpeg;base64," + base64.b64encode(jpg).decode()
                parts += [{"type": "text", "text": f"{role} camera:"},
                          {"type": "image_url", "image_url": {"url": url, "detail": "low"}}]
        return parts

    def start(self, text: str) -> Job:
        with self.lock:
            if self.job and self.job.state in ("running", "waiting"):
                raise ValueError("a request is still running; stop it first")
            job = self.job = Job(text)
        self.last_app_beat = time.time()
        threading.Thread(target=self._run, args=(job,), daemon=True, name="job").start()
        return job

    def _run(self, job: Job):
        job.add("user", job.text)
        try:
            llm = self.llm_factory()
        except Exception as e:
            job.add("error", str(e))
            job.state = "failed"
            return
        self.frames.now()
        time.sleep(0.3)
        lim = self.limits()
        system = SYSTEM_PROMPT.format(x0=lim["x"][0], x1=lim["x"][1], y0=lim["y"][0], y1=lim["y"][1],
                                      z0=round(lim["z"][0]), z1=lim["z"][1])
        scene = self.scene()
        user = {"role": "user", "content": [{"type": "text", "text": job.text + "\n\nCurrent scene (json): " +
                                             json.dumps(scene)}] + self._images()}
        msgs = [{"role": "system", "content": system}] + self.history[-20:] + [user]
        calls = 0
        try:
            while True:
                if job.stop.is_set():
                    raise Stopped("stopped")
                reply = llm.chat(msgs, TOOLS)
                msgs.append(reply)
                if reply.get("content"):
                    job.add("assistant", reply["content"])
                if not reply.get("tool_calls"):
                    break
                for tc in reply["tool_calls"]:
                    calls += 1
                    name = tc["function"]["name"]
                    try:
                        args = json.loads(tc["function"]["arguments"] or "{}")
                    except ValueError:
                        args = {}
                    job.add("tool", name, args)
                    if calls > C.MAX_TOOL_CALLS:
                        result = {"error": f"tool call limit ({C.MAX_TOOL_CALLS}) reached; stop and report"}
                    elif not self._approve(job, name, args):
                        result = {"error": "the user rejected this step"}
                    else:
                        try:
                            result = self.run_tool(job, name, args)
                        except ToolError as e:
                            result = {"error": str(e)}
                    job.add("result", json.dumps(result)[:600], result)
                    msgs.append({"role": "tool", "tool_call_id": tc["id"], "content": json.dumps(result)})
                if calls > C.MAX_TOOL_CALLS + 2:
                    job.add("error", "too many tool calls; the request was ended")
                    break
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
        # Keep the conversation as text only: images and scene dumps are stale by the next request.
        self.history += [{"role": "user", "content": job.text}]
        final = next((e["text"] for e in reversed(job.log) if e["kind"] == "assistant"), None)
        if final:
            self.history.append({"role": "assistant", "content": final})
        self.history = self.history[-20:]

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
