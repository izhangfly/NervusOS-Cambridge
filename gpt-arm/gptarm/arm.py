"""The arm, as this project uses it: a tool point that stays vertical, moved through the PiPER driver.

The driver (hardware/piper/piper_webctl.py, 127.0.0.1:8770) owns CAN, plans and smooths the motion and keeps
its own guards. This module adds what a pick-and-place needs on top:

  * a TOOL POINT between the finger pads, 120 mm past the wrist flange (the driver's pose is the flange), with
    the tool pointing straight down and a yaw about the vertical (as piper-astra-jev's kinematics define it);
  * blocking moves that keep the servo goal alive until the arm is there, and say why if it isn't;
  * the gripper, and "is it holding something" from where the fingers stopped;
  * a floor: the fingertips never go below `floor_z_mm`.

Units: millimetres and degrees, in the arm's base frame (+x forward, +y left, +z up).
"""
from __future__ import annotations

import json
import math
import threading
import time
import urllib.error
import urllib.request

CLIENT = "gptarm"
GRASP_OFFSET_MM = 120.0     # flange to the point between the finger pads
TIP_OFFSET_MM = 15.0        # from that point on to the closed fingertips
GRIPPER_OPEN_MM = 75.0
HOLD_MIN_MM = 8.0           # fingers that stopped wider than this hold something


class ArmError(Exception):
    pass


class Stop(Exception):
    """The run was stopped (STOP in the portal, or the watchdog)."""


def tool_to_flange(x, y, z, yaw_deg=0.0):
    """Flange pose [x, y, z, rx, ry, rz] (driver convention, static XYZ: R = Rz Ry Rx) for a vertical tool."""
    return [float(x), float(y), float(z) + GRASP_OFFSET_MM, 0.0, 180.0, float(yaw_deg)]


