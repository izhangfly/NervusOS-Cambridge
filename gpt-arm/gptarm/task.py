"""The task as the model sees it: a handful of skills, the state they act on, and the checks under them.

The split is the one in AgileX's piper-astra-jev (Apache-2.0), adapted to this arm, these cameras and a task
with a return trip: the model only chooses WHICH skill comes next; each skill is a bounded motion written
here; a governor refuses a skill whose preconditions do not hold and says why; a grasp is believed when the
fingers stop on something. What is new here: the skills take a target (bottle, basket, start spot), because
the task goes there and back; a refusal's reason is shown to the model on its next turn; and `look` is a
skill, because the one overhead camera cannot see past the arm.

Units are millimetres in the arm's base frame (+x forward, +y left, +z up; the table is z = 0).
"""
from __future__ import annotations

import json
import math
import os
import time
from dataclasses import asdict, dataclass
from pathlib import Path

import cv2

from . import arm as A
from . import calib as C
from . import perception as P

HOME = Path(os.environ.get("GPTARM_HOME", "~/.config/gptarm")).expanduser()
CONFIG_PATH = HOME / "config.json"
CONTEXT_PATH = HOME / "context.md"

DEFAULT_INSTRUCTION = ("Put the hair gel bottle into the basket. Then take it out again and stand it upright "
                       "on the table where it started.")

SKILLS = {
    "look": "Move the arm aside and photograph the table to find the bottle and the basket again. Use it when "
            "the bottle's position is unknown or old (bottle.seen_s_ago is large and something has happened "
            "since). Only when height is carry.",
    "move_above": "Carry the gripper, at carry height, to above the target: bottle, basket or start_spot. Only "
                  "when height is carry. Target bottle only when holding is false and the bottle's position is "
                  "known.",
    "lower": "Lower the gripper straight down from carry height to working height at the place it is above: "
             "around the bottle to grasp it (above bottle, gripper open, not holding), or down to where a held "
             "bottle can be let go (above basket or above start_spot, holding). Only when height is carry and "
             "above is not nothing.",
    "close_gripper": "Close the fingers to grasp what is between them. Only when gripper is open and height is "
                     "working.",
    "open_gripper": "Open the fingers: lets go of a held bottle, or reopens after a grasp that caught nothing. "
                    "Only when gripper is closed.",
    "lift": "Raise the gripper straight up to carry height. Only when height is not carry.",
    "stand_up": "Stand a bottle that lies on its side on the table back up: the gripper takes it near the cap, "
                "lifts so that it hangs base down, sets it on its base and lets go. Only when the bottle lies on the "
                "table (bottle.upright is false), the gripper is open and empty, and height is carry.",
    "done": "The whole task is complete: stop. The result is then checked by the cameras and by touch.",
}
TARGETS = ("bottle", "basket", "start_spot", "none")
SCHEMA = {"type": "object", "additionalProperties": False, "required": ["skill", "target", "note"],
          "properties": {"skill": {"type": "string", "enum": list(SKILLS)},
                         "target": {"type": "string", "enum": list(TARGETS)},
                         "note": {"type": "string"}}}

DEFAULT_CONTEXT = """\
# The arm and the scene

You control an AgileX PiPER, a six-joint arm with a two-finger gripper, bolted to the near edge of a small
white table. You do not move its joints. You choose one skill at a time from the list; each skill is a
bounded motion carried out by code, and a governor refuses a skill whose conditions do not hold.

- Coordinates are millimetres from the arm's base: +x is forward (away from the base, across the table),
  +y is to the arm's left, +z is up. The table top is z = 0.
- The gripper opens to 75 mm. "holding" is true when the closed fingers stopped on something.
- Two cameras. The overhead picture shows the whole table from above; detections are drawn on it (bottle,
  basket, the start spot as a green cross, the gripper as a magenta cross). The wrist picture looks along
  the gripper. The arm hides part of the table from the overhead camera, so the bottle's position is only
  refreshed by the `look` skill and after the gripper lets go of something.
- Heights: "carry" is the travelling height, clear of everything; "working" is down at the table (or down
  inside the basket) where the gripper grasps or lets go.

# How a pick and a place go

Pick: move_above the bottle, lower, close_gripper (check holding), lift.
Place: move_above the destination, lower, open_gripper, lift.

If close_gripper did not catch the bottle (holding is false), open_gripper, lift, look, and try again.
If a skill is refused, read the reason and choose differently. Do not repeat a refused skill unchanged.
Say done only when the whole instruction has been carried out.
"""


