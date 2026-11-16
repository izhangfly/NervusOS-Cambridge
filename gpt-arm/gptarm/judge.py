"""Did it work? A model looks at the table before and after a free-form instruction and says.

Only the bottle cycle can be checked by code (cameras and touch). Any other instruction typed in the portal is
judged here by the same kind of model, from the pictures taken before the run and after it (arm parked out of
view), the instruction and a summary of what the arm did. The prompt is adapted from PragmaBot's success
detector (leggedrobotics/pragmabot, vlm_success_detector.py, BSD-3-Clause): realistic, not harsh, and anything
knocked over or dropped by accident is a failure. The answer has three outcomes (success, fail, impossible)
as in Agent Workflow Memory's evaluator (zorazrw/agent-workflow-memory, Apache-2.0), and a critique the
driving model can act on, as in Voyager's critic (MineDojo/Voyager, MIT).
"""
from __future__ import annotations

SCHEMA = {"type": "object", "additionalProperties": False,
          "required": ["before", "after", "changes", "status", "failure_kind", "explanation", "critique", "confidence"],
          "properties": {
              "before": {"type": "string"}, "after": {"type": "string"}, "changes": {"type": "string"},
              "status": {"type": "string", "enum": ["success", "fail", "impossible"]},
              "failure_kind": {"anyOf": [{"type": "string"}, {"type": "null"}]},
              "explanation": {"type": "string"},
              "critique": {"anyOf": [{"type": "string"}, {"type": "null"}]},
              "confidence": {"type": "number"}}}

PROMPT = """\
You judge whether a robot arm (an AgileX PiPER with a two-finger gripper, on a white table) carried out an
instruction. Be realistic but not overly harsh: the arm has limited accuracy. Any accident (knocking something
over, dropping something that was not meant to be dropped, pushing something off the table) is a failure even
if the instruction does not mention it. The instruction may also be a question or a gesture (for example
"wave" or "point at the basket"): then judge the motions described, and the answer the arm's model gave.

# The instruction
{instruction}

# What the arm did (its own log)
{log}

# The model driving the arm finally said
{claim}

{images}

Describe the table before and after, list what changed, then decide:
- success: the instruction was carried out (well enough);
- fail: it was not, or something was knocked over or broken; say what went wrong in `failure_kind` (a few words)
  and in `critique` what the arm should do next to fix it;
- impossible: it cannot be done with this arm and this scene (say why).
`confidence` is 0 to 1.
"""


def judge(llm, instruction, log, claim, images):
    """-> (verdict dict, usage). images: {"before", "after", "wrist"} JPEG bytes (any may be missing) and
    "during": [(step number, JPEG bytes)], overhead pictures taken soon after the fingers let go of something
    (runner.release_views), which show where things were put on the way."""
    imgs, said = {}, []
    if images.get("before"):
        imgs["before"] = images["before"]
        said.append(f"{len(imgs)} = overhead camera BEFORE the run")
    for n, data in images.get("during") or []:
        imgs[f"during-{n}"] = data
        said.append(f"{len(imgs)} = overhead camera DURING the run, soon after the gripper let go of something at "
                    f"step {n} (the arm may hide part of the table)")
    for k, what in (("after", "overhead camera AFTER (arm parked)"), ("wrist", "wrist camera AFTER")):
        if images.get(k):
            imgs[k] = images[k]
            said.append(f"{len(imgs)} = {what}")
    text = "Images: " + ("; ".join(said) if said else "none") + "."
    return llm.ask(PROMPT.format(instruction=instruction.strip(), log=log, claim=claim or "(nothing)", images=text),
                   imgs, SCHEMA)
