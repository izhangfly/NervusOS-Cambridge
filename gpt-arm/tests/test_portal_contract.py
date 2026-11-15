#!/usr/bin/env python3
"""Contract test for the GPT Arm service API (gpt-arm/API.md), as the portal uses it.

By default it starts gpt-arm/tools/mock_server.py on a free port and checks every endpoint against it.
Point it at another service (the real backend) with an environment variable:

    python3 -m unittest gpt-arm/tests/test_portal_contract.py -v            # against the mock
    GPTARM_URL=http://127.0.0.1:8800 python3 -m unittest gpt-arm/tests/test_portal_contract.py -v

Against anything but the mock, only read-only checks run: starting a run, a benchmark, home, calibrate, stop
and e-stop act on the real arm (the owner's rule: no arm motion in tests). Set GPTARM_CONTRACT_MUTATE=1 to
run those too, and only with a fake or simulated driver behind the service. Context edits are restored.
Works with pytest as well; needs only the standard library.
"""
import json
import os
import re
import socket
import subprocess
import sys
import time
import unittest
import urllib.error
import urllib.request
from pathlib import Path

HERE = Path(__file__).resolve().parent
MOCK = HERE.parent / "tools" / "mock_server.py"
WEB = HERE.parent / "gptarm" / "web"

EXTERNAL = os.environ.get("GPTARM_URL", "").rstrip("/")
MUTATE = not EXTERNAL or os.environ.get("GPTARM_CONTRACT_MUTATE") == "1"
URL = EXTERNAL

STEP_KINDS = {"user", "perception", "decision", "info", "error", "verdict"}
RUN_STATES = {"running", "done", "failed", "stopped"}
BENCH_STATES = {"idle", "running", "paused", "done", "stopped"}
BUSY = {None, "run", "bench", "calibrate", "home"}


# ---------------------------------------------------------------------------------------------------- HTTP

def call(method, path, body=None, timeout=10):
    """(status, content_type, bytes). Never raises on HTTP errors."""
    data = None if body is None else json.dumps(body).encode()
    req = urllib.request.Request(URL + path, data=data, method=method,
                                 headers={"Content-Type": "application/json"} if data is not None else {})
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            return r.status, r.headers.get("Content-Type", ""), r.read()
    except urllib.error.HTTPError as e:
        with e:
            return e.code, e.headers.get("Content-Type", ""), e.read()


def get_json(path):
    code, ctype, raw = call("GET", path)
    return code, json.loads(raw) if raw else None


def post_json(path, body=None):
    code, ctype, raw = call("POST", path, body if body is not None else {})
    return code, json.loads(raw) if raw else None


def wait_for(pred, timeout=20, every=0.2):
    end = time.time() + timeout
    while time.time() < end:
        v = pred()
        if v:
            return v
        time.sleep(every)
    return None


# ---------------------------------------------------------------------------------------------------- shapes

def num(v):
    return isinstance(v, (int, float)) and not isinstance(v, bool)


def opt(check):
    return lambda v: v is None or check(v)


def is_int(v):
    return isinstance(v, int) and not isinstance(v, bool)


def vec(n):
    return lambda v: isinstance(v, list) and len(v) == n and all(num(x) for x in v)


def one_of(*vals):
    return lambda v: v in vals


