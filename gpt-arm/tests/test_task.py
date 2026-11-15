"""The skills, the governor and the scripted policy, on the pretend arm."""
import sys
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from gptarm import sim, task  # noqa: E402


def world(**kw):
    t = sim.Table(**kw)
    cam = sim.camera()
    w = task.World(sim.SimArm(t), sim.SimOverhead(t, cam), sim.SimWrist(), cam, task.Config(), perceive=sim.perceive(t))
    return w, t


def drive(w, limit=60):
    """Follow the scripted policy to `done`; the skills taken."""
    taken = []
    for _ in range(limit):
        skill, target, _ = task.oracle(w.state())
        taken.append(skill if target == "none" else f"{skill}:{target}")
        try:
            w.do(skill, target)
        except task.Refused as e:
            raise AssertionError(f"the policy chose a refused skill {skill}({target}): {e}; so far {taken}") from None
        w.steps_used += 1
        if skill == "done":
            return taken
    raise AssertionError(f"no done within {limit} steps: {taken}")


class TaskTest(unittest.TestCase):
    def test_a_clean_cycle(self):
        w, t = world()
        w.do("look")
        w.start_xy = list(w.bottle["xy"])
        taken = drive(w)
        self.assertEqual(taken, ["move_above:bottle", "lower", "close_gripper", "lift", "move_above:basket", "lower",
                                 "open_gripper", "lift", "move_above:bottle", "lower", "close_gripper", "lift",
                                 "move_above:start_spot", "lower", "open_gripper", "lift", "done"])
        self.assertTrue(w.entered_basket)
        self.assertEqual(t.bottle["where"], "table")
        self.assertTrue(t.bottle["upright"])
        self.assertLess(abs(t.bottle["xy"][0] - 330) + abs(t.bottle["xy"][1] - 200), 2)
        self.assertTrue(w.feel_upright())

    def test_a_missed_grasp_is_retried(self):
        w, t = world()
        w.do("look")
        w.start_xy = list(w.bottle["xy"])
        t.miss = 1
        taken = drive(w)
        self.assertEqual(taken[:6], ["move_above:bottle", "lower", "close_gripper", "open_gripper", "lift", "move_above:bottle"])
        self.assertTrue(t.bottle["upright"] and t.bottle["where"] == "table" and w.entered_basket)

    def test_the_governor_says_why(self):
        w, _ = world()
        for skill, target, word in (("lower", "none", "not above"), ("close_gripper", "none", "mid-air"),
                                    ("move_above", "bottle", "not known"), ("move_above", "none", "target"),
                                    ("open_gripper", "none", "already open"), ("lift", "none", "already at carry")):
            with self.assertRaises(task.Refused) as r:
                w.do(skill, target)
            self.assertIn(word, str(r.exception))
        w.do("look")
        w.do("move_above", "start_spot") if w.start_xy else None
        w.do("move_above", "bottle")
        w.do("lower")
        with self.assertRaises(task.Refused):
            w.do("move_above", "basket")          # not at carry height
        with self.assertRaises(task.Refused):
            w.do("look")

    def test_a_fallen_bottle_is_felt(self):
        w, t = world()
        w.do("look")
        t.bottle["upright"] = False
        self.assertFalse(w.feel_upright())

    def test_prompt_carries_refusals(self):
        w, _ = world()
        text = task.prompt("Do it.", task.DEFAULT_CONTEXT, w.state(), [("lower", "none", "n", False, "refused: not above anything")])
        self.assertIn("NOT DONE", text)
        self.assertIn("refused: not above anything", text)
        self.assertIn('"steps_left"', text)


if __name__ == "__main__":
    unittest.main()
