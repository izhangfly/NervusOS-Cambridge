"""Direct mode: the model drives the arm itself.

Each step the model gets both camera pictures, the joint angles, the tool point, the gripper, and what its
previous motions did; it answers with ONE motion: a tool point to go to (x, y, z mm, optional lean and yaw),
six joint angles, a gripper width, or done. Nothing here plans for it. What this module adds is the floor
under it: a motion is refused if a joint would leave its range, a fingertip would go into the table (on the
way, not only at the end), the wrist would rise into the camera, or the point is out of reach; the reason
goes back to the model. Motions are slow (a fixed speed) and each is checked as it runs.

The overhead picture carries a millimetre grid drawn on the table (the arm's own coordinates, from the
calibration) and, marked as guesses, what the detector found. The model may use them or not.
"""
from __future__ import annotations

import json
import math
import os
from pathlib import Path

import cv2
import numpy as np

from . import arm as A
from . import perception as P

HOME = Path(os.environ.get("GPTARM_HOME", "~/.config/gptarm")).expanduser()
CONTEXT_PATH = HOME / "context-direct.md"

ACTIONS = ("move_tool", "move_by", "move_joints", "gripper", "skill", "done")
SKILLS = {"look": "move the arm aside, photograph the table, and come back with fresh detections",
          "stand_up": "stand a bottle that lies on the table back up (grips it near the cap, lets it hang, sets it on its base)"}
_num = {"anyOf": [{"type": "number"}, {"type": "null"}]}
_str = {"anyOf": [{"type": "string"}, {"type": "null"}]}
SCHEMA = {
    "type": "object", "additionalProperties": False,
    "required": ["action", "x", "y", "z", "lean", "yaw", "joints", "gripper_mm", "skill", "expect", "note"],
    "properties": {
        "action": {"type": "string", "enum": list(ACTIONS)},
        "x": _num, "y": _num, "z": _num, "lean": _num, "yaw": _num,
        "joints": {"anyOf": [{"type": "array", "items": {"type": "number"}}, {"type": "null"}]},
        "gripper_mm": _num,
        "skill": {"anyOf": [{"type": "string", "enum": list(SKILLS)}, {"type": "null"}]},
        "expect": _str,
        "note": {"type": "string"},
    },
}
PLAN_SCHEMA = {"type": "object", "additionalProperties": False, "required": ["stages"],
               "properties": {"stages": {"type": "array", "items": {
                   "type": "object", "additionalProperties": False, "required": ["goal", "done_when"],
                   "properties": {"goal": {"type": "string"}, "done_when": {"type": "string"}}}}}}

