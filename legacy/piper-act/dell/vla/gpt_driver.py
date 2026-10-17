#!/usr/bin/env python3
"""
Drive the PiPER directly from a frontier vision model (default: GPT-6 Astra).

This is the "VLM in the loop" approach rather than a trained VLA: each step the
model sees a camera frame plus the arm's measured pose and calls one tool. It
needs no GPU, no dataset and no fine-tuning, so it is the fastest way to get
language-driven manipulation running — and it doubles as the teleoperator for
collecting the demonstrations a real VLA (SmolVLA, pi0, OpenVLA) would train on.

What it is NOT: fast or precise. Every step is a round-trip to a large model, so
expect a few seconds per action and open-loop-ish behaviour between frames. For
closed-loop 10-50 Hz control you want a distilled policy; see README.md.

    setx OPENAI_API_KEY "sk-..."        (once, then reopen the shell)
    python gpt_driver.py "pick up the red block and put it in the bowl"
    python gpt_driver.py --no-arm "describe what you see"     # vision only
"""

import argparse
import json
import os
import sys
import time

from camera import Camera

MODEL = os.environ.get("PIPER_VLM_MODEL", "gpt-6-astra")

SYSTEM = """\
You control a 6-DOF AgileX PiPER robot arm with a parallel gripper, watching \
through a fixed RGB camera.

Frame of reference (millimetres, right-handed, base at the origin):
  +X is away from the robot base (forward), +Y is to the robot's left,
  +Z is up. Rotations RX/RY/RZ are degrees.

How to work:
  - Look at the image and the reported pose, then call exactly ONE tool.
  - Move in small steps (<= 40 mm) and re-look. You cannot see between steps.
  - The gripper opens to 70 mm and closes to 0 mm.
  - Approach objects from above: get over the target, then descend, then close.
  - If a move does not change the pose the way you expected, you are probably at
    a joint limit — back off and approach from a different angle rather than
    repeating the same command.
  - Call `done` as soon as the task is achieved, or if it is impossible.

Be decisive and brief. One tool call per turn."""

TOOLS = [
    {
        "type": "function",
        "function": {
            "name": "move_delta",
            "description": ("Move the gripper by a relative offset in mm/degrees. "
                            "Each component is clamped to +-60 mm / +-30 deg."),
            "parameters": {
                "type": "object",
                "properties": {
                    "dx": {"type": "number", "description": "mm, + is forward"},
                    "dy": {"type": "number", "description": "mm, + is left"},
                    "dz": {"type": "number", "description": "mm, + is up"},
                    "drx": {"type": "number"},
                    "dry": {"type": "number"},
                    "drz": {"type": "number"},
                    "why": {"type": "string", "description": "one short clause"},
                },
                "required": ["why"],
            },
        },
    },
    {
        "type": "function",
        "function": {
            "name": "set_gripper",
            "description": "Set gripper opening in mm (0 closed, 70 fully open).",
            "parameters": {
                "type": "object",
                "properties": {
                    "mm": {"type": "number"},
                    "why": {"type": "string"},
                },
                "required": ["mm", "why"],
            },
        },
    },
    {
        "type": "function",
        "function": {
            "name": "go_ready",
            "description": ("Return to the READY pose. Use this to recover if the "
                            "arm seems stuck or badly positioned."),
            "parameters": {
                "type": "object",
                "properties": {"why": {"type": "string"}},
                "required": ["why"],
            },
        },
    },
    {
        "type": "function",
        "function": {
            "name": "done",
            "description": "The task is complete, or cannot be done. Stops the loop.",
            "parameters": {
                "type": "object",
                "properties": {
                    "success": {"type": "boolean"},
                    "summary": {"type": "string"},
                },
                "required": ["success", "summary"],
            },
        },
    },
]


def observation(arm, cam, task, step, total):
    """One user turn: the task, the current pose, and what the camera sees."""
    if arm is not None:
        pose = [round(v, 1) for v in arm.pose()]
        status = arm.status()[0]
        grip = "unknown"
        text = (f"Task: {task}\n"
                f"Step {step}/{total}\n"
                f"Gripper pose: X={pose[0]} Y={pose[1]} Z={pose[2]} "
                f"RX={pose[3]} RY={pose[4]} RZ={pose[5]}\n"
                f"Arm status: {status}\n"
                f"Gripper opening: {grip}")
    else:
        text = f"Task: {task}\nStep {step}/{total}\n(vision only, no arm attached)"
    return [
        {"type": "text", "text": text},
        {"type": "image_url",
         "image_url": {"url": "data:image/jpeg;base64," + cam.read_jpeg_b64()}},
    ]


def run(task, steps, use_arm, speed, model, verbose):
    try:
        from openai import OpenAI
    except ImportError:
        sys.exit("pip install openai")
    if not os.environ.get("OPENAI_API_KEY"):
        sys.exit("OPENAI_API_KEY is not set.\n"
                 '  setx OPENAI_API_KEY "sk-..."   then open a new shell')

    client = OpenAI()
    arm = None
    if use_arm:
        from arm import PiperArm
        arm = PiperArm(speed=speed)
        arm.enable()
        print("arm ready at", [round(v, 1) for v in arm.ready()])

    cam = Camera()
    print(f"camera: {cam.name} (index {cam.index}) | model: {model}\n")

    messages = [{"role": "system", "content": SYSTEM}]
    try:
        for step in range(1, steps + 1):
            messages.append({"role": "user",
                             "content": observation(arm, cam, task, step, steps)})
            resp = client.chat.completions.create(
                model=model, messages=messages, tools=TOOLS, tool_choice="required")
            msg = resp.choices[0].message
            messages.append(msg)

            if not msg.tool_calls:
                print("model returned no tool call; stopping")
                break
            call = msg.tool_calls[0]
            args = json.loads(call.function.arguments or "{}")
            name = call.function.name
            print(f"[{step}] {name}({', '.join('%s=%s' % kv for kv in args.items())})")

            if name == "done":
                print("\n%s: %s" % ("SUCCESS" if args.get("success") else "GAVE UP",
                                    args.get("summary", "")))
                break

            result = "ok"
            if arm is None:
                result = "no arm attached (vision-only run); pretend it worked"
            elif name == "move_delta":
                p = arm.move_delta(**{k: v for k, v in args.items() if k != "why"})
                result = "new pose " + str([round(v, 1) for v in p])
                if arm.is_soft_faulted():
                    result += " (target was unreachable — at a joint limit)"
            elif name == "set_gripper":
                result = "gripper now %.1f mm" % arm.set_gripper(args["mm"])
            elif name == "go_ready":
                result = "at ready " + str([round(v, 1) for v in arm.ready()])

            if verbose:
                print("     ->", result)
            messages.append({"role": "tool", "tool_call_id": call.id,
                             "content": result})
        else:
            print("\nstep budget exhausted")
    finally:
        cam.close()
        if arm is not None:
            arm.close()


def main():
    ap = argparse.ArgumentParser(description=__doc__,
                                 formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("task", help="what the arm should do, in plain language")
    ap.add_argument("--steps", type=int, default=15, help="max tool calls")
    ap.add_argument("--speed", type=int, default=35, help="arm speed 1-100")
    ap.add_argument("--model", default=MODEL)
    ap.add_argument("--no-arm", action="store_true",
                    help="run the vision loop without moving the arm")
    ap.add_argument("-q", "--quiet", action="store_true")
    a = ap.parse_args()
    run(a.task, a.steps, not a.no_arm, a.speed, a.model, not a.quiet)


if __name__ == "__main__":
    main()
