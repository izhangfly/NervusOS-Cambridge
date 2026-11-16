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
    "required": ["action", "x", "y", "z", "lean", "yaw", "grip", "joints", "gripper_mm", "skill", "expect", "note"],
    "properties": {
        "action": {"type": "string", "enum": list(ACTIONS)},
        "x": _num, "y": _num, "z": _num, "lean": _num, "yaw": _num, "grip": _num,
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

A backboard (a white sheet against the wall) stands just past the far edge of the table, at about x = 480
(a little nearer at the sides).
No part of the arm may touch it, and the wrist camera sticks out about 4 cm past the fingers on the far side
of the gripper (when the tool leans outward) and is the first thing to hit it. Code refuses a motion that would
bring any part within 15 mm of the backboard, and stops one that gets that close on the way;
`nearest_wall` in the state says how far the closest part is now. Things pushed against the backboard
cannot be reached from above with the tool leaning outward: lean less, or pull them towards you first.

## Motions (one per answer; the fields a motion does not use are null)

- move_tool: put the TOOL POINT (the point midway between the two finger pads) at x, y, z. The tool points
  down. `lean` (degrees, optional) tips the fingertips outward, away from the base, about the line the
  fingers close along: the pads stay level. Where the wrist cannot reach with a vertical tool, or would come
  too near the backboard, the smallest lean that works is used anyway; leave lean null unless you want more.
  `yaw` (degrees, optional) is the direction, in the table plane, that the tool leans towards. Null yaw
  means the bearing of the point from the base (x axis = 0, +y = 90). The arm can only lean roughly away
  from its base: a yaw more than about 50 degrees from the bearing is usually out of reach, so leave yaw
  null. `grip` (degrees, optional) is the direction, in the table plane, of the line the two fingers close
  along: the gripper turns about its own axis to match (grip and grip + 180 are the same). Null grip: at
  right angles to the yaw. To close across an object lying down, set grip to its long axis + 90. The arm
  moves there in a path of its own choosing; it does not travel in a straight line.
- move_by: move the tool point BY x, y, z millimetres from where it is now (relative; null counts as 0). The
  tool keeps its present lean, yaw and grip unless you give new ones. Best for small corrections, and for
  going straight down onto something once the depth map's ring is on it.
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
- Wrist: the colour camera on the side of the gripper, looking down past the two fingers (the dark shapes at
  the bottom corners). It is not mirrored: UP in the wrist picture is the tool's yaw direction (away from the
  base when yaw is null), LEFT is 90 degrees counter-clockwise from that seen from above; a compass in the
  corner shows where the arm's +x and +y point. It looks at the fingers from the side and at an angle, so
  where something appears relative to the fingers depends on how far below them it is: from high up, an
  object that looks "between the fingers" is usually NOT under them. Use it to see what things are, not
  to line them up.
- Wrist depth map (image 3, when there is one): MEASURED, and the picture to line up a grasp with. The depth
  camera's view turned into a map of the table seen from straight above, centred on the tool point: +x up,
  +y left (like the overhead picture), 320 x 320 mm, coloured by height above the table (scale at the
  bottom; black = not seen), with a grid every 50 mm labelled in the arm's x and y. The white ring is the tool
  point's x, y: what is under the ring is what the fingers close on when you lower the tool straight down
  (move_by with only z keeps x and y). The two white dots are the open fingertips and the white line between
  them is the line they close along: it must lie ACROSS the object. Objects that stand out from the floor
  around them are labelled D1, D2, ... (listed in `wrist_depth.objects_below`).

- Wrist depth numbers (`wrist_depth` in the state): `surface_under_tool_z_mm` is the height of whatever is
  straight under the tool point (null if the depth camera cannot see there from this pose) and
  `fingertips_above_it_mm` how far the fingertips are above it. `objects_below` lists what rises from the
  floor around it, nearest first: centre (x, y), top height, length and width, `long_axis_deg` (the direction
  of its long side in the table plane: 0 = +x, 90 = +y), its offset from the tool point, and whether the view
  cuts it off (then it may be bigger). A bottle lying down is about 125 x 45 mm with its top about 45 mm
  above what it lies on; a basket wall is long, thin and about 100 mm high. Trust these over the detector's
  guesses and over estimates from the pictures. The depth camera reads nothing closer than about 15 cm and
  little from black fabric: `seen: false` means no reading, not an empty table.
