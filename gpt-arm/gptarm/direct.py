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

ACTIONS = ("move_tool", "move_joints", "gripper", "done")
_num = {"anyOf": [{"type": "number"}, {"type": "null"}]}
SCHEMA = {
    "type": "object", "additionalProperties": False,
    "required": ["action", "x", "y", "z", "lean", "yaw", "joints", "gripper_mm", "note"],
    "properties": {
        "action": {"type": "string", "enum": list(ACTIONS)},
        "x": _num, "y": _num, "z": _num, "lean": _num, "yaw": _num,
        "joints": {"anyOf": [{"type": "array", "items": {"type": "number"}}, {"type": "null"}]},
        "gripper_mm": _num,
        "note": {"type": "string"},
    },
}

DEFAULT_CONTEXT = """\
# The arm

You drive an AgileX PiPER: six joints and a two-finger gripper, bolted to the near edge of a white table.
You see the table through two cameras and you move the arm one motion at a time. Code carries out each
motion slowly and refuses one that would hit the table, leave a joint's range or go out of reach; it tells
you why. Nothing else plans for you.

Coordinates are millimetres from the arm's base: +x forward (away from the base, across the table), +y to
the arm's left, +z up. The table top is z = 0.

## Motions (one per answer; the fields a motion does not use are null)

- move_tool: put the TOOL POINT (the point midway between the two finger pads) at x, y, z. The tool points
  down. `lean` (degrees, optional) tips the fingertips outward, away from the base, about the line the
  fingers close along: the pads stay level. Where the wrist cannot reach with a vertical tool, the smallest
  lean that works is used anyway; leave lean null unless you want more. `yaw` (degrees, optional) is the
  direction, in the table plane, that the tool leans towards; the fingers close at right angles to it.
  Null yaw means the bearing of the point from the base (x axis = 0, +y = 90), so the fingers close
  sideways. The arm moves there in a path of its own choosing; it does not travel in a straight line.
- move_joints: set all six joint angles (degrees), moved together along a straight line in joint space.
  J1 turns the arm about the vertical (positive turns it to the left, +y). J2 is the shoulder, J3 the elbow,
  J4 rolls the forearm, J5 bends the wrist, J6 turns the gripper about its own axis.
  Ranges: J1 -150..150, J2 0..180, J3 -170..0, J4 -100..100, J5 -70..70, J6 -100..100.
- gripper: open or close the fingers: gripper_mm from 0 (closed) to 75 (fully open). After a close,
  `holding` tells you whether the fingers stopped on something.
- done: you believe the instruction has been carried out. The result is then checked by the cameras and by
  touch.

## The cameras

- Overhead: the whole table from above. A green grid is drawn on the table top every 100 mm, labelled in
  the arm's x and y. The magenta cross is the tool point. Yellow and blue marks are an automatic detector's
  guesses (bottle, basket); they can be wrong, and the arm hides part of the table.
- Wrist: looks down along the gripper, between the fingers.

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
    return {
        "joints_deg": [round(v, 1) for v in st["joints"]],
        "tool_point_mm": [round(v) for v in tool], "tool_lean_deg": round(lean), "tool_yaw_deg": round(yaw),
        "gripper_mm": None if st["gripper_mm"] is None else round(float(st["gripper_mm"]), 1),
        "holding": bool(w.holding),
        "detector_guess": det or None,
        "bottle_start_xy_mm": None if w.start_xy is None else [round(v) for v in w.start_xy],
        "steps_used": w.steps_used, "steps_left": max(0, w.cfg.max_steps_direct - w.steps_used),
    }


def prompt(instruction, context, st, history):
    lines = [context.strip(), "", "# The instruction", instruction.strip(), "", "# Now", json.dumps(st, indent=1),
             "", "# Your previous motions in this run (oldest first)"]
    if not history:
        lines.append("(none yet: this is the first)")
    for desc, ok, detail in history[-14:]:
        lines.append(f"- {desc}: {'done' if ok else 'NOT DONE'}. {detail}".rstrip())
    lines += ["", "Image 1 is the overhead camera, image 2 the wrist camera.",
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
        world.gripper = "closed" if float(mm) < 3.0 else "open"
        was = world.holding
        world.holding = arm.holding() if float(mm) < 3.0 else False
        if was and not world.holding:
            world.note_release()
        return f"the fingers are now {w:.0f} mm apart" + ("; they stopped on something: holding" if world.holding else
                                                           ("; they closed on nothing" if float(mm) < 3.0 else ""))
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