@dataclass
class Config:
    speed: int = 20                 # percent of the arm's speed
    carry_z: float = 250.0          # tool point height for travelling (raised if the basket needs more)
    grasp_z: float = 58.0           # tool point height when gripping the standing bottle (mid-body, near its centre of mass)
    grip_effort: int = 2500         # gripper effort, 0.001 N*m (0.6 let the bottle slip on the lift)
    release_extra: float = 4.0      # let go this far above where it was gripped
    basket_rim: float = 110.0       # height of the basket's rim above the table
    basket_floor: float = 10.0      # the basket's inside floor above the table
    basket_drop: bool = False       # True: let go above the rim instead of standing the bottle on the basket's floor
    bottle_height: float = P.BOTTLE_HEIGHT_MM
    return_tol: float = 40.0        # "where it started": within this distance
    max_steps: int = 40
    park: tuple = ((160.0, 0.0, 330.0), (110.0, 230.0, 300.0))    # where the arm waits during a look: back over its base, else left
    feel: bool = True               # check "upright" by closing the fingers on the cap
    stand_grip_from_centre: float = 25.0   # stand_up grips this far from a lying bottle's middle, towards the cap
    stand_effort: int = 1500        # firm enough to hold it, loose enough that it swings to hang
    bottle_length: float = 125.0    # base to the top of the cap
    max_steps_direct: int = 60      # direct mode takes more, smaller steps
    joint_speed_deg_s: float = 25.0 # direct mode's joint-space moves
    floor_direct: float = 8.0       # direct mode: the lowest a fingertip may go (the table is 0)

    @classmethod
    def load(cls):
        c = cls()
        try:
            for k, v in json.loads(CONFIG_PATH.read_text()).items():
                if hasattr(c, k):
                    setattr(c, k, tuple(map(tuple, v)) if k == "park" else v)
        except (OSError, ValueError):
            pass
        return c

    def to_dict(self):
        return asdict(self)

    @property
    def travel_z(self):
        """Carry height: a held bottle's base must clear the basket's rim."""
        return max(self.carry_z, self.basket_rim + self.grasp_z + 30.0)


def load_context():
    try:
        return CONTEXT_PATH.read_text(), False
    except OSError:
        return DEFAULT_CONTEXT, True


def save_context(text=None):
    HOME.mkdir(parents=True, exist_ok=True)
    if text is None:
        CONTEXT_PATH.unlink(missing_ok=True)
    else:
        CONTEXT_PATH.write_text(text)


class Refused(Exception):
    """The governor's no: the skill was not attempted."""


