"""LLM Arm tests against the simulated arm and cameras, with a scripted model (no network, no hardware).

    PYTHONPATH=llm-arm/engine python -m unittest discover -s llm-arm/tests
"""
import json
import math
import os
import sys
import tempfile
import threading
import time
import unittest
import urllib.request
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "engine"))

from nervus_llmarm import config as C  # noqa: E402
from nervus_llmarm.llm import Scripted  # noqa: E402
from nervus_llmarm.server import Service, demo_script, make_handler  # noqa: E402
from nervus_llmarm.vision import SimCameras  # noqa: E402


def wait(pred, timeout=30.0):
    end = time.time() + timeout
    while time.time() < end:
        if pred():
            return True
        time.sleep(0.05)
    return False


def ground(target_xy, place_xy, kind="on_object"):
    (tu, tv), (pu, pv) = SimCameras.px(*target_xy), SimCameras.px(*place_xy)
    return {"found": True, "target_label": "block", "target_u": tu, "target_v": tv, "place_label": "place",
            "place_u": pu, "place_v": pv, "place_kind": kind, "note": "seen"}


def skills(*names):
    return [{"skill": n, "note": n} for n in names]


class KeyTest(unittest.TestCase):
    def test_key_rules(self):
        with tempfile.TemporaryDirectory() as d:
            key, st = C.load_key(Path(d), "deepseek", environ={})
            self.assertIsNone(key)
            self.assertEqual(st["state"], "no key")
            p = Path(d) / "openai.key"
            p.write_text("sk-test-0123456789\n")
            os.chmod(p, 0o644)
            self.assertIsNone(C.load_key(Path(d), "openai", environ={})[0], "a world-readable key is refused")
            os.chmod(p, 0o600)
            key, st = C.load_key(Path(d), "openai", environ={})
            self.assertEqual(key, "sk-test-0123456789")
            self.assertNotIn(key, json.dumps(st), "the status never carries the key")

    def test_settings(self):
        s = C.Settings(None)
        with self.assertRaises(ValueError):
            s.update({"provider": "nope"})
        self.assertEqual(s.update({"provider": "openai"})["provider"], "openai")
        with self.assertRaises(ValueError):
            s.update({"openai_effort": "max"})
        with self.assertRaises(ValueError):
            s.update({"openai_base_url": "http://example.com"})


