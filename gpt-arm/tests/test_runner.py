"""A whole run and a small benchmark on the pretend arm, with the scripted policy in place of a model."""
import json
import sys
import tempfile
import threading
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from gptarm import llm, runner, sim, task  # noqa: E402


def setup(table=None):
    t = table or sim.Table()
    cam = sim.camera()
    arm = sim.SimArm(t)
    make = lambda: task.World(arm, sim.SimOverhead(t, cam), sim.SimWrist(), cam, task.Config(), perceive=sim.perceive(t))  # noqa: E731
    return t, make


class RunnerTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.root = Path(self.tmp.name)

    def tearDown(self):
        self.tmp.cleanup()

    def cycle(self, t, make, policy=runner.scripted_policy):
        run = runner.Run("scripted", task.DEFAULT_INSTRUCTION, root=self.root)
        return runner.run_cycle(make(), llm.Scripted(policy), task.DEFAULT_INSTRUCTION, task.DEFAULT_CONTEXT, run, threading.Event())

    def test_a_clean_run_succeeds_and_is_recorded(self):
        t, make = setup()
        d = self.cycle(t, make)
        self.assertEqual((d["state"], d["outcome"]["success"], d["outcome"]["reason"]), ("done", True, ""))
        self.assertEqual(d["outcome"]["checks"]["entered_basket"], True)
        self.assertLess(d["outcome"]["checks"]["returned_mm"], 5)
        self.assertTrue(d["outcome"]["checks"]["upright"])
        self.assertEqual(d["usage"]["llm_calls"], 17)
        kinds = [s["kind"] for s in d["steps"]]
        self.assertEqual(kinds[:2], ["user", "perception"])
        self.assertEqual(kinds[-1], "verdict")
        on_disk = json.loads((self.root / d["id"] / "run.json").read_text())
        self.assertEqual(on_disk["outcome"]["success"], True)
        step = next(s for s in d["steps"] if s["kind"] == "decision")
        self.assertTrue((self.root / d["id"] / step["images"]["overhead"]).exists())
        self.assertEqual([s["n"] for s in d["steps"]], list(range(len(d["steps"]))))

    def test_a_bottle_that_falls_over_fails_the_run(self):
        t, make = setup()
        t.topple = 1
        d = self.cycle(t, make)
        self.assertEqual((d["outcome"]["success"], d["outcome"]["reason"]), (False, "tipped_over"))

    def test_done_too_early_fails(self):
        t, make = setup()
        d = self.cycle(t, make, lambda p, i, s: {"skill": "done", "target": "none", "note": "all good"})
        self.assertEqual((d["outcome"]["success"], d["outcome"]["reason"]), (False, "never_entered"))

    def test_refusals_are_fed_back_and_counted(self):
        t, make = setup()
        seen = []

        def policy(p, i, s):
            seen.append(p)
            if len(seen) == 1:
                return {"skill": "close_gripper", "target": "none", "note": "grab"}
            return runner.scripted_policy(p, i, s)

        d = self.cycle(t, make, policy)
        self.assertTrue(d["outcome"]["success"])
        self.assertEqual(d["usage"]["refusals"], 1)
        self.assertIn("refused: closing in mid-air", seen[1])
        self.assertFalse(d["steps"][2]["ok"])

    def test_a_stop_ends_the_run(self):
        t, make = setup()
        stop = threading.Event()
        n = [0]

        def policy(p, i, s):
            n[0] += 1
            if n[0] == 4:
                stop.set()
            return runner.scripted_policy(p, i, s)

        run = runner.Run("scripted", "x", root=self.root)
        w = make()
        w.arm.stop_event = stop
        d = runner.run_cycle(w, llm.Scripted(policy), "x", task.DEFAULT_CONTEXT, run, stop)
        self.assertEqual(d["state"], "stopped")
        w.arm.stop_event = threading.Event()

    def test_a_benchmark_recovers_the_scene_and_pauses_when_it_cannot(self):
        t, make = setup()
        t.topple = 1                                   # the first run leaves the bottle lying: needs a hand
        b = runner.Bench(["scripted"], 3, task.DEFAULT_INSTRUCTION, root=self.root)
        stop = threading.Event()
        th = threading.Thread(target=b.run, args=(make, task.DEFAULT_CONTEXT, stop, None, lambda m: None), daemon=True)
        th.start()
        for _ in range(400):
            if b.d["state"] == "paused":
                break
            th.join(0.05)
        self.assertEqual(b.d["state"], "paused")
        self.assertIn("upright", b.d["message"])
        t.bottle.update(upright=True, where="table", xy=[330.0, 200.0])      # the hand
        b.resume_event.set()
        th.join(30)
        self.assertEqual(b.d["state"], "done")
        r = b.d["results"][0]
        self.assertEqual((r["runs"], r["successes"], r["failures"]), (3, 2, {"tipped_over": 1}))
        self.assertTrue((b.dir / "bench.json").exists())


if __name__ == "__main__":
    unittest.main()