class Shapes(unittest.TestCase):
    """Assertion helpers that name the path of the field that is wrong."""

    def field(self, obj, key, check, where, required=True):
        if key not in obj:
            if required:
                self.fail(f"{where}: missing field {key!r}")
            return None
        v = obj[key]
        self.assertTrue(check(v), f"{where}.{key}: unexpected value {v!r}")
        return v

    def obj(self, v, where):
        self.assertIsInstance(v, dict, f"{where}: expected an object, got {v!r}")
        return v

    def check_state(self, s, where):
        self.obj(s, where)
        f = lambda k, c, required=True: self.field(s, k, c, where, required)  # noqa: E731
        f("tool_xyz_mm", opt(vec(3)))
        f("gripper", one_of("open", "closed", None))
        f("holding", opt(lambda v: isinstance(v, bool)))
        b = f("bottle", opt(lambda v: isinstance(v, dict)))
        if b:
            self.field(b, "where", one_of("table", "basket", "gripper", "unknown"), where + ".bottle")
            self.field(b, "upright", opt(lambda v: isinstance(v, bool)), where + ".bottle")
            self.field(b, "xy_mm", opt(vec(2)), where + ".bottle")
            self.field(b, "seen_s_ago", opt(num), where + ".bottle")
        k = f("basket", opt(lambda v: isinstance(v, dict)))
        if k:
            self.field(k, "xy_mm", opt(vec(2)), where + ".basket")
            self.field(k, "seen_s_ago", opt(num), where + ".basket")
        f("start_spot_xy_mm", opt(vec(2)))
        f("above", one_of("bottle", "basket", "start_spot", "nothing", None))
        f("height", one_of("carry", "working", "grasp", "release", "between", None))
        f("steps_used", opt(is_int))
        f("steps_left", opt(is_int))

    def check_step(self, st, where):
        self.obj(st, where)
        f = lambda k, c, required=True: self.field(st, k, c, where, required)  # noqa: E731
        f("n", is_int)
        f("t", num)
        kind = f("kind", lambda v: v in STEP_KINDS)
        f("note", opt(lambda v: isinstance(v, str)), required=False)
        f("detail", opt(lambda v: isinstance(v, str)), required=False)
        f("ok", opt(lambda v: isinstance(v, bool)), required=False)
        for k in ("llm_s", "motion_s"):
            f(k, opt(num), required=False)
        f("tokens", opt(is_int), required=False)
        f("attempt", opt(lambda v: is_int(v) and v >= 1), required=False)  # contract addition
        imgs = f("images", opt(lambda v: isinstance(v, dict)), required=False)
        for cam, name in (imgs or {}).items():
            self.assertTrue(name is None or isinstance(name, str), f"{where}.images.{cam}: {name!r}")
        if st.get("state") is not None:
            self.check_state(st["state"], where + ".state")
        if kind == "decision":
            f("skill", lambda v: isinstance(v, str) and v)
            f("target", opt(lambda v: isinstance(v, str)), required=False)
            f("ok", lambda v: isinstance(v, bool))
        if kind == "verdict":
            f("ok", opt(lambda v: isinstance(v, bool)))
        if kind == "user":
            f("note", lambda v: isinstance(v, str) and v)

    def check_run(self, r, where, with_steps):
        self.obj(r, where)
        f = lambda k, c, required=True: self.field(r, k, c, where, required)  # noqa: E731
        f("id", lambda v: isinstance(v, str) and v and "/" not in v)
        f("model", lambda v: isinstance(v, str) and v)
        f("mode", opt(lambda v: isinstance(v, str)), required=False)  # contract addition
        f("instruction", lambda v: isinstance(v, str))
        f("state", lambda v: v in RUN_STATES)
        f("started", num)
        f("ended", opt(num))
        o = f("outcome", lambda v: isinstance(v, dict))
        self.field(o, "success", opt(lambda v: isinstance(v, bool)), where + ".outcome")
        self.field(o, "reason", lambda v: isinstance(v, str), where + ".outcome")
        self.field(o, "checks", lambda v: isinstance(v, dict), where + ".outcome")
        u = f("usage", lambda v: isinstance(v, dict))
        self.field(u, "llm_calls", is_int, where + ".usage")
        self.field(u, "tokens", is_int, where + ".usage")
        self.field(u, "llm_s", num, where + ".usage")
        self.field(u, "motion_s", num, where + ".usage")
        p = f("pending", lambda v: v is None or isinstance(v, (dict, str)))
        if isinstance(p, dict):  # assumed shape, see the portal report
            self.field(p, "stage", lambda v: isinstance(v, str), where + ".pending")
            self.field(p, "since", opt(num), where + ".pending", required=False)
        if with_steps:
            steps = f("steps", lambda v: isinstance(v, list))
            for i, st in enumerate(steps):
                self.check_step(st, f"{where}.steps[{i}]")
            ns = [st["n"] for st in steps]
            self.assertEqual(len(ns), len(set(ns)), f"{where}.steps: step numbers repeat")
            self.assertEqual(ns, sorted(ns), f"{where}.steps: not in order")
        else:
            self.assertNotIn("steps", r, f"{where}: list endpoints leave steps out")

    def check_bench(self, b, where):
        self.obj(b, where)
        f = lambda k, c, required=True: self.field(b, k, c, where, required)  # noqa: E731
        f("state", lambda v: v in BENCH_STATES)
        f("instruction", lambda v: isinstance(v, str))
        f("runs_per_model", is_int)
        f("started", opt(num))
        f("mode", opt(lambda v: isinstance(v, str)), required=False)  # contract addition
        c = f("current", opt(lambda v: isinstance(v, dict)))
        if c:
            self.field(c, "model", lambda v: isinstance(v, str), where + ".current")
            self.field(c, "index", is_int, where + ".current")
            self.field(c, "run_id", opt(lambda v: isinstance(v, str)), where + ".current")
        f("message", lambda v: isinstance(v, str))
        for i, res in enumerate(f("results", lambda v: isinstance(v, list))):
            w = f"{where}.results[{i}]"
            self.obj(res, w)
            self.field(res, "model", lambda v: isinstance(v, str), w)
            self.field(res, "runs", is_int, w)
            self.field(res, "successes", is_int, w)
            self.field(res, "rate", opt(num), w)
            self.field(res, "mean_steps", opt(num), w)
            self.field(res, "mean_s", opt(num), w)
            self.field(res, "tokens", is_int, w)
            fails = self.field(res, "failures", lambda v: isinstance(v, dict), w)
            for k, n in fails.items():
                self.assertTrue(isinstance(k, str) and is_int(n), f"{w}.failures: {k!r}: {n!r}")
        ids = f("run_ids", lambda v: isinstance(v, list))
        self.assertTrue(all(isinstance(x, str) for x in ids), f"{where}.run_ids: {ids!r}")

    def check_status(self, s):
        w = "status"
        self.obj(s, w)
        f = lambda k, c, required=True: self.field(s, k, c, w, required)  # noqa: E731
        f("version", lambda v: isinstance(v, str))
        f("time", num)
        a = f("arm", lambda v: isinstance(v, dict))
        for k, c in (("reachable", lambda v: isinstance(v, bool)), ("enabled", opt(lambda v: isinstance(v, bool))),
                     ("status", opt(lambda v: isinstance(v, str))), ("tool_xyz_mm", opt(vec(3))),
                     ("lean_deg", opt(num)), ("joints_deg", opt(vec(6))), ("gripper_mm", opt(num)),
                     ("holding", opt(lambda v: isinstance(v, bool))), ("moving", opt(lambda v: isinstance(v, bool))),
                     ("guard", opt(lambda v: isinstance(v, str))), ("error", opt(lambda v: isinstance(v, str)))):
            self.field(a, k, c, "status.arm")
        cams = f("cameras", lambda v: isinstance(v, dict))
        for cam in ("overhead", "wrist"):
            c = self.field(cams, cam, lambda v: isinstance(v, dict), "status.cameras")
            self.field(c, "ok", lambda v: isinstance(v, bool), f"status.cameras.{cam}")
            self.field(c, "age_s", opt(num), f"status.cameras.{cam}")
            self.field(c, "error", opt(lambda v: isinstance(v, str)), f"status.cameras.{cam}")
            self.field(c, "size", opt(vec(2)), f"status.cameras.{cam}")
        cal = f("calibration", opt(lambda v: isinstance(v, dict)))
        if cal:
            self.field(cal, "ok", lambda v: isinstance(v, bool), "status.calibration")
            self.field(cal, "points", opt(is_int), "status.calibration")
            self.field(cal, "residual_px", opt(num), "status.calibration")
            self.field(cal, "when", opt(num), "status.calibration")
        if s.get("scene") is not None:
            self.check_state(s["scene"], "status.scene")
        else:
            f("scene", lambda v: v is None)
        models = f("models", lambda v: isinstance(v, list) and v)
        for i, m in enumerate(models):
            self.field(m, "id", lambda v: isinstance(v, str) and v, f"status.models[{i}]")
            self.field(m, "label", lambda v: isinstance(v, str) and v, f"status.models[{i}]")
        f("model", lambda v: v in [m["id"] for m in models])
        modes = f("modes", lambda v: isinstance(v, list) and v, required=False)  # contract addition
        if modes:
            for i, m in enumerate(modes):
                self.field(m, "id", lambda v: isinstance(v, str) and v, f"status.modes[{i}]")
                self.field(m, "label", lambda v: isinstance(v, str) and v, f"status.modes[{i}]")
            f("mode", lambda v: v in [m["id"] for m in modes])
        f("default_instruction", lambda v: isinstance(v, str) and v)
        f("busy", lambda v: v in BUSY)
        if s.get("run") is not None:
            self.check_run(s["run"], "status.run", with_steps=True)
        else:
            f("run", lambda v: v is None)
        if s.get("bench") is not None:
            self.check_bench(s["bench"], "status.bench")
        else:
            f("bench", lambda v: v is None)
        f("specs", lambda v: isinstance(v, dict))