class SimTest(unittest.TestCase):
    def service(self, answers, confirm=False):
        d = tempfile.mkdtemp()
        svc = Service(Path(d), "", "", sim=True, llm=Scripted(answers))
        svc.settings.update({"confirm_moves": confirm, "speed_mm_s": 100.0})
        self.addCleanup(svc.arm.close)
        self.addCleanup(svc.frames.close)
        stop = threading.Event()
        self.addCleanup(stop.set)

        def beat():
            while not stop.is_set():
                svc.agent.last_app_beat = time.time()
                time.sleep(0.2)
        threading.Thread(target=beat, daemon=True).start()
        self.beat_stop = stop
        self.assertTrue(wait(lambda: svc.frames.get("scene")[0] is not None, 5))
        return svc

    def run_job(self, svc, text, timeout=90):
        job = svc.agent.start(text)
        self.assertTrue(wait(lambda: job.state not in ("running", "waiting"), timeout), job.view())
        return job.view()

    def test_red_onto_blue(self):
        svc = self.service(demo_script())
        v = self.run_job(svc, "put the red block on the blue one")
        self.assertEqual(v["state"], "done", v["log"])
        self.assertFalse([e for e in v["log"] if e["kind"] in ("error", "refused")], v["log"])
        self.assertLess(math.dist(svc.arm.blocks["red"], svc.arm.blocks["blue"]), 15, "red sits on blue")
        self.assertAlmostEqual(svc.arm.heights["red"], 30.0)
        self.assertTrue(svc.agent.task.released)

    def test_governor_refuses_a_skill_out_of_order(self):
        # Closing before descending is refused; the run goes on with the next decision.
        svc = self.service([ground((380, 0), (300, -120))] + skills("close_gripper", "done"))
        v = self.run_job(svc, "green on blue")
        refused = [e for e in v["log"] if e["kind"] == "refused"]
        self.assertEqual(len(refused), 1, v["log"])
        self.assertIn("close_gripper", refused[0]["text"])
        self.assertGreater(svc.arm.state()["gripper_mm"], 5, "the gripper never closed")

    def test_grounding_outside_the_workspace_is_refused(self):
        svc = self.service([ground((380, 0), (300, -120)) | {"place_u": 630.0, "place_v": 470.0}])
        v = self.run_job(svc, "somewhere far")
        self.assertEqual(v["state"], "failed")
        self.assertIn("outside the arm's workspace", v["log"][-1]["text"])

    def test_stop_and_silent_app(self):
        svc = self.service([ground((380, 0), (300, -120))] + skills("approach_target")
                           + [ground((380, 0), (300, -120))] + skills("approach_target"))
        svc.settings.update({"speed_mm_s": 10.0})
        job = svc.agent.start("slow")
        self.assertTrue(wait(lambda: svc.arm.state()["owner"] == "llm", 10))
        svc.agent.stop()
        self.assertTrue(wait(lambda: job.state == "stopped", 5))
        p = svc.arm.state()["pose"]
        time.sleep(0.6)
        self.assertLess(math.dist(p[:3], svc.arm.state()["pose"][:3]), 2.0, "the arm stopped")
        self.beat_stop.set()
        job = svc.agent.start("slow")
        self.assertTrue(wait(lambda: job.state == "stopped", 8))
        self.assertIn("quiet", job.view()["log"][-1]["text"])

    def test_confirm_mode_waits_for_approve(self):
        svc = self.service([ground((380, 0), (300, -120))] + skills("approach_target", "done"), confirm=True)
        job = svc.agent.start("green")
        self.assertTrue(wait(lambda: job.state == "waiting", 10))
        self.assertEqual(job.view()["pending"]["skill"], "approach_target")
        svc.agent.approve(False)
        self.assertTrue(wait(lambda: job.state == "done", 10))
        self.assertTrue([e for e in job.view()["log"] if e["kind"] == "refused" and "rejected" in e["text"]])

    def test_context_is_editable_and_reaches_the_model(self):
        llm = Scripted([ground((380, 0), (300, -120))] + skills("done"))
        d = tempfile.mkdtemp()
        svc = Service(Path(d), "", "", sim=True, llm=llm)
        self.addCleanup(svc.arm.close)
        self.addCleanup(svc.frames.close)
        with self.assertRaises(ValueError):
            svc.agent.set_context("no placeholder")
        svc.agent.set_context("Our arm is called Bob. Task: {task}")
        svc.agent.last_app_beat = time.time() + 30
        self.assertTrue(wait(lambda: svc.frames.get("scene")[0] is not None, 5))
        job = svc.agent.start("wave")
        self.assertTrue(wait(lambda: job.state != "running", 10))
        self.assertIn("Our arm is called Bob. Task: wave", llm.prompts[0][1])
        svc.agent.set_context(None)
        self.assertIn("AgileX PiPER", svc.agent.context())

    def test_http_api(self):
        from http.server import ThreadingHTTPServer
        d = tempfile.mkdtemp()
        svc = Service(Path(d), "", "", sim=True, llm=Scripted([]))
        self.addCleanup(svc.arm.close)
        self.addCleanup(svc.frames.close)
        srv = ThreadingHTTPServer(("127.0.0.1", 0), make_handler(svc))
        threading.Thread(target=srv.serve_forever, daemon=True).start()
        self.addCleanup(srv.shutdown)
        base = f"http://127.0.0.1:{srv.server_address[1]}"
        get = lambda p: json.loads(urllib.request.urlopen(base + p).read())  # noqa: E731
        st = get("/api/status")
        self.assertTrue(st["sim"])
        self.assertIn("openai", st["providers"])
        self.assertIn("{task}", get("/api/context")["text"])
        self.assertIn("over_target", get("/api/state"))
        # No frames are grabbed until the app says it is open.
        self.assertIsNone(svc.frames.get("scene")[0])
        urllib.request.urlopen(urllib.request.Request(base + "/api/heartbeat", b"{}", method="POST")).read()
        self.assertTrue(wait(lambda: svc.frames.get("scene")[0] is not None, 5))
        self.assertEqual(urllib.request.urlopen(base + "/api/camera/scene.jpg").status, 200)


if __name__ == "__main__":
    unittest.main()