DEFAULT_CONTEXT = """\
# The arm

You drive an AgileX PiPER: six joints and a two-finger gripper, bolted to the near edge of a white table.
You see the table through two cameras and you move the arm one motion at a time. Code carries out each
motion slowly and refuses one that would hit the table, leave a joint's range or go out of reach; it tells
you why. Nothing else plans for you.

Coordinates are millimetres from the arm's base: +x forward (away from the base, across the table), +y to
the arm's left, +z up. The table top is z = 0.

A backboard (a white sheet against the wall) stands just past the far edge of the table, at about x = 500.
No part of the arm may touch it, and the wrist camera sticks out about 4 cm past the fingers on the far side
of the gripper (when the tool leans outward) and is the first thing to hit it. Code refuses a motion that would
bring any part within 15 mm of the backboard, and stops one that gets that close on the way;
`nearest_wall` in the state says how far the closest part is now. Things pushed against the backboard
cannot be reached from above with the tool leaning outward: lean less, turn the yaw, or pull them
towards you first.

## Motions (one per answer; the fields a motion does not use are null)

- move_tool: put the TOOL POINT (the point midway between the two finger pads) at x, y, z. The tool points
  down. `lean` (degrees, optional) tips the fingertips outward, away from the base, about the line the
  fingers close along: the pads stay level. Where the wrist cannot reach with a vertical tool, or would come
  too near the backboard, the smallest lean that works is used anyway; leave lean null unless you want more. `yaw` (degrees, optional) is the
  direction, in the table plane, that the tool leans towards; the fingers close at right angles to it.
  Null yaw means the bearing of the point from the base (x axis = 0, +y = 90), so the fingers close
  sideways. The arm moves there in a path of its own choosing; it does not travel in a straight line.
- move_by: move the tool point BY x, y, z millimetres from where it is now (relative; null counts as 0).
  Best for small corrections you read off the wrist picture ("the bottle is 15 mm to the left").
- move_joints: set all six joint angles (degrees), moved together along a straight line in joint space.
  J1 turns the arm about the vertical (positive turns it to the left, +y). J2 is the shoulder, J3 the elbow,
  J4 rolls the forearm, J5 bends the wrist, J6 turns the gripper about its own axis.
  Ranges: J1 -150..150, J2 0..180, J3 -170..0, J4 -100..100, J5 -70..70, J6 -100..100.
- gripper: open or close the fingers: gripper_mm from 0 (closed) to 75 (fully open). After a close,
  `holding` tells you whether the fingers stopped on something.
- skill: run a ready-made routine named in `skill`: "look" (move the arm aside, photograph the table, refresh
  the detector) or "stand_up" (stand a bottle that lies on the table back up). Use them or not, as you see fit.
- done: you believe the instruction has been carried out. The result is then checked by the cameras and by
  touch.

## The cameras

- Overhead: the whole table from above. A green grid is drawn on the table top every 100 mm, labelled in
  the arm's x and y. The magenta cross is the tool point. Yellow and blue marks are an automatic detector's
  guesses (bottle, basket); white boxes labelled O1, O2, ... are everything else that stands out from the
  table (listed in `objects_on_table` with position, size and colour; positions are where the object's middle
  meets the table, so a tall object's base is a little nearer the middle of the picture). They can be wrong,
  and the arm hides part of the table.
- Wrist: looks down along the gripper, between the two fingers (the dark shapes at the bottom corners).
  It is not mirrored: UP in the wrist picture is the tool's yaw direction (away from the base when yaw is
  null), LEFT is 90 degrees counter-clockwise from that seen from above. A compass drawn in the corner
  shows where the arm's +x and +y point, and a cyan mark shows ROUGHLY where the point between the fingertips
  is (it is fixed in the picture and drifts with height: trust the depth picture's ring instead). Close to an
  object, 1 mm is roughly 3 to 4 pixels.
- Wrist depth picture (image 3, when there is one): the same view as distance, warm = near, cool = far,
  black = no reading, flat grey = below the table top (the floor past the table's edge), with a millimetre
  scale. Drawn on it from the arm's exact geometry: a white ring where
  the point between the fingertips is, two white dots for the open fingertips, and a white cross on the
  surface straight below the tool point with that surface's height. If the ring sits on the object you want
  to grasp, the fingers will close around it. This is measured; prefer it to the colour picture's cyan mark.

- Wrist depth (`wrist_depth` in the state, when the depth camera is calibrated): measured, not guessed.
  `surface_under_tool_z_mm` is the height of whatever is straight under the tool point and
  `fingertips_above_it_mm` how far the fingertips are above it; `nearest_object` is the highest thing below
  the gripper within 20 cm: its top's centre (x, y), its top height, its extent, and its offset from the tool
  point. Trust it over the detector's guesses and over estimates from the pictures. The depth camera reads
  nothing closer than about 15 cm and little from black fabric: `seen: false` means no reading, not an
  empty table.

Fill `expect` with what you expect to see after the motion; the next step shows it next to what happened.

## Things on the table

- A hair gel bottle: a cylinder about 45 mm wide and 125 mm tall with a black cap on top; the fingers can
  close around its body. It falls over easily: move slowly near it, and do not push it sideways.
- A basket: a soft black fabric box, about 30 x 22 cm, its rim roughly 10 to 15 cm high. It is not fixed
  to the table.
"""


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


def grid(img, cam, step=100, x_range=(0, 500), y_range=(-400, 400)):
    """The table's millimetre grid drawn on the overhead picture, labelled."""
    out = img.copy()
    h, w = out.shape[:2]
    for x in range(x_range[0], x_range[1] + 1, step):
        a, b = cam.project([x, y_range[0], 0.0]), cam.project([x, y_range[1], 0.0])
        cv2.line(out, tuple(map(int, a)), tuple(map(int, b)), (60, 200, 60), 1, cv2.LINE_AA)
    for y in range(y_range[0], y_range[1] + 1, step):
        a, b = cam.project([x_range[0], y, 0.0]), cam.project([x_range[1], y, 0.0])
        cv2.line(out, tuple(map(int, a)), tuple(map(int, b)), (60, 200, 60), 1, cv2.LINE_AA)
    for x in range(x_range[0] + step, x_range[1] + 1, step):
        for y in range(y_range[0] + step, y_range[1], step * 2):
            u, v = cam.project([x, y, 0.0])
            if 0 <= u < w - 60 and 12 <= v < h:
                cv2.putText(out, f"{x},{y}", (int(u) + 3, int(v) - 3), cv2.FONT_HERSHEY_SIMPLEX, 0.42, (40, 150, 40), 1, cv2.LINE_AA)
    return out


