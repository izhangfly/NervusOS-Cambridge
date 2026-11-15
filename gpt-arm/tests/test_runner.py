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

    def test_a_bottle_that_falls_over_is_stood_up_and_put_back(self):
        t, make = setup()
        t.topple = 1
        d = self.cycle(t, make)
        self.assertEqual((d["outcome"]["success"], d["outcome"]["reason"]), (True, ""))
        self.assertIn("stand_up", [s.get("skill") for s in d["steps"]])

    def test_a_bottle_that_cannot_be_stood_up_fails_the_run(self):
        t, make = setup()
        t.topple, t.miss = 1, 99
        d = self.cycle(t, make)
        self.assertEqual(d["outcome"]["success"], False)

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
        t.bottle.update(upright=False, xy=[300.0, 150.0])   # lying, and the arm cannot get hold of it
        t.miss = 99
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
        t.miss = 0
        b.resume_event.set()
        th.join(60)
        self.assertEqual(b.d["state"], "done")
        r = b.d["results"][0]
        self.assertEqual((r["runs"], r["successes"], r["failures"]), (3, 3, {}))
        self.assertTrue((b.dir / "bench.json").exists())


class FreeAndLessonsTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.root = Path(self.tmp.name)
        import os
        from gptarm import lessons
        lessons.PATH = self.root / "lessons.json"
        lessons.HOME = self.root

    def tearDown(self):
        self.tmp.cleanup()

    def test_a_free_instruction_is_not_checked_and_records_a_video(self):
        t, make = setup()
        run = runner.Run("scripted", "Wave hello", "direct", root=self.root)
        done = {"action": "done", "x": None, "y": None, "z": None, "lean": None, "yaw": None, "joints": None,
                "gripper_mm": None, "note": "I waved."}
        d = runner.run_cycle(make(), llm.Scripted(lambda p, i, s: done), "Wave hello", "ctx", run, threading.Event(),
                             mode="direct")
        self.assertEqual((d["outcome"]["success"], d["outcome"]["reason"]), (None, "not_checked"))
        v = [s for s in d["steps"] if s["kind"] == "verdict"][0]
        self.assertIsNone(v["ok"])
        self.assertEqual(v["detail"], "I waved.")
        self.assertEqual(d["video"]["state"], "ready", d["video"])
        self.assertTrue((self.root / d["id"] / "video.mp4").stat().st_size > 1000)

    def test_lessons_are_learned_and_shown_next_time(self):
        from gptarm import lessons
        t, make = setup()

        class Learner(llm.Scripted):                    # a "real" model as far as the runner is concerned
            pass

        prompts = []

        def policy(p, images, schema):
            prompts.append(p)
            if schema is lessons.SCHEMA:
                return {"operations": [{"op": "ADD", "index": None, "text": "Lying bottles must be gripped at table level."}]}
            return runner.scripted_policy(p, images, schema)

        m = Learner(policy)
        m.__class__ = type("Real", (object,), {"ask": lambda self, p, i, s: (policy(p, i, s), {"tokens": 5, "seconds": 0})})
        run = runner.Run("m", task.DEFAULT_INSTRUCTION, root=self.root)
        d = runner.run_cycle(make(), m, task.DEFAULT_INSTRUCTION, task.DEFAULT_CONTEXT, run, threading.Event(),
                             lessons_on=True, record=False)
        self.assertEqual(d["steps"][-1]["kind"], "lesson")
        self.assertIn("new: Lying bottles", d["steps"][-1]["note"])
        self.assertEqual(lessons.state()["count"], 1)
        run2 = runner.Run("m", task.DEFAULT_INSTRUCTION, root=self.root)
        d2 = runner.run_cycle(make(), m, task.DEFAULT_INSTRUCTION, task.DEFAULT_CONTEXT, run2, threading.Event(),
                              lessons_on=True, learn=False, record=False)
        self.assertEqual(d2["lessons_used"], 1)
        self.assertIn("# Lessons from earlier runs", prompts[-1])
        run3 = runner.Run("m", task.DEFAULT_INSTRUCTION, root=self.root)
        n = len(prompts)
        runner.run_cycle(make(), m, task.DEFAULT_INSTRUCTION, task.DEFAULT_CONTEXT, run3, threading.Event(),
                         lessons_on=False, record=False)
        self.assertNotIn("# Lessons from earlier runs", prompts[n])


if __name__ == "__main__":
    unittest.main()
