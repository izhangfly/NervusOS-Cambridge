"""Direct mode on the pretend arm: the model's motions, the checks under them, and a whole cycle."""
import json
import sys
import tempfile
import threading
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from gptarm import arm as A, direct as D, llm, runner, sim, task  # noqa: E402


def mv(x, y, z, **kw):
    return {"action": "move_tool", "x": x, "y": y, "z": z, "lean": kw.get("lean"), "yaw": kw.get("yaw"),
            "joints": None, "gripper_mm": None, "note": kw.get("note", "")}


def grip(mm):
    return {"action": "gripper", "x": None, "y": None, "z": None, "lean": None, "yaw": None, "joints": None,
            "gripper_mm": mm, "note": ""}


DONE = {"action": "done", "x": None, "y": None, "z": None, "lean": None, "yaw": None, "joints": None,
        "gripper_mm": None, "note": "finished"}


class DirectTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.t = sim.Table()
        cam = sim.camera()
        self.arm = sim.SimArm(self.t)
        self.make = lambda: task.World(self.arm, sim.SimOverhead(self.t, cam), sim.SimWrist(), cam, task.Config(),
                                       perceive=sim.perceive(self.t))

    def tearDown(self):
        self.tmp.cleanup()

    def test_the_checks_refuse_and_say_why(self):
        w = self.make()
        w.arm.floor_z_mm = 8.0
        with self.assertRaises(A.ArmError) as e:
            D.execute(w, mv(300, 0, 10))
        self.assertIn("below the floor", str(e.exception))
        with self.assertRaises(A.ArmError) as e:
            D.execute(w, {**DONE, "action": "move_joints", "joints": [0, 200, -60, 0, 45, 0]})
        self.assertIn("J2", str(e.exception))
        with self.assertRaises(D.Refused):
            D.execute(w, {**DONE, "action": "gripper", "gripper_mm": 120})
        with self.assertRaises(D.Refused):
            D.execute(w, mv(None, 0, 100))
        out = D.execute(w, {**DONE, "action": "move_joints", "joints": [10, 60, -70, 0, 50, 0]})
        self.assertIn("reached", out)

    def test_a_scripted_cycle_in_direct_mode(self):
        bx, by = self.t.bottle["xy"]
        kx, ky = self.t.basket["xy"]
        plan = [mv(bx, by, 250), mv(bx, by, 60), grip(0), mv(bx, by, 250), mv(kx, ky, 250), mv(kx, ky, 72), grip(75),
                mv(kx, ky, 250), mv(kx, ky, 72), grip(0), mv(kx, ky, 250), mv(bx, by, 250), mv(bx, by, 62), grip(75),
                mv(bx, by, 250), DONE]
        prompts = []

        def policy(p, images, schema):
            prompts.append(p)
            self.assertEqual(schema, D.SCHEMA)
            self.assertEqual(set(images), {"overhead", "wrist"})
            return plan[len(prompts) - 1]

        run = runner.Run("scripted", task.DEFAULT_INSTRUCTION, root=Path(self.tmp.name))
        d = runner.run_cycle(self.make(), llm.Scripted(policy), task.DEFAULT_INSTRUCTION, D.DEFAULT_CONTEXT, run,
                             threading.Event(), mode="direct")
        self.assertEqual((d["outcome"]["success"], d["outcome"]["reason"]), (True, ""), json.dumps(d["outcome"]))
        self.assertEqual(d["mode"], "direct")
        first = json.loads(prompts[0].split("# Now\n", 1)[1].split("\n\n#", 1)[0])
        self.assertEqual(len(first["joints_deg"]), 6)
        self.assertEqual(first["detector_guess"]["bottle"]["xy_mm"], [330, 200])
        self.assertIn("holding", prompts[3])
        self.assertIn("gripper 0 mm: done. the fingers are now", prompts[3])
        dec = [s for s in d["steps"] if s["kind"] == "decision"]
        self.assertEqual(dec[0]["skill"], "move_tool")
        self.assertEqual(dec[0]["target"], f"({bx:.0f}, {by:.0f}, 250)")


if __name__ == "__main__":
    unittest.main()
