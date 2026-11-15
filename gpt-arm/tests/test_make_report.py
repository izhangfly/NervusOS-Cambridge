"""Tests for gpt-arm/tools/make_report.py on synthetic run folders (no hardware, no network).

    python3 -m unittest discover -s gpt-arm/tests
"""
import json
import math
import shutil
import sys
import tempfile
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "tools"))

import make_report as MR  # noqa: E402

ASTRA, SOL61, SOL56 = "gpt-6-astra", "gpt-6.1-sol", "gpt-5.6-sol"


def state(upright=True, where="table", holding=False):
    return {"gripper": "closed" if holding else "open", "holding": holding,
            "bottle": {"where": where, "upright": upright, "xy_mm": [330, 120], "seen_s_ago": 1.0}}


def make_run(root, rid, model, started, decisions, success, reason="", mode=None, run_state=None, checks=None,
             usage=True, images=False):
    """decisions: list of (skill, ok, detail[, state]). Every decision: 2 s LLM, 3 s motion, 1000 tokens."""
    d = root / rid
    d.mkdir(parents=True)
    steps = [{"n": 0, "t": started, "kind": "user", "note": "Put the bottle in the basket ..."}]
    t = started
    for i, dec in enumerate(decisions, start=1):
        skill, ok, detail = dec[:3]
        st = dec[3] if len(dec) > 3 else state()
        t += 5.0
        step = {"n": i, "t": t, "kind": "decision", "skill": skill, "target": None, "note": f"choose {skill}",
                "ok": ok, "detail": detail, "llm_s": 2.0, "tokens": 1000, "motion_s": 3.0, "state": st}
        if images:
            step["images"] = {"overhead": f"{i:03d}-overhead.jpg", "wrist": f"{i:03d}-wrist.jpg"}
        steps.append(step)
    ended = t + 5.0
    if success is not None:
        steps.append({"n": len(steps), "t": ended, "kind": "verdict", "ok": success, "note": reason or "done"})
    n = len(decisions)
    run = {"id": rid, "model": model, "instruction": "Put the bottle in the basket ...",
           "state": run_state or ("done" if success else "failed"), "started": started,
           "ended": None if run_state == "running" else ended,
           "outcome": {"success": success, "reason": reason, "checks": checks or {}}, "pending": None, "steps": steps}
    if mode:
        run["mode"] = mode
    if usage:
        run["usage"] = {"llm_calls": n, "tokens": 1000 * n, "llm_s": 2.0 * n, "motion_s": 3.0 * n}
    (d / "run.json").write_text(json.dumps(run))
    return rid


def ok_steps(n, skill="move_above"):
    return [(skill, True, "")] * n


class MakeReportTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.tmp = Path(tempfile.mkdtemp(prefix="gptarm-report-test-"))
        root = cls.root = cls.tmp / "runs"
        root.mkdir()
        ids = []
        # GPT-6 Astra, skills mode (the default: no "mode" field): 3 of 4 succeed.
        ids.append(make_run(root, "a1", ASTRA, 1000, ok_steps(8), True, usage=False, images=True))  # clean
        ids.append(make_run(root, "a2", ASTRA, 2000, ok_steps(4) + [("lift", False, "refused: preconditions for "
                                                                   "lift do not hold")] + ok_steps(4), True))
        ids.append(make_run(root, "a3", ASTRA, 3000, ok_steps(3) + [("close_gripper", False, "the fingers closed "
                                                                    "on nothing")]
                            + [("close_gripper", True, "")] + ok_steps(5), True))
        ids.append(make_run(root, "a4", ASTRA, 4000, ok_steps(6), False, reason="missed_grasp"))
        # GPT-6.1 Sol: one success after a knock-over, one drop, one out of decisions.
        knock = ok_steps(3) + [("descend", True, "", state(True)), ("close_gripper", True, "", state(False)),
                               ("stand_up", True, "", state(True))] + ok_steps(6)
        ids.append(make_run(root, "s1", SOL61, 5000, knock, True))
        ids.append(make_run(root, "s2", SOL61, 6000, ok_steps(7), False,
                            reason="The bottle slipped out of the gripper over the basket."))
        ids.append(make_run(root, "s3", SOL61, 7000, ok_steps(40), False, reason="step limit reached (40 decisions)"))
        # GPT-5.6 Sol: stopped by hand without a verdict; one bottle left lying down.
        ids.append(make_run(root, "f1", SOL56, 8000, ok_steps(5), None, run_state="stopped"))
        ids.append(make_run(root, "f2", SOL56, 9000, ok_steps(11), False,
                            checks={"entered_basket": True, "returned_mm": 12, "upright": False}))
        # GPT-6 Astra in direct mode.
        ids.append(make_run(root, "d1", ASTRA, 10000, ok_steps(14, "move_tool"), True, mode="direct"))
        ids.append(make_run(root, "d2", ASTRA, 11000, ok_steps(9, "move_tool"), False, mode="direct",
                            reason="the arm cannot reach the basket at that height"))
        # Still running: listed, not counted.
        ids.append(make_run(root, "r1", SOL56, 12000, ok_steps(2), None, run_state="running"))
        # A run outside the bench, and an older bench that must not be picked.
        make_run(root, "x1", ASTRA, 500, ok_steps(3), False, reason="missed_grasp")
        for name, rids in (("bench-20261101-090000", ["x1"]), ("bench-20261116-101500", ids + ["gone"])):
            (root / name).mkdir()
            (root / name / "bench.json").write_text(json.dumps({
                "state": "done", "instruction": "Put the bottle in the basket ...", "runs_per_model": 4,
                "started": 1000.0, "results": [{"model": m} for m in (ASTRA, SOL61, SOL56)], "run_ids": rids}))
        if MR.HAVE_PIL:
            for i in range(1, 9):
                for cam in ("overhead", "wrist"):
                    MR.Image.new("RGB", (64, 48), (40 * (i % 6), 90, 160)).save(root / "a1" / f"{i:03d}-{cam}.jpg")
        cls.out = cls.tmp / "out"
        cls.res = MR.build(root, None, cls.out, [], figures=True)
        cls.s = cls.res["summary"]
        cls.md = (cls.out / "REPORT.md").read_text()

    @classmethod
    def tearDownClass(cls):
        shutil.rmtree(cls.tmp, ignore_errors=True)

    def g(self, model, mode="skills"):
        return self.s["per_group"][f"{model}|{mode}"]

    # --- statistics ---------------------------------------------------------------------------------
    def test_wilson_against_hand_computation(self):
        # 3 of 4, z = 1.96: centre (0.75 + 0.4802) / 1.9604 = 0.6275, half-width 0.3269.
        lo, hi = MR.wilson(3, 4)
        self.assertAlmostEqual(lo, 0.3006, places=4)
        self.assertAlmostEqual(hi, 0.9544, places=4)
        self.assertAlmostEqual(MR.wilson(0, 10)[1], 0.2775, places=4)
        self.assertAlmostEqual(MR.wilson(10, 10)[0], 0.7225, places=4)
        self.assertTrue(all(math.isnan(v) for v in MR.wilson(0, 0)))

    def test_fisher_exact(self):
        # [[3, 1], [0, 4]]: hypergeometric terms 4, 24, 24, 4 over 56; two-sided p = 8/56.
        self.assertAlmostEqual(MR.fisher_exact(3, 4, 0, 4), 8 / 56, places=9)
        self.assertAlmostEqual(MR.fisher_exact(2, 4, 2, 4), 1.0, places=9)

    # --- selection ----------------------------------------------------------------------------------
    def test_newest_bench_and_counting(self):
        self.assertEqual(self.s["bench"], "bench-20261116-101500")
        self.assertEqual(len(self.s["runs"]), 11)
        self.assertNotIn("x1", [r["id"] for r in self.s["runs"]])
        self.assertEqual([x["id"] for x in self.s["skipped"]], ["r1"])
        self.assertEqual(self.s["missing"], ["gone"])
        self.assertEqual(self.s["groups"], [f"{ASTRA}|skills", f"{ASTRA}|direct", f"{SOL61}|skills",
                                            f"{SOL56}|skills"])

    # --- per group numbers --------------------------------------------------------------------------
    def test_astra_skills(self):
        a = self.g(ASTRA)
        self.assertEqual((a["runs"], a["successes"]), (4, 3))
        self.assertAlmostEqual(a["wilson95"][0], 0.3006, places=4)
        self.assertAlmostEqual(a["wilson95"][1], 0.9544, places=4)
        self.assertEqual((a["clean_successes"], a["recovered_successes"]), (1, 2))
        self.assertEqual(a["decisions"], {"n": 4, "median": 8.5, "min": 6, "max": 10})
        self.assertEqual((a["decisions_total"], a["refused_total"], a["failed_decisions_total"]), (33, 1, 1))
        self.assertAlmostEqual(a["refusal_rate"], 1 / 33)
        self.assertEqual(a["retries_total"], 1)
        self.assertEqual(a["failures"], {"missed_grasp": 1})
        # wall time = 5 s per decision + 5 s; a1 has no usage block, so its LLM time is the steps' sum (8 x 2 s).
        self.assertEqual(a["wall_s"], {"n": 4, "median": 47.5, "min": 35.0, "max": 55.0})
        a1 = next(r for r in self.s["runs"] if r["id"] == "a1")
        self.assertEqual((a1["llm_s"], a1["motion_s"], a1["tokens"]), (16.0, 24.0, 8000.0))
        self.assertEqual(a["tokens"]["median"], 8500.0)

    def test_failure_taxonomy(self):
        self.assertEqual(self.g(SOL61)["failures"], {"dropped": 1, "step_limit": 1})
        self.assertEqual(self.g(SOL56)["failures"], {"not_upright": 1, "operator_stop": 1})
        self.assertEqual(self.g(ASTRA, "direct")["failures"], {"motion_error": 1})

    def test_recoveries(self):
        s1 = next(r for r in self.s["runs"] if r["id"] == "s1")
        self.assertEqual((s1["knock_overs"], s1["clean"], s1["recovered"]), (1, False, True))
        a3 = next(r for r in self.s["runs"] if r["id"] == "a3")
        self.assertEqual((a3["failed_decisions"], a3["refused"], a3["retries"]), (1, 0, 1))
        self.assertEqual(self.g(SOL61)["knock_overs_total"], 1)

    def test_stages(self):
        self.assertEqual(self.g(ASTRA)["stages"], [1, 0, 0, 0, 3])
        f2 = next(r for r in self.s["runs"] if r["id"] == "f2")
        self.assertEqual(f2["stage"], 2)

    # --- the Markdown -------------------------------------------------------------------------------
    def test_report_text(self):
        md = self.md
        self.assertIn("| GPT-6 Astra (skills) | 4 | 3 | 75% | 30% to 95% | 1 | 2 |", md)
        self.assertIn("| GPT-6 Astra (direct) | 2 | 1 | 50% |", md)
        self.assertIn("## 5. Skills versus direct control", md)
        self.assertIn("| GPT-6 Astra | direct | 1/2 | 50% |", md)
        self.assertIn("| GPT-6 Astra (skills) | 8.5 [6, 10] |", md)
        self.assertIn("| GPT-6 Astra (skills) | 33 | 1 | 3% | 1/4 | 1 | 1 | 0 |", md)
        self.assertIn("| step_limit | Ran out of decisions |", md)
        self.assertIn("FAILED close_gripper", md)  # a3, the median recovered success
        refused = {"skill": "lift", "ok": False, "detail": "refused: preconditions for lift do not hold"}
        self.assertEqual(MR.step_word(refused), "REFUSED lift")
        self.assertIn("### Success after a recovery", md)
        self.assertIn("`r1` (GPT-5.6 Sol): still running", md)

    def test_figures(self):
        figs = self.out / "figures"
        if MR.HAVE_MPL:
            for name in ("success_rate.png", "effort.png", "timelines.png"):
                self.assertTrue((figs / name).stat().st_size > 1000, name)
        else:
            self.assertIn("matplotlib is not importable", self.md)
        if MR.HAVE_PIL:
            self.assertTrue((figs / "contact-gpt-6-astra-skills.jpg").exists())
        else:
            self.assertIn("Pillow is not importable", self.md)

    def test_tables_only_and_cli(self):
        out = self.tmp / "out-plain"
        self.assertEqual(MR.main(["--runs", str(self.root), "--out", str(out), "--no-figures",
                                  "--exclude", "a4"]), 0)
        s = json.loads((out / "summary.json").read_text())
        self.assertFalse((out / "figures").exists())
        a = s["per_group"][f"{ASTRA}|skills"]
        self.assertEqual((a["runs"], a["successes"]), (3, 3))
        self.assertIn("--no-figures", (out / "REPORT.md").read_text())
        out2 = self.tmp / "out-old"
        MR.main(["--runs", str(self.root), "--out", str(out2), "--bench", "20261101-090000", "--no-figures"])
        self.assertEqual([r["id"] for r in json.loads((out2 / "summary.json").read_text())["runs"]], ["x1"])


if __name__ == "__main__":
    unittest.main()
