"""A pretend arm, table and cameras with the same interfaces as the real ones.

For tests and for trying the service, the portal and a model without the arm: the bottle is picked up if
the fingers close around it, stands if it is set down gently on a floor, falls over if it is dropped, and
can be made to slip (`miss`) or to fall on release (`topple`) to exercise the retries.
"""
from __future__ import annotations

import math
import threading
import time

import cv2
import numpy as np

from . import arm as A
from . import calib as C
from . import perception as P


def camera(size=(1600, 1200)):
    """A plausible overhead camera: 0.9 m up, a little off to the right, looking down."""
    f, c = 1240.0, np.array([300.0, -150.0, 900.0])
    K = np.array([[f, 0, size[0] / 2.0], [0, f, size[1] / 2.0], [0, 0, 1.0]])
    rx, rz = math.radians(172.0), math.radians(100.0)
    Rx = np.array([[1, 0, 0], [0, math.cos(rx), -math.sin(rx)], [0, math.sin(rx), math.cos(rx)]])
    Rz = np.array([[math.cos(rz), -math.sin(rz), 0], [math.sin(rz), math.cos(rz), 0], [0, 0, 1]])
    R = Rx @ Rz
    return C.Camera(K, np.zeros(5), cv2.Rodrigues(R)[0], -R @ c, size, points=[{}] * 20, residual_px=0.0)


class Table:
    """The truth the pretend arm and cameras share."""

    def __init__(self, bottle_xy=(330.0, 200.0), basket_xy=(300.0, -170.0)):
        self.bottle = {"xy": list(bottle_xy), "where": "table", "upright": True, "held": False}
        self.basket = {"xy": list(basket_xy), "radius": 120.0, "floor": 8.0, "rim": 200.0}
        self.miss = 0            # the next N grasps catch nothing
        self.topple = 0          # the next N releases on the table fall over
        self.grip_height = 0.0   # tool height above the bottle's base while held

    def floor_at(self, xy):
        return self.basket["floor"] if math.dist(xy, self.basket["xy"]) < self.basket["radius"] else 0.0


class SimArm:
    def __init__(self, table=None, speed=20, move_s=0.0, stop_event=None):
        self.t = table or Table()
        self.tool = [138.0 + 60.0, 0.0, 300.0]
        self.lean = 40.0
        self.yaw, self.grip_dir = None, None          # None: towards the bearing, fingers across the lean
        self.width = A.GRIPPER_OPEN_MM
        self.grip_cmd = None
        self.move_s = move_s
        self.stop_event = stop_event or threading.Event()
        self.speed = speed
        self.enabled = True
        self.floor_z_mm = 20.0
        self.walls = [dict(w) for w in A.DEFAULT_WALLS]   # the real table's backboard, at its default place

    # the same surface as arm.Arm
    def _check(self):
        if self.stop_event.is_set():
            raise A.Stop("stopped")

    def raw_state(self):
        pose = self._pose()
        return {"enabled": self.enabled, "feedback_fresh": True, "arm_status": "NORMAL", "pose_current": pose,
                "current": self._joints(pose), "gripper_actual_mm": self.width, "trajectory": {"moving": False}}

    def _pose(self):
        return A.flange_pose(*self.tool, self.lean, self.yaw, self.grip_dir)

    def _joints(self, pose):
        q = A.KIN.nearest_ik(pose, [0, 45, -60, 0, 45, 0])
        return [round(float(v), 1) for v in q] if q is not None else [0.0] * 6

    def state(self):
        pose = self._pose()
        return {"enabled": self.enabled, "fresh": True, "status": "NORMAL", "teaching": False,
                "flange": [round(v, 1) for v in pose], "tool": [round(v, 1) for v in self.tool], "lean": self.lean,
                "yaw": A.tool_from_flange(pose)[2], "grip": A.grip_of(pose),
                "joints": self._joints(pose), "gripper_mm": self.width, "moving": False, "plan": None,
                "plan_reason": None, "guard": None, "owner": A.CLIENT, "speed": self.speed}

    def enable(self):
        self.enabled = True

    def home(self, timeout=40.0):
        self.tool, self.lean, self.yaw, self.grip_dir = [198.0, 0.0, 300.0], 40.0, None, None

    def move_tool(self, x, y, z, lean=None, yaw_deg=None, tol_mm=4.0, timeout=60.0, speed=None, min_tip_z=None,
                  grip_deg=None):
        self._check()
        self.enable()                     # as arm.Arm does: a new move after an e-stop re-enables
        use, goal = A.goal_for(x, y, z, lean, yaw_deg, self._pose(), self.walls, grip_deg)
        low = min(float(t[2]) for t in A.tip_points(goal))
        if low < self.floor_z_mm:
            raise A.ArmError(f"z={z:.0f} mm would put the fingertips at {low:.0f} mm, below the floor ({self.floor_z_mm:.0f} mm)")
        if self.t.bottle["held"]:
            base = z - self.t.grip_height
            if math.dist((x, y), self.t.basket["xy"]) < self.t.basket["radius"] + 25 and base < self.t.floor_at((x, y)) - 2:
                raise A.ArmError("stopped 12 mm short of the target (blocked: something is in the way)")
        t_end = time.time() + self.move_s
        while time.time() < t_end:
            self._check()
            time.sleep(0.02)
        self.tool, self.lean = [float(x), float(y), float(z)], use
        self.yaw, self.grip_dir = yaw_deg, grip_deg
        if self.t.bottle["held"]:
            self.t.bottle["xy"] = [float(x), float(y)]
        return self.state()

    def move_joints(self, q, max_deg_s=30.0, timeout=30.0):
        self._check()
        self.enable()
        for i, (v, (lo, hi)) in enumerate(zip(q, A.JOINT_LIMITS_DEG)):
            if not lo <= v <= hi:
                raise A.ArmError(f"J{i + 1} = {v:.1f} deg is outside its range ({lo:.0f} to {hi:.0f})")
        pose = A.KIN.fk_pose(q)
        if min(float(t[2]) for t in A.tip_points(pose)) < self.floor_z_mm:
            raise A.ArmError("the fingertips would go below the floor")
        A.check_walls(pose, self._pose(), self.walls)
        tool, lean, yaw = A.tool_from_flange(pose)
        self.tool, self.lean, self.yaw, self.grip_dir = tool, lean, yaw, A.grip_of(pose)
        if self.t.bottle["held"]:
            self.t.bottle["xy"] = [tool[0], tool[1]]
        return self.state()

    def hold(self):
        pass

    def release(self):
        pass

    def estop(self):
        self.enabled = False

    def grip(self, width_mm, effort=600, wait_s=1.6):
        self._check()
        b, t = self.t.bottle, self.t
        self.grip_cmd = float(width_mm)
        if width_mm < 40.0:                       # a close (to anything narrower than the bottle)
            floor = t.floor_at(b["xy"])
            near = math.dist(self.tool[:2], b["xy"]) < (14.0 if b["upright"] else 45.0)
            tall = P.BOTTLE_HEIGHT_MM if b["upright"] else P.BOTTLE_DIAMETER_MM
            around = (floor + 25.0 <= self.tool[2] <= floor + tall - 5.0) if b["upright"] else (floor + 10.0 <= self.tool[2] <= floor + tall)
            if near and around and not b["held"] and t.miss <= 0:
                self.width = 30.0 if self.tool[2] > floor + 100 else 48.0
                if effort >= 500:                       # a firm grasp picks it up; a light touch only feels it
                    b["held"], t.grip_height = True, self.tool[2] - floor
                    b["hanging"] = not b["upright"]     # taken lying, near one end: it swings to hang base down
            else:
                if near and around:
                    t.miss -= 1
                self.width = max(0.4, float(width_mm))
        else:
            if b["held"]:
                b["held"] = False
                xy = list(self.tool[:2])
                floor = t.floor_at(xy)
                in_basket = math.dist(xy, t.basket["xy"]) < t.basket["radius"]
                if b.pop("hanging", False):
                    gentle = self.tool[2] - floor <= 125.0 / 2.0 + 25.0 + 12.0
                else:
                    gentle = (self.tool[2] - t.grip_height) <= floor + 12.0
                b["where"] = "basket" if in_basket else "table"
                b["upright"] = bool(gentle)
                if not in_basket and gentle and t.topple > 0:
                    t.topple -= 1
                    b["upright"] = False
                b["xy"] = xy if b["upright"] else [xy[0] + 45.0, xy[1] + 20.0]
            self.width = float(width_mm)
        return self.width

    def holding(self):
        if self.grip_cmd is None or self.grip_cmd > A.GRIPPER_OPEN_MM - 10.0:
            return False
        return bool(self.width > max(A.HOLD_MIN_MM, self.grip_cmd + 4.0))