def state(world):
    """What the model is told besides the pictures."""
    st = world.arm.state()
    tool, lean, yaw = A.tool_from_flange(st["flange"])
    w = world
    det = {}
    if w.scene is not None:
        b = w.scene.bottle
        det["bottle"] = None if not b else {"xy_mm": [round(v) for v in b["xy"]], "where": b["where"],
                                             "standing": b.get("upright")}
        det["basket"] = None if not w.scene.basket else {"xy_mm": [round(v) for v in w.scene.basket["xy"]],
                                                         "size_mm": w.scene.basket["size_mm"]}
    wd = None
    if getattr(w, "depth", None) is not None and getattr(w, "mount", None) is not None:
        try:
            from . import depth as Dp
            wd = Dp.summary(w.depth.settled(3), st["flange"], w.mount)
        except Exception as e:                               # noqa: BLE001 (no depth this step)
            wd = {"seen": False, "error": str(e)[:120]}
    walls = getattr(w.arm, "walls", None)
    near = None
    if walls:
        gap, part, wall = A.wall_clearance(st["flange"], walls)
        near = {"wall": wall, "closest_part": part, "clearance_mm": round(gap)}
    return {
        "nearest_wall": near,
        "joints_deg": [round(v, 1) for v in st["joints"]],
        "tool_point_mm": [round(v) for v in tool], "tool_lean_deg": round(lean), "tool_yaw_deg": round(yaw),
        "gripper_mm": None if st["gripper_mm"] is None else round(float(st["gripper_mm"]), 1),
        "holding": bool(w.holding),
        "detector_guess": det or None,
        "objects_on_table": [{k: o[k] for k in ("label", "xy_mm", "size_mm", "colour")} for o in getattr(w, "objects", [])] or None,
        "wrist_depth": wd,
        "bottle_start_xy_mm": None if w.start_xy is None else [round(v) for v in w.start_xy],
        "steps_used": w.steps_used, "steps_left": max(0, w.cfg.max_steps_direct - w.steps_used),
    }


PLAN_PROMPT = """{context}

# The instruction
{instruction}

Before moving, write a short plan for carrying out the instruction with this arm: 2 to 8 stages, each with a
goal and a condition you can check in the camera pictures (or from `holding`) that says the stage is done.
The pictures show the table now."""


def plan_prompt(instruction, context):
    return PLAN_PROMPT.format(context=context.strip(), instruction=instruction.strip())


def plan_text(plan):
    if not plan:
        return ""
    return "\n".join(f"{n}. {s['goal']} (done when: {s['done_when']})" for n, s in enumerate(plan, 1))


def wrist_overlay(img, yaw_deg):
    """The compass and the fingertip mark on the wrist picture (see the context's description)."""
    if img is None:
        return None
    out = img.copy()
    h, w = out.shape[:2]
    cx, cy, r = w - 90, 90, 60
    cv2.circle(out, (cx, cy), r + 18, (30, 30, 30), -1)
    for name, ang, col in (("+x", 0.0, (60, 60, 255)), ("+y", 90.0, (60, 220, 60))):
        a = math.radians(ang - yaw_deg)
        dx, dy = -math.sin(a), -math.cos(a)
        tip = (int(cx + dx * r), int(cy + dy * r))
        cv2.arrowedLine(out, (cx, cy), tip, col, 3, cv2.LINE_AA, tipLength=0.25)
        cv2.putText(out, name, (int(cx + dx * (r + 4)) - 12, int(cy + dy * (r + 4)) + 6), cv2.FONT_HERSHEY_SIMPLEX,
                    0.6, (255, 255, 255), 2, cv2.LINE_AA)
    u, v = int(w * 0.505), int(h * 0.83)                      # between the fingertips, measured on this arm
    cv2.drawMarker(out, (u, v), (255, 255, 0), cv2.MARKER_CROSS, 34, 2, cv2.LINE_AA)
    cv2.putText(out, "between fingertips", (u + 20, v - 12), cv2.FONT_HERSHEY_SIMPLEX, 0.6, (255, 255, 0), 2, cv2.LINE_AA)
    return out


def prompt(instruction, context, st, history, plan=None, depth_image=None):
    """depth_image: whether a depth picture goes with the prompt as image 3 (None: when depth saw something)."""
    if depth_image is None:
        depth_image = bool(st.get("wrist_depth") and st["wrist_depth"].get("seen"))
    lines = [context.strip(), "", "# The instruction", instruction.strip()]
    if plan:
        lines += ["", "# Your plan (written at the start; follow it, or depart from it if the pictures say so)", plan_text(plan)]
    lines += ["", "# Now", json.dumps(st, indent=1), "", "# Your previous motions in this run (oldest first)"]
    if not history:
        lines.append("(none yet: this is the first)")
    for item in history[-14:]:
        desc, ok, detail = item[:3]
        expect = item[3] if len(item) > 3 else None
        lines.append(f"- {desc}: {'done' if ok else 'NOT DONE'}. {detail}".rstrip()
                     + (f" (you expected: {expect})" if expect else ""))
    lines += ["", "Image 1 is the overhead camera, image 2 the wrist camera"
              + (", image 3 the wrist depth camera." if depth_image else "."),
              "Choose the next motion. In `note`, say in one or two sentences what you see and why."]
    return "\n".join(lines)