# ---------------------------------------------------------------------------------------------------- set-up

_proc = None


def setUpModule():
    global _proc, URL
    if EXTERNAL:
        URL = EXTERNAL
    else:
        with socket.socket() as s:
            s.bind(("127.0.0.1", 0))
            port = s.getsockname()[1]
        _proc = subprocess.Popen([sys.executable, str(MOCK), "--port", str(port), "--speed", "40", "--seed", "3"],
                                 stdout=subprocess.DEVNULL, stderr=subprocess.PIPE)
        URL = f"http://127.0.0.1:{port}"
    ok = wait_for(lambda: _answers(), timeout=20)
    if not ok:
        err = _proc.stderr.read().decode() if _proc and _proc.poll() is not None else ""
        raise RuntimeError(f"no answer from {URL}/api/status {err}")


def tearDownModule():
    if _proc:
        _proc.terminate()
        try:
            _proc.wait(5)
        except subprocess.TimeoutExpired:
            _proc.kill()
        _proc.stderr.close()


def _answers():
    try:
        return call("GET", "/api/status", timeout=2)[0] == 200
    except OSError:
        return False


def idle_now(t):
    """Stops whatever is going on and waits until the service is idle (mock: back to the idle scenario)."""
    if not EXTERNAL:
        post_json("/_mock/scenario", {"name": "idle"})
    else:
        post_json("/api/stop")
    t.assertTrue(wait_for(lambda: get_json("/api/status")[1]["busy"] is None, 30), "the service did not go idle")