class SimOverhead:
    def __init__(self, table, cam):
        self.t, self.cam = table, cam

    def frame(self):
        img = np.full((self.cam.size[1], self.cam.size[0], 3), 40, np.uint8)
        board = self.cam.project(np.array([[0, -340, 0], [440, -340, 0], [440, 340, 0], [0, 340, 0]], float))
        cv2.fillPoly(img, [np.round(board).astype(np.int32)], (205, 205, 205))
        k = P.px_per_mm(self.cam, [300, 0, 0])
        u, v = self.cam.project([*self.t.basket["xy"], self.t.basket["rim"]])
        cv2.circle(img, (int(u), int(v)), int(self.t.basket["radius"] * k * 1.25), (25, 25, 25), -1)
        b = self.t.bottle
        if not b["held"]:
            z = self.t.floor_at(b["xy"]) + (P.BOTTLE_HEIGHT_MM if b["upright"] else 25.0)
            u, v = self.cam.project([*b["xy"], z])
            cv2.circle(img, (int(u), int(v)), int(26 * k), (150, 215, 225), -1)
            cv2.circle(img, (int(u), int(v)), int(19 * k), (20, 20, 20), -1)
        return img

    def settled(self, n=3, gap_s=0.0):
        return self.frame()

    def jpeg(self):
        return cv2.imencode(".jpg", self.frame())[1].tobytes()

    def info(self):
        return {"age_s": 0.1, "width": self.cam.size[0], "height": self.cam.size[1], "exposure_us": 30000, "gain": 4.0}


class SimWrist:
    error = None

    def frame(self, max_age_s=2.0):
        img = np.full((720, 1280, 3), 190, np.uint8)
        cv2.putText(img, "simulated wrist camera", (360, 370), cv2.FONT_HERSHEY_SIMPLEX, 1.4, (90, 90, 90), 3, cv2.LINE_AA)
        return img

    def close(self):
        pass


def perceive(table):
    """The truth, as perception would report it."""
    def f(_img, _joints):
        s = P.Scene(table_level=200.0)
        s.basket = {"xy": list(table.basket["xy"]), "px": [0, 0], "box_px": [0, 0, 0, 0], "size_mm": [260, 240]}
        b = table.bottle
        if not b["held"]:
            s.bottle = {"where": b["where"], "xy": list(b["xy"]), "upright": b["upright"] if b["where"] == "table" else None,
                        "px": [0, 0]}
            if b["where"] == "table" and not b["upright"]:
                s.bottle.update(axis_deg=b.get("axis_deg", 30.0), length_mm=110.0)
        return s
    return f