- The depth camera looks AHEAD of the gripper (towards the yaw direction): what is right under the tool point
  and behind it (towards the base) is often out of its view. The map therefore keeps what it saw at earlier
  steps of this run, drawn DIMMER ("remembered"); it is forgotten whenever the gripper opens or closes.
  Positions are in the arm's coordinates and stay true after the arm moves: you can look at an object from a
  little behind it, read its centre and long axis, then move the tool point straight to that centre.

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
            import time
            from . import depth as Dp
            frame = w.depth.settled(3)
            if getattr(w, "depth_memory", None) is None:
                w.depth_memory = Dp.DepthMemory()
            wd = Dp.summary(frame, st["flange"], w.mount, memory=w.depth_memory)
            w.last_depth = (frame, st["flange"], wd, time.time())      # image 3 is drawn from the same frame
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
        "fingers_close_along_deg": None if st.get("grip") is None else round(st["grip"]),
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
    """The compass on the wrist picture (see the context's description). (A fixed mark for the fingertips was
    drawn here until 2026-11-16: it is right only at the fingertips' own depth, and the model kept lining
    objects far below up with it.)"""
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
              + (", image 3 the wrist depth map (seen from above, centred on the tool point)." if depth_image else "."),
              "Choose the next motion. In `note`, say in one or two sentences what you see and why."]
    return "\n".join(lines)


def describe(ans):
    a = ans.get("action")
    if a == "move_tool":
        extra = "".join(f" {k} {ans[k]:.0f}" for k in ("lean", "yaw", "grip") if ans.get(k) is not None)
        return f"move_tool ({_f(ans.get('x'))}, {_f(ans.get('y'))}, {_f(ans.get('z'))}){extra}"
    if a == "move_joints":
        return "move_joints [" + ", ".join(_f(v, 1) for v in (ans.get("joints") or [])) + "]"
    if a == "gripper":
        return f"gripper {_f(ans.get('gripper_mm'))} mm"
    if a == "move_by":
        extra = "".join(f" {k} {ans[k]:.0f}" for k in ("lean", "yaw", "grip") if ans.get(k) is not None)
        return f"move_by ({_f(ans.get('x') or 0)}, {_f(ans.get('y') or 0)}, {_f(ans.get('z') or 0)}){extra}"
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
        world.depth_memory = None                 # what was where may have changed: the depth map starts again
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
        # relative: the tool keeps its lean, yaw and grip unless the answer gives new ones
        keep = {"lean": st["lean"], "yaw": st.get("yaw"), "grip": st.get("grip")}
        ans = {**ans, "x": st["tool"][0] + d[0], "y": st["tool"][1] + d[1], "z": st["tool"][2] + d[2],
               **{k: v for k, v in keep.items() if ans.get(k) is None and v is not None}}
        a = "move_tool"
    if a == "move_tool":
        x, y, z = (ans.get(k) for k in ("x", "y", "z"))
        if None in (x, y, z):
            raise Refused("move_tool needs x, y and z")
        lean, yaw, grip = ans.get("lean"), ans.get("yaw"), ans.get("grip")
        if lean is not None and not 0.0 <= float(lean) <= A.LEAN_MAX_DEG:
            raise Refused(f"lean must be 0 to {A.LEAN_MAX_DEG:.0f} degrees")
        arm.move_tool(float(x), float(y), float(z), lean=None if lean is None else float(lean),
                      yaw_deg=None if yaw is None else float(yaw), speed=cfg.speed, min_tip_z=arm.floor_z_mm,
                      grip_deg=None if grip is None else float(grip))
    if a == "move_joints":
        q = ans.get("joints")
        if not q or len(q) != 6:
            raise Refused("move_joints needs six angles")
        arm.move_joints([float(v) for v in q], max_deg_s=cfg.joint_speed_deg_s)
    world.after_motion()
    st = arm.state()
    return (f"reached: tool point now ({st['tool'][0]:.0f}, {st['tool'][1]:.0f}, {st['tool'][2]:.0f}), lean {st['lean']:.0f}"
            + (f", yaw {st['yaw']:.0f}, fingers close along {st['grip']:.0f}" if st.get("grip") is not None else ""))