# ---------------------------------------------------------------------------------------------------- read-only

class ReadOnly(Shapes):
    def test_page_served(self):
        code, ctype, raw = call("GET", "/")
        self.assertEqual(code, 200)
        self.assertIn("text/html", ctype)
        self.assertIn(b"app.js", raw)
        for f, kind in (("app.js", "javascript"), ("app.css", "css"), ("favicon.svg", "svg")):
            code, ctype, _ = call("GET", "/" + f)
            self.assertEqual(code, 200, f)
            self.assertIn(kind, ctype, f)

    def test_page_needs_no_network(self):
        """The robot host may be offline: no CDN, no external fonts, no absolute URLs."""
        for p in WEB.iterdir():
            if p.suffix not in (".html", ".js", ".css", ".svg"):
                continue
            text = p.read_text()
            if p.suffix == ".svg":
                text = re.sub(r'xmlns(:\w+)?="[^"]*"', "", text)
            hits = re.findall(r"""(?:src|href)\s*=\s*["']\s*(?:https?:)?//|url\(\s*["']?(?:https?:)?//|@import|https?://""", text)
            self.assertFalse(hits, f"{p.name} refers to the network: {hits}")

    def test_status(self):
        code, s = get_json("/api/status")
        self.assertEqual(code, 200)
        self.check_status(s)

    def test_cameras(self):
        for path in ("/api/camera/overhead.jpg", "/api/camera/overhead.jpg?annotated=1", "/api/camera/wrist.jpg",
                     "/api/camera/overhead.jpg?annotated=1&t=123"):  # the portal adds a cache-busting t=
            code, ctype, raw = call("GET", path)
            if code == 200:
                self.assertIn("image/jpeg", ctype, path)
                self.assertEqual(raw[:2], b"\xff\xd8", f"{path}: not a JPEG")
            else:
                self.assertEqual(code, 404, path)
                self.assertIsInstance(json.loads(raw).get("error"), str, path)

    def test_runs_list(self):
        code, d = get_json("/api/runs?limit=5")
        self.assertEqual(code, 200)
        runs = self.field(d, "runs", lambda v: isinstance(v, list), "runs")
        self.assertLessEqual(len(runs), 5)
        for i, r in enumerate(runs):
            self.check_run(r, f"runs[{i}]", with_steps=False)
        starts = [r["started"] for r in runs]
        self.assertEqual(starts, sorted(starts, reverse=True), "runs are not newest first")

    def test_run_detail_and_images(self):
        code, d = get_json("/api/runs?limit=3")
        if not d["runs"]:
            self.skipTest("no runs yet")
        rid = d["runs"][0]["id"]
        code, r = get_json("/api/runs/" + rid)
        self.assertEqual(code, 200)
        self.check_run(r, "run", with_steps=True)
        files = [f for st in r["steps"] for f in (st.get("images") or {}).values() if f][:2]
        for f in files:
            code, ctype, raw = call("GET", f"/api/runs/{rid}/{f}")
            self.assertEqual(code, 200, f)
            self.assertIn("image/jpeg", ctype)
            self.assertEqual(raw[:2], b"\xff\xd8")

    def test_unknown_run_is_an_error(self):
        code, d = get_json("/api/runs/no-such-run-0000")
        self.assertIn(code, (400, 404))
        self.assertIsInstance(d.get("error"), str)

    def test_bench(self):
        code, b = get_json("/api/bench")
        self.assertEqual(code, 200)
        self.check_bench(b, "bench")

    def test_context_get(self):
        code, d = get_json("/api/context")
        self.assertEqual(code, 200)
        self.assertIsInstance(d.get("text"), str)


