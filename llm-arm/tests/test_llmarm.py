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
from nervus_llmarm.llm import ScriptedLLM  # noqa: E402
from nervus_llmarm.server import Service, make_handler  # noqa: E402


def wait(pred, timeout=30.0):
    end = time.time() + timeout
    while time.time() < end:
        if pred():
            return True
        time.sleep(0.05)
    return False


class KeyTest(unittest.TestCase):
    def test_key_rules(self):
        with tempfile.TemporaryDirectory() as d:
            key, st = C.load_key(Path(d), "deepseek", environ={})
            self.assertIsNone(key)
            self.assertEqual(st["state"], "no key")
            p = Path(d) / "openai.key"
            p.write_text("sk-test-0123456789\n")
            os.chmod(p, 0o644)
            key, st = C.load_key(Path(d), "openai", environ={})
            self.assertIsNone(key, "a world-readable key is refused")
            os.chmod(p, 0o600)
            key, st = C.load_key(Path(d), "openai", environ={})
            self.assertEqual(key, "sk-test-0123456789")
            self.assertNotIn(key, json.dumps(st), "the status never carries the key")
            key, st = C.load_key(Path(d), "deepseek", environ={"DEEPSEEK_API_KEY": "sk-env-abcdefgh"})
            self.assertEqual(st["source"], "env")

    def test_settings_refuse_unknown_provider(self):
        s = C.Settings(None)
        with self.assertRaises(ValueError):
            s.update({"provider": "nope"})
        self.assertEqual(s.update({"provider": "openai"})["provider"], "openai")
        with self.assertRaises(ValueError):
            s.update({"openai_base_url": "http://example.com"})


class SimTest(unittest.TestCase):
    def service(self, script):
        d = tempfile.mkdtemp()
        svc = Service(Path(d), "", "", sim=True, llm=ScriptedLLM(script))
        svc.settings.update({"confirm_moves": False, "speed_mm_s": 100.0})
        self.addCleanup(svc.arm.close)
        self.addCleanup(svc.frames.close)
        return svc

    def beat(self, svc, stop):
        def run():
            while not stop.is_set():
                svc.agent.last_app_beat = time.time()
                time.sleep(0.2)
        threading.Thread(target=run, daemon=True).start()

    def test_pick_and_place_on(self):
        svc = self.service([[("get_scene", {})], [("pick", {"object_id": "red-1"})],
                            [("place_on", {"object_id": "blue-1"})], "Done: red is on blue."])
        stop = threading.Event()
        self.addCleanup(stop.set)
        self.beat(svc, stop)
        self.assertTrue(wait(lambda: svc.frames.get("scene")[0] is not None, 5))
        job = svc.agent.start("put the red block on the blue one")
        self.assertTrue(wait(lambda: job.state not in ("running", "waiting"), 60), job.view())
        v = job.view()
        self.assertEqual(v["state"], "done", v["log"])
        errors = [e for e in v["log"] if e["kind"] == "result" and "error" in (e["data"] or {})]
        self.assertFalse(errors, errors)
        red, blue = svc.arm.blocks["red"], svc.arm.blocks["blue"]
        self.assertLess(math.dist(red, blue), 15, "red sits on blue")
        self.assertAlmostEqual(svc.arm.heights["red"], 30.0)
        self.assertEqual(v["log"][-1]["text"], "Done: red is on blue.")

    def test_rejects_moves_outside_the_box(self):
        svc = self.service([[("move_to", {"x_mm": 900, "y_mm": 0, "z_mm": 200})], "ok"])
        stop = threading.Event()
        self.addCleanup(stop.set)
        self.beat(svc, stop)
        job = svc.agent.start("go far")
        self.assertTrue(wait(lambda: job.state == "done", 10))
        res = [e for e in job.view()["log"] if e["kind"] == "result"][0]
        self.assertIn("outside", res["data"]["error"])

    def test_stop_mid_motion_and_silent_app(self):
        far = [("move_to", {"x_mm": 420, "y_mm": 200, "z_mm": 300})]
        svc = self.service([far, far])
        svc.settings.update({"speed_mm_s": 10.0})
        stop = threading.Event()
        self.beat(svc, stop)
        job = svc.agent.start("slow move")
        self.assertTrue(wait(lambda: svc.arm.state()["owner"] == "llm", 5))
        svc.agent.stop()
        self.assertTrue(wait(lambda: job.state == "stopped", 5))
        p = svc.arm.state()["pose"]
        time.sleep(0.6)
        self.assertLess(math.dist(p[:3], svc.arm.state()["pose"][:3]), 2.0, "the arm stopped")
        # The app going quiet ends a job too.
        stop.set()
        job = svc.agent.start("slow move")
        self.assertTrue(wait(lambda: job.state == "stopped", 5))
        self.assertIn("quiet", job.view()["log"][-1]["text"])

    def test_confirm_mode_waits_for_approve(self):
        svc = self.service([[("set_gripper", {"width_mm": 40})], "ok"])
        svc.settings.update({"confirm_moves": True})
        stop = threading.Event()
        self.addCleanup(stop.set)
        self.beat(svc, stop)
        job = svc.agent.start("open a bit")
        self.assertTrue(wait(lambda: job.state == "waiting", 5))
        self.assertEqual(job.view()["pending"]["tool"], "set_gripper")
        svc.agent.approve(False)
        self.assertTrue(wait(lambda: job.state == "done", 5))
        self.assertIn("rejected", [e for e in job.view()["log"] if e["kind"] == "result"][0]["text"])

    def test_http_api(self):
        from http.server import ThreadingHTTPServer
        svc = self.service(["Hello."])
        srv = ThreadingHTTPServer(("127.0.0.1", 0), make_handler(svc))
        threading.Thread(target=srv.serve_forever, daemon=True).start()
        self.addCleanup(srv.shutdown)
        base = f"http://127.0.0.1:{srv.server_address[1]}"
        st = json.loads(urllib.request.urlopen(base + "/api/status").read())
        self.assertTrue(st["sim"])
        self.assertIn("openai", st["providers"])
        self.assertNotIn("key\": \"sk", json.dumps(st))
        self.assertTrue(wait(lambda: urllib.request.urlopen(base + "/api/camera/scene.jpg").status == 200, 5))


if __name__ == "__main__":
    unittest.main()
