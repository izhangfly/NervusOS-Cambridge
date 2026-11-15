"""The arm, as this project uses it: a tool point that stays vertical, moved through the PiPER driver.

The driver (hardware/piper/piper_webctl.py, 127.0.0.1:8770) owns CAN, plans and smooths the motion and keeps
its own guards. This module adds what a pick-and-place needs on top:

  * a TOOL POINT between the finger pads, 120 mm past the wrist flange (the driver's pose is the flange), with
    the tool pointing down. Where the wrist can't reach with a vertical tool (J5 only bends 70 deg), the tool
    LEANS about the line the fingers close along, tips towards the target's side: the pads stay level, so a
    bottle held upright between them keeps hanging upright (piper-astra-jev's `about_fingers` tilt);
  * blocking moves that keep the servo goal alive until the arm is there, and say why if it isn't;
  * the gripper, and "is it holding something" from where the fingers stopped;
  * a floor: the fingertips never go below `floor_z_mm`.

Units: millimetres and degrees, in the arm's base frame (+x forward, +y left, +z up).
"""
from __future__ import annotations

import json
import math
import sys
import threading
import time
import urllib.error
import urllib.request
from pathlib import Path

import numpy as np

# The driver's kinematics (FK, closed-form IK inside the joint limits): next to the driver, or in this repository.
for _p in (Path(__file__).resolve().parents[2] / "hardware" / "piper", Path(__file__).resolve().parent / "vendor",
           Path("/opt/nervus/piper")):
    if (_p / "kinematics.py").exists():
        sys.path.insert(0, str(_p))
        break
import kinematics as K  # noqa: E402

KIN = K.PiperKinematics()
R_DOWN = np.diag([-1.0, 1.0, -1.0])       # tool pointing straight down
LEAN_STEP_DEG = 5.0
LEAN_MAX_DEG = 65.0

CLIENT = "gptarm"
GRASP_OFFSET_MM = 120.0     # flange to the point between the finger pads
TIP_OFFSET_MM = 15.0        # from that point on to the closed fingertips
GRIPPER_OPEN_MM = 75.0
HOLD_MIN_MM = 8.0           # fingers that stopped wider than this hold something
FINGER_HALF_MM = 40.0       # open fingers: each this far from the tool axis, along the closing line
CEILING_FLANGE_MM = 560.0   # the overhead camera hangs about a metre above the table: the wrist stays well below it


# The driver's joint ranges, a few degrees inside the arm's own (J1 +-154, J2 0..195, J3 -175..0, J4 -100..112,
# J5 +-75, J6 +-170): what a model may ask for in joint space.
JOINT_LIMITS_DEG = [(-150.0, 150.0), (0.0, 180.0), (-170.0, 0.0), (-100.0, 100.0), (-70.0, 70.0), (-100.0, 100.0)]
STREAM_DT = 0.04


class ArmError(Exception):
    pass


class Stop(Exception):
    """The run was stopped (STOP in the portal, or the watchdog)."""


def _rz(a):
    c, s = math.cos(a), math.sin(a)
    return np.array([[c, -s, 0.0], [s, c, 0.0], [0.0, 0.0, 1.0]])


def _ry(a):
    c, s = math.cos(a), math.sin(a)
    return np.array([[c, 0.0, s], [0.0, 1.0, 0.0], [-s, 0.0, c]])


def tool_rotation(yaw_deg: float, lean_deg: float) -> np.ndarray:
    """Tool down, tips leaning `lean` towards the yaw direction; the fingers close across it, level."""
    return _rz(math.radians(yaw_deg)) @ _ry(-math.radians(lean_deg)) @ R_DOWN


def flange_pose(x, y, z, lean_deg=0.0, yaw_deg=None):
    """Driver pose [x, y, z, rx, ry, rz] of the flange for a tool point at (x, y, z). yaw defaults to the
    target's bearing from the base, so the lean points outward and the fingers close sideways."""
    yaw = math.degrees(math.atan2(y, x)) if yaw_deg is None else float(yaw_deg)
    R = tool_rotation(yaw, lean_deg)
    p = np.array([x, y, z], float) - R @ np.array([0.0, 0.0, GRASP_OFFSET_MM])
    return K.rt_to_pose(p, R)


def reachable(x, y, z, lean_deg, yaw_deg=None) -> bool:
    yaw = math.degrees(math.atan2(y, x)) if yaw_deg is None else float(yaw_deg)
    R = tool_rotation(yaw, lean_deg)
    p = np.array([x, y, z], float) - R @ np.array([0.0, 0.0, GRASP_OFFSET_MM])
    return len(KIN.ik_all_rt(p, R)) > 0


def min_lean(x, y, z, yaw_deg=None, at_least=0.0):
    """The smallest lean (deg, in LEAN_STEP steps) that reaches the tool point, or None."""
    lean = float(at_least)
    while lean <= LEAN_MAX_DEG + 1e-6:
        if reachable(x, y, z, lean, yaw_deg):
            return lean
        lean += LEAN_STEP_DEG
    return None


def tip_points(pose):
    """The two open fingertips and the closed tip (mm) for a driver flange pose: the lowest, widest parts."""
    p, R = K.pose_to_rt(pose)
    p = np.asarray(p, float)
    end = p + R @ np.array([0.0, 0.0, GRASP_OFFSET_MM + TIP_OFFSET_MM])
    side = R @ np.array([0.0, FINGER_HALF_MM, 0.0])
    return [end, end + side, end - side]