# ---------------------------------------------------------------------------------------------------- actions

@unittest.skipUnless(MUTATE, "acts on the arm: set GPTARM_CONTRACT_MUTATE=1 (fake driver only)")
class Actions(Shapes):
    def setUp(self):
        idle_now(self)

    def test_context_roundtrip(self):
        code, before = get_json("/api/context")
        try:
            code, d = post_json("/api/context", {"text": before["text"] + "\n<!-- contract test -->\n"})
            self.assertEqual(code, 200)
            self.assertIn("contract test", get_json("/api/context")[1]["text"])
            code, d = post_json("/api/context", {})
            self.assertEqual(code, 400)
            self.assertIsInstance(d.get("error"), str)
            code, d = post_json("/api/context", {"reset": True})
            self.assertEqual(code, 200)
            self.assertNotIn("contract test", get_json("/api/context")[1]["text"])
        finally:
            if not before.get("default", True):
                post_json("/api/context", {"text": before["text"]})

    def test_run_lifecycle(self):
        code, s = get_json("/api/status")
        model = s["model"]
        code, d = post_json("/api/run", {"model": model})
        self.assertEqual(code, 400, "a run without an instruction must be refused")
        self.assertIsInstance(d.get("error"), str)
        body = {"instruction": s["default_instruction"], "model": model}
        if s.get("modes"):
            body["mode"] = s["modes"][-1]["id"]
        code, d = post_json("/api/run", body)
        self.assertEqual(code, 200, d)
        self.assertTrue(d.get("ok"))
        rid = d.get("id")
        self.assertIsInstance(rid, str)
        s = get_json("/api/status")[1]
        self.check_status(s)
        self.assertEqual(s["busy"], "run")
        self.assertEqual(s["run"]["id"], rid)
        if "mode" in body:
            self.assertEqual(s["run"].get("mode"), body["mode"])
        self.assertEqual(post_json("/api/run", body)[0], 409, "a second run while busy must be refused")
        for p in ("/api/home", "/api/look", "/api/calibrate"):
            code, d = post_json(p)
            self.assertEqual(code, 409, f"{p} while running")
            self.assertIsInstance(d.get("error"), str)
        self.assertTrue(wait_for(lambda: len(get_json("/api/status")[1]["run"]["steps"]) >= 3, 30))
        code, d = post_json("/api/stop")
        self.assertEqual(code, 200)
        self.assertTrue(wait_for(lambda: get_json("/api/status")[1]["busy"] is None, 30))
        code, r = get_json("/api/runs/" + rid)
        self.check_run(r, "run", with_steps=True)
        self.assertEqual(r["state"], "stopped")
        self.assertIn(rid, [x["id"] for x in get_json("/api/runs?limit=10")[1]["runs"]])

    def test_run_to_verdict(self):
        s = get_json("/api/status")[1]
        code, d = post_json("/api/run", {"instruction": "Put the bottle in the basket", "model": s["model"]})
        self.assertEqual(code, 200, d)
        done = wait_for(lambda: get_json("/api/status")[1]["busy"] is None, 120, 0.5)
        self.assertTrue(done, "the run did not finish")
        r = get_json("/api/runs/" + d["id"])[1]
        self.check_run(r, "run", with_steps=True)
        self.assertIn(r["state"], ("done", "failed"))
        self.assertEqual(r["steps"][0]["kind"], "user")
        self.assertEqual(r["steps"][-1]["kind"], "verdict")
        self.assertIsInstance(r["outcome"]["success"], bool)

    def test_bench_lifecycle(self):
        s = get_json("/api/status")[1]
        ids = [m["id"] for m in s["models"]]
        code, d = post_json("/api/bench/resume")
        self.assertEqual(code, 409, "resume when not paused")
        code, d = post_json("/api/bench/start", {"models": [], "runs_per_model": 2, "instruction": "x"})
        self.assertEqual(code, 400)
        code, d = post_json("/api/bench/start", {"models": ids[:2], "runs_per_model": 2,
                                                 "instruction": s["default_instruction"]})
        self.assertEqual(code, 200, d)
        s = get_json("/api/status")[1]
        self.check_status(s)
        self.assertEqual(s["busy"], "bench")
        self.assertEqual(post_json("/api/run", {"instruction": "x", "model": ids[0]})[0], 409)
        b = get_json("/api/bench")[1]
        self.check_bench(b, "bench")
        self.assertIn(b["state"], ("running", "paused"))
        self.assertEqual(post_json("/api/stop")[0], 200)
        self.assertTrue(wait_for(lambda: get_json("/api/status")[1]["busy"] is None, 30))
        self.assertEqual(get_json("/api/bench")[1]["state"], "stopped")

    @unittest.skipIf(EXTERNAL, "needs the mock's scenario switch")
    def test_paused_bench_and_resume(self):
        self.assertEqual(post_json("/_mock/scenario", {"name": "paused"})[0], 200)
        s = get_json("/api/status")[1]
        self.check_status(s)
        self.assertEqual(s["bench"]["state"], "paused")
        self.assertTrue(s["bench"]["message"])
        self.assertTrue(s["bench"]["results"])
        self.assertEqual(post_json("/api/bench/resume")[0], 200)
        self.assertTrue(wait_for(lambda: get_json("/api/bench")[1]["state"] == "running", 10))
        post_json("/api/stop")

    @unittest.skipIf(EXTERNAL, "needs the mock's scenario switch")
    def test_camera_error_state(self):
        post_json("/_mock/scenario", {"name": "camerr"})
        s = get_json("/api/status")[1]
        self.check_status(s)
        self.assertFalse(s["cameras"]["wrist"]["ok"])
        self.assertTrue(s["cameras"]["wrist"]["error"])
        code, ctype, raw = call("GET", "/api/camera/wrist.jpg")
        self.assertEqual(code, 404)
        self.assertIsInstance(json.loads(raw)["error"], str)

    def test_estop_always_accepted(self):
        self.assertEqual(post_json("/api/estop")[0], 200)
        self.assertEqual(post_json("/api/stop")[0], 200)


if __name__ == "__main__":
    unittest.main(verbosity=2)