class Arm:
    def __init__(self, base="http://127.0.0.1:8770", speed=20, floor_z_mm=20.0, stop_event=None):
        self.base = base.rstrip("/")
        self.speed = int(speed)
        self.floor_z_mm = float(floor_z_mm)        # lowest allowed fingertip height
        self.stop_event = stop_event or threading.Event()
        self.grip_cmd = None

    # ---- http ------------------------------------------------------------------------------------------------
    def _req(self, path, body=None, timeout=3.0):
        data = None if body is None else json.dumps({"client": CLIENT, **body}).encode()
        req = urllib.request.Request(self.base + path, data, {"Content-Type": "application/json"})
        try:
            with urllib.request.urlopen(req, timeout=timeout) as r:
                return json.loads(r.read() or b"{}")
        except urllib.error.HTTPError as e:
            try:
                msg = json.loads(e.read()).get("error", "")
            except ValueError:
                msg = ""
            raise ArmError(f"driver refused {path}: {msg or e.code}") from None
        except (urllib.error.URLError, OSError) as e:
            raise ArmError(f"arm driver unreachable: {getattr(e, 'reason', e)}") from None

    def raw_state(self):
        return self._req("/api/state?lite=1")

    def state(self):
        s = self.raw_state()
        p = s.get("pose_current") or [0.0] * 6
        return {
            "enabled": bool(s.get("enabled")), "fresh": bool(s.get("feedback_fresh")),
            "status": s.get("arm_status"), "teaching": bool(s.get("teaching")),
            "flange": [round(v, 1) for v in p],
            "tool": [round(p[0], 1), round(p[1], 1), round(p[2] - GRASP_OFFSET_MM, 1)],   # valid while vertical
            "vertical": abs(abs(p[4]) - 180.0) < 6.0 or (abs(p[4]) < 6.0 and abs(abs(p[3]) - 180.0) < 6.0),
            "joints": [round(v, 1) for v in s.get("current") or []],
            "gripper_mm": s.get("gripper_actual_mm"), "moving": bool((s.get("trajectory") or {}).get("moving")),
            "plan": (s.get("plan") or {}).get("kind"), "plan_reason": (s.get("plan") or {}).get("reason"),
            "guard": (s.get("guard") or {}).get("reason") if s.get("guard") else None,
            "owner": (s.get("owner") or {}).get("client"), "speed": s.get("speed"),
        }

    # ---- motion ----------------------------------------------------------------------------------------------
    def _check(self):
        if self.stop_event.is_set():
            raise Stop("stopped")

    def enable(self):
        s = self.raw_state()
        if not s.get("feedback_fresh"):
            raise ArmError("the arm is not sending feedback (is it powered, is CAN plugged in?)")
        if not s.get("enabled"):
            self._req("/api/enable", {"on": True}, timeout=6.0)
            time.sleep(0.5)

    def home(self, timeout=40.0):
        """The driver's READY pose, in joint space."""
        self.enable()
        self._req("/api/speed", {"speed": self.speed})
        self._req("/api/home", {"client": "panel"})
        ready, t_end = None, time.time() + timeout
        while time.time() < t_end:
            self._check()
            self._req("/api/heartbeat", {"client": "panel"})
            s = self.raw_state()
            ready = s.get("ready_pose") or [0, 45, -60, 0, 45, 0]
            if max(abs(a - b) for a, b in zip(s["current"], ready)) < 1.0 and not (s.get("trajectory") or {}).get("moving"):
                self._req("/api/stop", {})
                return
            time.sleep(0.25)
        self._req("/api/stop", {})
        raise ArmError("the arm did not reach its ready pose in time")

    def move_tool(self, x, y, z, yaw_deg=0.0, tol_mm=4.0, timeout=60.0, speed=None):
        """Move the tool point to (x, y, z) with the tool vertical. Blocks until there."""
        if z - TIP_OFFSET_MM < self.floor_z_mm:
            raise ArmError(f"z={z:.0f} mm would put the fingertips below the floor ({self.floor_z_mm:.0f} mm)")
        self.enable()
        goal = tool_to_flange(x, y, z, yaw_deg)
        body = {"pose": goal, "speed": int(speed or self.speed)}
        t_end = time.time() + timeout
        still = 0
        last = None
        while True:
            self._check()
            r = self._req("/api/servo", body)
            if r.get("clamped"):
                self.hold()
                raise ArmError(f"target {goal[:3]} is outside the driver's workspace box")
            s = self.raw_state()
            p = s["pose_current"]
            d = math.dist(p[:3], goal[:3])
            moving = (s.get("trajectory") or {}).get("moving")
            if d < tol_mm and not moving:
                return self.state()
            still = still + 1 if (last is not None and math.dist(p[:3], last) < 0.3 and not moving) else 0
            last = p[:3]
            if still > 25 or time.time() > t_end:           # ~2.5 s without motion, short of the goal
                plan = s.get("plan") or {}
                self.hold()
                raise ArmError(f"stopped {d:.0f} mm short of the target ({plan.get('kind')}: {plan.get('reason')})")
            time.sleep(0.1)

    def hold(self):
        try:
            self._req("/api/stop", {})
        except ArmError:
            pass

    def release(self):
        try:
            self._req("/api/release", {})
        except ArmError:
            pass

    def estop(self):
        self._req("/api/estop", {})

    # ---- gripper ---------------------------------------------------------------------------------------------
    def grip(self, width_mm, effort=600, wait_s=1.6):
        self._check()
        self._req("/api/gripper", {"mm": float(width_mm), "effort": int(effort)})
        self.grip_cmd = float(width_mm)
        t_end = time.time() + wait_s
        while time.time() < t_end:
            self._check()
            time.sleep(0.05)
        return self.raw_state().get("gripper_actual_mm")

    def holding(self):
        """True when the gripper was told to close and the fingers stopped part way."""
        w = self.raw_state().get("gripper_actual_mm")
        return bool(self.grip_cmd is not None and self.grip_cmd < 3.0 and w is not None and w > HOLD_MIN_MM)