def tool_from_flange(pose):
    """(tool point xyz, lean deg, yaw deg) from a driver flange pose."""
    p, R = K.pose_to_rt(pose)
    axis = R @ np.array([0.0, 0.0, 1.0])
    tool = np.asarray(p, float) + axis * GRASP_OFFSET_MM
    lean = math.degrees(math.acos(max(-1.0, min(1.0, -axis[2]))))
    yaw = math.degrees(math.atan2(axis[1], axis[0])) if lean > 1.0 else math.degrees(math.atan2(-R[1, 0], -R[0, 0]))
    return [round(float(v), 1) for v in tool], round(lean, 1), round(yaw, 1)


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
            "tool": tool_from_flange(p)[0], "lean": tool_from_flange(p)[1],
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

    def move_tool(self, x, y, z, lean=None, yaw_deg=None, tol_mm=4.0, timeout=60.0, speed=None, min_tip_z=None):
        """Move the tool point to (x, y, z). lean None = the smallest lean that reaches it; a number = at least
        that lean. Blocks until there; returns the state, with the lean used. min_tip_z: hold and fail if a
        fingertip dips below this height on the way (the path is the driver's, this is the check on it)."""
        use = min_lean(x, y, z, yaw_deg, at_least=lean or 0.0)
        if use is None:
            raise ArmError(f"({x:.0f}, {y:.0f}, {z:.0f}) mm is out of the arm's reach")
        goal = flange_pose(x, y, z, use, yaw_deg)
        low = min(float(t[2]) for t in tip_points(goal))
        if low < self.floor_z_mm:
            raise ArmError(f"z={z:.0f} mm would put the fingertips at {low:.0f} mm, below the floor ({self.floor_z_mm:.0f} mm)")
        if goal[2] > CEILING_FLANGE_MM:
            raise ArmError(f"({x:.0f}, {y:.0f}, {z:.0f}) mm puts the wrist at {goal[2]:.0f} mm, above the ceiling ({CEILING_FLANGE_MM:.0f} mm)")
        self.enable()
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
            tip = min(float(t[2]) for t in tip_points(p))
            if tip < min(self.floor_z_mm, low) - 8.0 or (min_tip_z is not None and tip < min_tip_z - 5.0):
                self.hold()
                raise ArmError(f"the fingertips dipped to {tip:.0f} mm on the way: stopped")
            if d < tol_mm and not moving:
                return self.state()
            still = still + 1 if (last is not None and math.dist(p[:3], last) < 0.3 and not moving) else 0
            last = p[:3]
            if still > 25 or time.time() > t_end:           # ~2.5 s without motion, short of the goal
                plan = s.get("plan") or {}
                self.hold()
                raise ArmError(f"stopped {d:.0f} mm short of the target ({plan.get('kind')}: {plan.get('reason')})")
            time.sleep(0.1)

    def move_joints(self, q, max_deg_s=30.0, timeout=30.0):
        """Move to joint angles q (deg) along a straight line in joint space, streamed at 25 Hz so the speed is
        ours to set. The whole line is checked first: every point keeps the fingertips above the floor and the
        wrist below the ceiling. Blocks until there; raises ArmError if refused or if the arm falls behind."""
        q = [float(v) for v in q]
        if len(q) != 6 or not all(math.isfinite(v) for v in q):
            raise ArmError("joints must be 6 numbers in degrees")
        for i, (v, (lo, hi)) in enumerate(zip(q, JOINT_LIMITS_DEG)):
            if not lo <= v <= hi:
                raise ArmError(f"J{i + 1} = {v:.1f} deg is outside its range ({lo:.0f} to {hi:.0f})")
        self.enable()
        now = np.array(self.raw_state()["current"], float)
        goal = np.array(q)
        n = max(1, int(math.ceil(float(np.max(np.abs(goal - now))) / (max_deg_s * STREAM_DT))))
        path = [now + (goal - now) * (k / n) for k in range(1, n + 1)]
        for k, qk in enumerate(path):
            pose = KIN.fk_pose(qk)
            low = min(float(t[2]) for t in tip_points(pose))
            if low < self.floor_z_mm:
                raise ArmError(f"{100 * (k + 1) // n}% of the way there the fingertips would be at z={low:.0f} mm, "
                               f"below the floor ({self.floor_z_mm:.0f} mm)")
            if pose[2] > CEILING_FLANGE_MM:
                raise ArmError(f"the wrist would rise to {pose[2]:.0f} mm, above the ceiling ({CEILING_FLANGE_MM:.0f} mm)")
        self._req("/api/speed", {"speed": self.speed})
        for k, qk in enumerate(path):
            self._check()
            self._req("/api/stream", {"joints": [round(float(v), 3) for v in qk]})
            if k % 5 == 4:
                actual = np.array(self.raw_state()["current"], float)
                if float(np.max(np.abs(actual - path[max(0, k - 5)]))) > 20.0:
                    raise ArmError("the arm fell behind the motion (blocked?): stopped where it is")
            time.sleep(STREAM_DT)
        t_end = time.time() + min(timeout, 6.0)
        while time.time() < t_end:
            self._check()
            self._req("/api/stream", {"joints": [round(float(v), 3) for v in goal]})
            actual = np.array(self.raw_state()["current"], float)
            if float(np.max(np.abs(actual - goal))) < 1.0:
                return self.state()
            time.sleep(0.1)
        off = np.abs(np.array(self.raw_state()["current"], float) - goal)
        raise ArmError(f"stopped short: J{int(np.argmax(off)) + 1} is {float(np.max(off)):.1f} deg from its target")

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