class World:
    """What is known about the scene, the skills that change it, and the governor."""

    def __init__(self, arm, overhead, wrist, cam, cfg=None, log=None, perceive=None):
        self.arm, self.overhead, self.wrist, self.cam = arm, overhead, wrist, cam
        self.cfg = cfg or Config.load()
        # (photograph, joints) -> perception.Scene; the simulator passes its own
        self.perceive = perceive or (lambda img, joints: P.look(img, self.cam, joints, self.cfg.bottle_height, self.cfg.basket_rim))
        self.log = log or (lambda *a: None)
        self.bottle = None            # {"where", "xy", "upright", "t"}
        self.basket = None            # {"xy", "t"}
        self.start_xy = None
        self.entered_basket = False
        self.basket_spot = None       # (x, y, z): where the gripper let the bottle go inside the basket
        self.gripper = "open"
        self.holding = False
        self.held_from = None         # "table" | "basket": where the held bottle was picked up
        self.grasped_z = None
        self.missed = False           # the last close_gripper caught nothing (cleared by the next look)
        self.just_released = False    # the bottle was let go and the gripper has not lifted away yet
        self.scene = None             # the last perception.Scene
        self.steps_used = 0
        self.last_frames = {}         # {"overhead": bgr, "wrist": bgr}
        self.events = []              # things that happened, for the verdict: ("knocked_over", t) ...

    # ---- where things are -----------------------------------------------------------------------------
    def tool(self):
        st = self.arm.state()
        return st["tool"], st

    def _target_xy(self, target):
        if target == "bottle":
            if self.bottle and self.bottle["where"] == "basket" and self.basket_spot:
                return self.basket_spot[:2]
            return self.bottle["xy"] if self.bottle and self.bottle.get("xy") else None
        if target == "basket":
            return self.basket["xy"] if self.basket else None
        if target == "start_spot":
            return self.start_xy
        return None

    def above(self, tool=None):
        tool = tool or self.tool()[0]
        best, dist = "nothing", 30.0
        for name in ("bottle", "basket", "start_spot"):
            xy = self._target_xy(name)
            if name == "bottle" and self.holding:
                continue
            if xy is not None and math.dist(tool[:2], xy) < dist:
                best, dist = name, math.dist(tool[:2], xy)
        return best

    def height(self, tool=None):
        tool = tool or self.tool()[0]
        if tool[2] >= self.cfg.travel_z - 15.0:
            return "carry"
        return "working" if tool[2] <= self._working_z(self.above(tool)) + 12.0 else "between"

    def _working_z(self, above):
        c = self.cfg
        if above == "basket" or (above == "bottle" and self.bottle and self.bottle["where"] == "basket"):
            if self.holding:
                return (c.basket_rim + c.grasp_z + 15.0) if c.basket_drop else (c.basket_floor + c.grasp_z + c.release_extra)
            return self.basket_spot[2] if self.basket_spot else c.basket_floor + c.grasp_z
        if self.holding:
            return (self.grasped_z if self.held_from == "table" and self.grasped_z else c.grasp_z) + c.release_extra
        return c.grasp_z

    def state(self):
        """The flags the model is given (API.md: State)."""
        tool, _ = self.tool()
        now = time.time()

        def seen(o):
            return None if o is None else round(now - o["t"], 1)

        return {
            "tool_xyz_mm": [round(v) for v in tool], "gripper": self.gripper, "holding": self.holding,
            "bottle": None if not self.bottle else {
                "where": "gripper" if self.holding else self.bottle["where"], "upright": self.bottle.get("upright"),
                "xy_mm": None if self.bottle.get("xy") is None else [round(v) for v in self.bottle["xy"]],
                "seen_s_ago": seen(self.bottle)},
            "basket": None if not self.basket else {"xy_mm": [round(v) for v in self.basket["xy"]], "seen_s_ago": seen(self.basket)},
            "start_spot_xy_mm": None if self.start_xy is None else [round(v) for v in self.start_xy],
            "bottle_has_been_in_basket": self.entered_basket, "last_grasp_missed": self.missed,
            "just_released": self.just_released,
            "above": self.above(tool), "height": self.height(tool),
            "steps_used": self.steps_used, "steps_left": max(0, self.cfg.max_steps - self.steps_used),
        }

    # ---- the governor ---------------------------------------------------------------------------------
    def check(self, skill, target, s=None):
        """Raise Refused with the reason, or return."""
        s = s or self.state()
        if skill not in SKILLS:
            raise Refused(f"there is no skill called {skill}")
        carry, working = s["height"] == "carry", s["height"] == "working"
        if skill == "look" and not carry:
            raise Refused("look needs the arm at carry height: lift first")
        if skill == "move_above":
            if target not in ("bottle", "basket", "start_spot"):
                raise Refused("move_above needs a target: bottle, basket or start_spot")
            if not carry:
                raise Refused("move_above only travels at carry height: lift first")
            if target == "bottle" and s["holding"]:
                raise Refused("the bottle is in the gripper: it cannot be a place to go to")
            if target == "bottle" and s["bottle"] and s["bottle"]["where"] == "table" and s["bottle"]["upright"] is False:
                raise Refused("the bottle is lying on its side: stand_up first")
            if self._target_xy(target) is None:
                raise Refused(f"the position of {target} is not known: look first")
        if skill == "lower":
            if not carry:
                raise Refused("lower starts from carry height")
            if s["above"] == "nothing":
                raise Refused("the gripper is not above the bottle, the basket or the start spot: move_above first")
            if s["above"] == "bottle" and (s["holding"] or s["gripper"] != "open"):
                raise Refused("lowering onto the bottle needs an open, empty gripper")
            if s["above"] == "start_spot" and not s["holding"]:
                raise Refused("there is nothing in the gripper to set down on the start spot")
            if s["above"] == "basket" and not s["holding"] and not (self.bottle and self.bottle["where"] == "basket"):
                raise Refused("the gripper is empty and the bottle is not in the basket: nothing to do down there")
            if s["above"] == "basket" and not s["holding"] and s["gripper"] != "open":
                raise Refused("open the gripper before lowering it onto the bottle")
        if skill == "close_gripper":
            if s["gripper"] != "open":
                raise Refused("the gripper is already closed")
            if not working:
                raise Refused("closing in mid-air catches nothing: lower first")
            if self.just_released:
                raise Refused("the bottle was only just let go here: lift away before grasping it again")
        if skill == "open_gripper" and s["gripper"] != "closed":
            raise Refused("the gripper is already open")
        if skill == "lift" and carry:
            raise Refused("already at carry height")
        if skill == "stand_up":
            b = s["bottle"]
            if not b or b["where"] != "table" or b["upright"] is not False:
                raise Refused("stand_up is for a bottle lying on the table, and it is not lying there")
            if s["holding"] or s["gripper"] != "open":
                raise Refused("stand_up needs an open, empty gripper")
            if not carry:
                raise Refused("stand_up starts from carry height: lift first")
            if self.bottle.get("axis_deg") is None:
                raise Refused("which way the bottle lies is not known: look first")

    # ---- the skills -----------------------------------------------------------------------------------
    def do(self, skill, target="none"):
        """Check, then carry out one skill. Returns a sentence about what happened. Raises Refused (not
        attempted), arm.ArmError (attempted, failed) or arm.Stop."""
        self.check(skill, target)
        return getattr(self, "_" + skill)(target)

    def _go(self, x, y, z, **kw):
        return self.arm.move_tool(x, y, z, speed=self.cfg.speed, **kw)

    def _look(self, _target=None, parks=None):
        found = None
        for park in (parks or self.cfg.park):
            self._go(*park, lean=40.0, min_tip_z=self.cfg.travel_z - 60.0)
            self.arm.hold()
            time.sleep(0.5)
            img = self.overhead.settled(3)
            joints = self.arm.state()["joints"]
            sc = self.perceive(img, joints)
            self.last_frames["overhead"] = img
            self.scene = sc
            self.missed = False
            now = time.time()
            if sc.basket:
                self.basket = {"xy": sc.basket["xy"], "t": now, "size_mm": sc.basket["size_mm"]}
            if sc.bottle and not self.holding:
                self.bottle = {"where": sc.bottle["where"], "xy": sc.bottle["xy"], "upright": sc.bottle.get("upright"), "t": now,
                               **{k: sc.bottle[k] for k in ("axis_deg", "length_mm") if k in sc.bottle}}
                if sc.bottle["where"] == "basket":
                    self.entered_basket = True
                found = sc.bottle
                break
        if found is None and not self.holding:
            self.bottle = {"where": "unknown", "xy": None, "upright": None, "t": time.time()}
            return "looked: the basket " + ("is" if self.basket else "is not") + " in view, the bottle was not found"
        if self.holding:
            return "looked: the bottle is in the gripper"
        where = "in the basket" if found["where"] == "basket" else f"on the table at ({found['xy'][0]:.0f}, {found['xy'][1]:.0f})"
        return f"looked: the bottle is {where}"

    def _move_above(self, target):
        x, y = self._target_xy(target)
        self._go(x, y, self.cfg.travel_z, min_tip_z=self.cfg.travel_z - 60.0)
        return f"above {target} at carry height"

    def _lower(self, _target=None):
        tool, st = self.tool()
        above = self.above(tool)
        z = self._working_z(above)
        x, y = self._target_xy(above)
        self._go(x, y, z, min_tip_z=z - 25.0)
        return f"lowered to {z:.0f} mm above {above}"

    def _close_gripper(self, _target=None):
        w = self.arm.grip(0.0, effort=self.cfg.grip_effort)
        self.gripper = "closed"
        self.holding = self.arm.holding()
        if self.holding:
            tool, _ = self.tool()
            self.grasped_z = tool[2]
            self.held_from = "basket" if (self.bottle and self.bottle["where"] == "basket") else "table"
            return f"closed on something {w:.0f} mm wide: holding"
        self.missed = True
        return "closed on nothing: the fingers met"

    def _open_gripper(self, _target=None):
        tool, _ = self.tool()
        was_holding, above = self.holding, self.above(tool)
        self.arm.grip(A.GRIPPER_OPEN_MM)
        self.gripper, self.holding = "open", False
        if not was_holding:
            return "opened (it held nothing)"
        self.just_released = True
        now = time.time()
        if above == "basket":
            self.entered_basket = True
            self.basket_spot = (tool[0], tool[1], tool[2])
            self.bottle = {"where": "basket", "xy": [tool[0], tool[1]], "upright": None, "t": now}
            return "let go of the bottle in the basket"
        self.bottle = {"where": "table", "xy": [tool[0], tool[1]], "upright": None, "t": now}
        return f"let go of the bottle at ({tool[0]:.0f}, {tool[1]:.0f})"

    def _lift(self, _target=None):
        tool, st = self.tool()
        self._go(tool[0], tool[1], self.cfg.travel_z, lean=None, min_tip_z=tool[2] - 30.0)
        self.just_released = False
        if self.gripper == "closed":
            still = self.arm.holding()
            if self.holding and not still:
                self.holding = False
                self.bottle = {"where": "unknown", "xy": None, "upright": None, "t": time.time()}
                return "lifted, but the bottle slipped out of the fingers"
            return "lifted" + (" with the bottle" if self.holding else "")
        # the gripper is free and something may have changed: look now, while the arm is up
        return "lifted. " + self._look()

    def _stand_up(self, _target=None):
        """A lying bottle, stood up by letting it hang: grip it across the body near the cap end (the fingers
        close across its axis), lift until it swings down to hang base first, set the base on the table, let
        go. Its axis runs along the tool's lean direction, so either of the two directions along it will do."""
        c, b = self.cfg, self.bottle
        a = math.radians(b["axis_deg"])
        gx, gy = b["xy"][0] + c.stand_grip_from_centre * math.cos(a), b["xy"][1] + c.stand_grip_from_centre * math.sin(a)
        hang = c.bottle_length / 2.0 + c.stand_grip_from_centre        # grip point to the base, hanging
        low = P.LYING_AXIS_Z + 2.0
        yaw = None
        for cand in sorted((b["axis_deg"], b["axis_deg"] + 180.0),
                           key=lambda y: abs(((y - math.degrees(math.atan2(gy, gx))) + 180.0) % 360.0 - 180.0)):
            if A.min_lean(gx, gy, low, cand) is not None and A.min_lean(gx, gy, c.travel_z, cand) is not None:
                yaw = cand
                break
        if yaw is None:
            raise A.ArmError(f"the arm cannot reach down to the lying bottle at ({gx:.0f}, {gy:.0f}) with its fingers across it")
        floor = self.arm.floor_z_mm
        try:
            self._go(gx, gy, c.travel_z, yaw_deg=yaw, min_tip_z=c.travel_z - 60.0)
            self.arm.floor_z_mm = 4.0                            # the fingertips go down to the table here
            self._go(gx, gy, low, yaw_deg=yaw, min_tip_z=low - 25.0)
            self.arm.grip(0.0, effort=c.stand_effort)
            if not self.arm.holding():
                self.arm.grip(A.GRIPPER_OPEN_MM)
                self._go(gx, gy, c.travel_z, yaw_deg=yaw)
                self.missed = True
                return "stand_up: the fingers closed on nothing; the bottle is still lying"
            self.gripper, self.holding = "closed", True
            self._go(gx, gy, hang + 45.0, yaw_deg=yaw, speed=max(8, c.speed // 2))     # it swings down to hang
            time.sleep(1.5)
            self._go(gx, gy, hang + 4.0, yaw_deg=yaw, speed=max(8, c.speed // 2))      # base onto the table
            self.arm.grip(A.GRIPPER_OPEN_MM)
            self.gripper, self.holding = "open", False
            time.sleep(0.5)
            self._go(gx, gy, c.travel_z, yaw_deg=yaw, min_tip_z=hang - 30.0)
        finally:
            self.arm.floor_z_mm = floor
        return "stand_up: set down on its base and let go. " + self._look()

    def _done(self, _target=None):
        return "the model says the task is complete"

    # ---- checks that are not skills ---------------------------------------------------------------------
    def feel_upright(self):
        """Is a bottle standing at the place the cameras put it? Close the fingers at cap height: a standing
        bottle stops them, a fallen one is not that tall. Returns True, False or None (could not check)."""
        if not self.bottle or self.bottle["where"] != "table" or not self.bottle.get("xy") or self.holding:
            return None
        x, y = self.bottle["xy"]
        z = self.cfg.bottle_height - 18.0
        try:
            if self.gripper != "open":
                self.arm.grip(A.GRIPPER_OPEN_MM)
                self.gripper = "open"
            self._go(x, y, self.cfg.travel_z, min_tip_z=self.cfg.travel_z - 60.0)
            self._go(x, y, z, min_tip_z=z - 25.0)
            self.arm.grip(0.0, effort=300)
            standing = self.arm.holding()
            self.arm.grip(A.GRIPPER_OPEN_MM)
            self._go(x, y, self.cfg.travel_z, min_tip_z=z - 25.0)
            return bool(standing)
        except A.ArmError as e:
            self.log(f"feel check failed: {e}")
            return None

    # ---- direct mode: the model moves the arm; this keeps track of the bottle ------------------------------
    def observe(self, img=None):
        """Run the detector on a fresh overhead picture (the arm stays where it is) and update what is known.
        Returns the picture."""
        if img is None:
            img = self.overhead.frame()
        self.last_frames["overhead"] = img
        sc = self.perceive(img, self.arm.state()["joints"])
        self.scene = sc
        now = time.time()
        if sc.basket:
            self.basket = {"xy": sc.basket["xy"], "t": now, "size_mm": sc.basket["size_mm"]}
        if sc.bottle and not self.holding:
            self.bottle = {"where": sc.bottle["where"], "xy": sc.bottle["xy"], "upright": sc.bottle.get("upright"), "t": now,
                           **{k: sc.bottle[k] for k in ("axis_deg", "length_mm") if k in sc.bottle}}
            if sc.bottle["where"] == "basket":
                self.entered_basket = True
        return img

    def in_basket(self, xy):
        if not self.basket or xy is None:
            return False
        a, b = self.basket.get("size_mm") or (300, 220)
        return math.dist(xy, self.basket["xy"]) < 0.5 * min(a, b) + 20.0

    def note_release(self):
        """The fingers opened on the bottle: where did it go?"""
        tool, _ = self.tool()
        now = time.time()
        if self.in_basket(tool[:2]):
            self.entered_basket = True
            self.basket_spot = (tool[0], tool[1], tool[2])
            self.bottle = {"where": "basket", "xy": [tool[0], tool[1]], "upright": None, "t": now}
        else:
            self.bottle = {"where": "table", "xy": [tool[0], tool[1]], "upright": None, "t": now}

    def after_motion(self):
        if self.holding:
            if self.gripper == "closed" and not self.arm.holding():
                self.holding = False
                self.bottle = {"where": "unknown", "xy": None, "upright": None, "t": time.time()}
            else:
                tool, _ = self.tool()
                self.bottle = {"where": "gripper", "xy": [tool[0], tool[1]], "upright": None, "t": time.time()}
                if self.in_basket(tool[:2]) and tool[2] < self.cfg.basket_rim + self.cfg.grasp_z:
                    self.entered_basket = True

    def frames(self, grid=False):
        """(overhead annotated bgr, wrist bgr or None): what the model is shown."""
        tool, _ = self.tool()
        try:
            img = self.overhead.frame()
        except Exception:                                   # noqa: BLE001 (a camera hiccup must not end a run)
            img = self.last_frames.get("overhead")
        if img is not None and grid:
            from .direct import grid as draw_grid
            img = draw_grid(img, self.cam)
        ann = P.draw(img, self.cam, self._scene_for_drawing(), tool, self.start_xy) if img is not None else None
        wrist = None
        try:
            wrist = self.wrist.frame() if self.wrist else None
        except Exception:                                   # noqa: BLE001
            pass
        return ann, wrist

    def _scene_for_drawing(self):
        sc = P.Scene()
        if self.scene and self.scene.basket:
            sc.basket = self.scene.basket
        if self.bottle and self.bottle.get("xy") and not self.holding:
            z = self.cfg.bottle_height if self.bottle["where"] == "table" else 80.0
            u, v = self.cam.project([self.bottle["xy"][0], self.bottle["xy"][1], z])
            sc.bottle = {"where": self.bottle["where"], "px": [float(u), float(v)]}
        return sc


def prompt(instruction, context, state, history):
    """The text half of one decision. history: [(skill, target, note, ok, detail), ...], oldest first."""
    lines = [context.strip(), "", "# The instruction", instruction.strip(), "", "# Skills"]
    lines += [f"- {k}: {v}" for k, v in SKILLS.items()]
    lines += ["", "# State", json.dumps(state, indent=1), "", "# What happened so far in this run (oldest first)"]
    if not history:
        lines.append("(nothing yet: this is the first decision)")
    for skill, target, note, ok, detail in history[-10:]:
        t = f"({target})" if target and target != "none" else ""
        lines.append(f"- {skill}{t}: {'ok' if ok else 'NOT DONE'}. {detail}".rstrip())
    lines += ["", "Image 1 is the overhead camera with detections drawn on it; image 2 is the wrist camera.",
              "Choose the next skill. `target` matters only for move_above (use none otherwise). In `note`, say in "
              "one or two sentences what you see and why you chose it."]
    return "\n".join(lines)


def oracle(state):
    """The obvious next skill for the default task, from the state alone: (skill, target, note). It drives
    the arm with no model in the loop: for testing the skills, and for tidying the scene between runs."""
    s = state
    carry = s["height"] == "carry"
    b = s.get("bottle")
    if s["holding"]:
        dest = "start_spot" if s["bottle_has_been_in_basket"] else "basket"
        if s["above"] == dest:
            return ("lower", "none", "above the destination") if carry else ("open_gripper", "none", "down: let go")
        return ("move_above", dest, "carrying the bottle") if carry else ("lift", "none", "holding: up first")
    if s["gripper"] == "closed":
        return "open_gripper", "none", "closed on nothing"
    if (s.get("last_grasp_missed") or s.get("just_released")) and not carry:
        return "lift", "none", "go up and look again"
    if not carry and not (s["above"] in ("bottle", "basket") and s["height"] == "working"):
        return "lift", "none", "get to carry height"
    if b is None or b["where"] == "unknown" or b.get("xy_mm") is None:
        return ("look", "none", "where is the bottle?") if carry else ("lift", "none", "up, to look")
    if b["where"] == "table" and b.get("upright") is False:
        return ("stand_up", "none", "the bottle is lying: stand it up") if carry else ("lift", "none", "up first")
    if s["bottle_has_been_in_basket"] and b["where"] == "table":
        home = s.get("start_spot_xy_mm")
        if home is None or math.dist(b["xy_mm"], home) <= 30.0:
            return ("done", "none", "the bottle went in and is back on the table") if carry else ("lift", "none", "finish high")
    if s["above"] == "bottle" or (b["where"] == "basket" and s["above"] == "basket"):
        return ("lower", "none", "onto the bottle") if carry else ("close_gripper", "none", "grasp")
    return ("move_above", "bottle", "go to the bottle") if carry else ("lift", "none", "up first")