def describe(ans):
    a = ans.get("action")
    if a == "move_tool":
        extra = "".join(f" {k} {ans[k]:.0f}" for k in ("lean", "yaw") if ans.get(k) is not None)
        return f"move_tool ({_f(ans.get('x'))}, {_f(ans.get('y'))}, {_f(ans.get('z'))}){extra}"
    if a == "move_joints":
        return "move_joints [" + ", ".join(_f(v, 1) for v in (ans.get("joints") or [])) + "]"
    if a == "gripper":
        return f"gripper {_f(ans.get('gripper_mm'))} mm"
    if a == "move_by":
        return f"move_by ({_f(ans.get('x') or 0)}, {_f(ans.get('y') or 0)}, {_f(ans.get('z') or 0)})"
    if a == "skill":
        return f"skill {ans.get('skill')}"
    return str(a)


def _f(v, nd=0):
    return "?" if v is None else f"{float(v):.{nd}f}"


class Refused(Exception):
    pass


def execute(world, ans):
    """Carry out one motion. Returns a sentence; raises Refused (not attempted) or arm.ArmError (attempted)."""
    a, arm, cfg = ans.get("action"), world.arm, world.cfg
    if a not in ACTIONS:
        raise Refused(f"unknown action {a!r}")
    if a == "done":
        return "you said done"
    if a == "gripper":
        mm = ans.get("gripper_mm")
        if mm is None or not 0.0 <= float(mm) <= A.GRIPPER_OPEN_MM + 0.5:
            raise Refused("gripper needs gripper_mm between 0 and 75")
        w = arm.grip(float(mm), effort=cfg.grip_effort)
        closing = float(mm) < A.GRIPPER_OPEN_MM - 10.0
        world.gripper = "closed" if closing else "open"
        was = world.holding
        world.holding = arm.holding() if closing else False
        if was and not world.holding:
            world.note_release()
        return f"the fingers are now {w:.0f} mm apart" + ("; they stopped on something: holding" if world.holding else
                                                           ("; they closed on nothing" if closing else ""))
    if a == "skill":
        name = ans.get("skill")
        if name not in SKILLS:
            raise Refused(f"skill must be one of {', '.join(SKILLS)}")
        from .task import Refused as SkillRefused
        try:
            if name == "look":
                tool, _ = world.tool()
                if tool[2] < world.cfg.travel_z - 15.0:
                    world._go(tool[0], tool[1], world.cfg.travel_z, min_tip_z=tool[2] - 30.0)
                out = world._look()
            else:
                world.gripper = "closed" if (arm.state()["gripper_mm"] or 0) < 3.0 else world.gripper
                out = world.do(name)
        except SkillRefused as e:
            raise Refused(str(e)) from None
        return out
    if a == "move_by":
        st = arm.state()
        d = [float(ans.get(k) or 0.0) for k in ("x", "y", "z")]
        if max(abs(v) for v in d) > 300.0:
            raise Refused("move_by is for moves of up to 300 mm per axis")
        ans = {**ans, "x": st["tool"][0] + d[0], "y": st["tool"][1] + d[1], "z": st["tool"][2] + d[2]}
        a = "move_tool"
    if a == "move_tool":
        x, y, z = (ans.get(k) for k in ("x", "y", "z"))
        if None in (x, y, z):
            raise Refused("move_tool needs x, y and z")
        lean, yaw = ans.get("lean"), ans.get("yaw")
        if lean is not None and not 0.0 <= float(lean) <= A.LEAN_MAX_DEG:
            raise Refused(f"lean must be 0 to {A.LEAN_MAX_DEG:.0f} degrees")
        arm.move_tool(float(x), float(y), float(z), lean=None if lean is None else float(lean),
                      yaw_deg=None if yaw is None else float(yaw), speed=cfg.speed, min_tip_z=arm.floor_z_mm)
    if a == "move_joints":
        q = ans.get("joints")
        if not q or len(q) != 6:
            raise Refused("move_joints needs six angles")
        arm.move_joints([float(v) for v in q], max_deg_s=cfg.joint_speed_deg_s)
    world.after_motion()
    st = arm.state()
    return f"reached: tool point now ({st['tool'][0]:.0f}, {st['tool'][1]:.0f}, {st['tool'][2]:.0f}), lean {st['lean']:.0f}"
