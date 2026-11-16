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

    def test_an_arm_error_while_checking_the_scene_pauses_the_benchmark(self):
        from gptarm import arm as A
        t, make = setup()

        def make_failing():
            w = make()

            def refuse(*a, **k):
                raise A.ArmError("the fingertips dipped to -2 mm on the way: stopped")
            w.arm.move_tool = refuse
            w.arm.state = lambda: {**type(w.arm).state(w.arm), "tool": [200.0, 0.0, 40.0]}
            w.tool = lambda: ([200.0, 0.0, 40.0], 0.0)
            return w
        b = runner.Bench(["scripted"], 1, "Put the bottle into the basket, then back on the table. Lying is fine.", root=self.root)
        stop = threading.Event()
        th = threading.Thread(target=b.run, args=(make_failing, task.DEFAULT_CONTEXT, stop, None, lambda m: None), daemon=True)
        th.start()
        for _ in range(400):
            if b.d["state"] == "paused":
                break
            th.join(0.05)
        self.assertEqual(b.d["state"], "paused")
        self.assertIn("couldn't get clear", b.d["message"])
        self.assertTrue(th.is_alive(), "the benchmark waits for Resume instead of ending")
        stop.set()
        th.join(10)

    def test_a_free_instruction_benchmark_starts_from_a_lying_bottle(self):
        t, make = setup()
        t.bottle.update(upright=False, xy=[300.0, 150.0])
        b = runner.Bench(["scripted"], 1, "Put the bottle into the basket, then back on the table. Lying is fine.", root=self.root)
        stop = threading.Event()
        th = threading.Thread(target=b.run, args=(make, task.DEFAULT_CONTEXT, stop, None, lambda m: None), daemon=True)
        th.start()
        for _ in range(400):
            if b.d["run_ids"] or b.d["state"] == "paused":
                break
            th.join(0.05)
        stop.set()
        th.join(60)
        self.assertNotEqual(b.d["state"], "paused", b.d.get("message"))
        self.assertEqual(len(b.d["run_ids"]), 1)


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


class JudgeTest(unittest.TestCase):
    def test_a_failed_check_gives_one_more_go(self):
        from gptarm import judge as J, direct as D
        tmp = tempfile.TemporaryDirectory()
        t, make = setup()
        prompts, verdicts = [], iter(["fail", "success"])
        done = {"action": "done", "x": None, "y": None, "z": None, "lean": None, "yaw": None, "joints": None,
                "gripper_mm": None, "skill": None, "expect": None, "note": "Waved."}

        def ask(p, images, schema):
            prompts.append(p)
            if schema is J.SCHEMA:
                st = next(verdicts)
                self.assertIn("before", images)
                return ({"before": "b", "after": "a", "changes": "none", "status": st, "failure_kind": None if st == "success" else "no wave",
                         "explanation": "x", "critique": None if st == "success" else "move the gripper side to side", "confidence": 0.8},
                        {"tokens": 7, "seconds": 0})
            if schema is D.PLAN_SCHEMA:
                return {"stages": [{"goal": "wave", "done_when": "waved"}]}, {"tokens": 3, "seconds": 0}
            return done, {"tokens": 5, "seconds": 0}

        m = type("Real", (object,), {"ask": lambda self, p, i, s: ask(p, i, s)})()
        run = runner.Run("m", "Wave hello", "direct", root=Path(tmp.name))
        d = runner.run_cycle(make(), m, "Wave hello", "ctx", run, threading.Event(), mode="direct", record=False)
        self.assertEqual((d["outcome"]["success"], d["outcome"]["reason"]), (True, ""))
        self.assertIn("move the gripper side to side", prompts[-2])
        self.assertEqual(sum(1 for s in d["steps"] if s.get("note", "").startswith("Checker:")), 2)
        self.assertEqual(d["plan"][0]["goal"], "wave")
        tmp.cleanup()

    def test_the_checker_sees_where_things_were_put_on_the_way(self):
        from gptarm import judge as J
        tmp = tempfile.TemporaryDirectory()
        run = runner.Run("m", "Into the basket and out again", "direct", root=Path(tmp.name))

        def dec(n, skill, holding, z, ok=True):
            (run.dir / f"{n:03d}-overhead.jpg").write_bytes(f"img{n}".encode())
            return {"n": n, "kind": "decision", "skill": skill, "ok": ok,
                    "state": {"holding": holding, "tool_xyz_mm": [200, 0, z]}, "images": {"overhead": f"{n:03d}-overhead.jpg"}}
        run.d["steps"] = [dec(1, "move_tool", False, 200), dec(2, "gripper", True, 45),        # grasp
                          dec(3, "move_tool", True, 200), dec(4, "gripper", False, 60),        # release in the basket
                          dec(5, "move_by", False, 200), dec(6, "move_tool", False, 200),      # lifted after 5
                          dec(7, "gripper", True, 45), dec(8, "gripper", False, 45),           # grasp, release on the table
                          dec(9, "done", False, 45)]
        views = runner.release_views(run)
        # release at 4: decision 6's picture (taken after 5 lifted the arm); release at 8: only 9 is left
        self.assertEqual(views, [(4, b"img6"), (8, b"img9")])
        seen = {}

        def ask(p, images, schema):
            seen["prompt"], seen["keys"] = p, list(images)
            return {"status": "success"}, {}
        J.judge(type("L", (), {"ask": lambda self, p, i, s: ask(p, i, s)})(), "x", "log", "done",
                {"before": b"b", "after": b"a", "wrist": b"w", "during": views})
        self.assertEqual(seen["keys"], ["before", "during-4", "during-8", "after", "wrist"])
        self.assertIn("2 = overhead camera DURING the run, soon after the gripper let go of something at step 4", seen["prompt"])
        self.assertIn("4 = overhead camera AFTER", seen["prompt"])
        tmp.cleanup()


if __name__ == "__main__":
    unittest.main()
