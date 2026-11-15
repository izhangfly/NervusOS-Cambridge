"""The GPT Arm service: the portal's backend on 127.0.0.1:8800 (API.md).

It owns the cameras (the wrist camera can only be opened once) and the arm client, runs one run, benchmark,
calibration or homing at a time in a worker thread, and serves the portal page from gptarm/web/.

    python -m gptarm.server [--port 8800] [--sim]

--sim uses the pretend arm, table and cameras (sim.py) and the scripted policy as an extra model, for trying
the portal and the models' prompts without the arm. Loopback only: it moves the arm and has no login.
"""
from __future__ import annotations

import argparse
import json
import mimetypes
import re
import threading
import time
import traceback
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from urllib.parse import parse_qs, unquote, urlparse

import cv2

from . import __version__
from . import arm as A
from . import calib as C
from . import direct as D
from . import llm as L
from . import perception as P
from . import runner as R
from . import sim
from . import task as T
from .cameras import CameraError, Overhead, Wrist, to_jpeg

WEB = Path(__file__).resolve().parent / "web"
MODES = [{"id": "direct", "label": "Direct (model moves the arm)"}, {"id": "skills", "label": "Skills (AgileX style)"}]


class Busy(Exception):
    pass


class Service:
    def __init__(self, use_sim=False):
        self.sim = use_sim
        self.lock = threading.RLock()
        self.busy = None                  # None | run | bench | calibrate | home | look
        self.stop_event = threading.Event()
        self.run = None                   # the current or most recent run (dict, with steps)
        self.bench = None                 # runner.Bench
        self.world = None                 # the current run's world (for the live scene)
        self.cfg = T.Config.load()
        self.error = None
        self._ann = (0.0, None)           # (time, jpeg) of the last annotated overhead picture
        if use_sim:
            self.table = sim.Table()
            self.cam = sim.camera()
            self.arm = sim.SimArm(self.table, move_s=0.6, stop_event=self.stop_event)
            self.overhead = sim.SimOverhead(self.table, self.cam)
            self.wrist = sim.SimWrist()
        else:
            self.cam = C.Camera.load()
            self.arm = A.Arm(speed=self.cfg.speed, stop_event=self.stop_event)
            self.overhead = Overhead()
            self.wrist = Wrist()

    # ---- the world a run starts from: new each time, nothing carried over ----------------------------------
    def make_world(self):
        cfg = T.Config.load()
        self.arm.speed = cfg.speed
        self.arm.floor_z_mm = 20.0
        kw = {"perceive": sim.perceive(self.table)} if self.sim else {}
        w = T.World(self.arm, self.overhead, self.wrist, self.cam, cfg, log=lambda m: None, **kw)
        self.world = w
        return w

    def models(self):
        out = list(L.MODELS)
        if self.sim:
            out.append({"id": "scripted", "label": "Scripted (no model)"})
        return out

    # ---- the worker ------------------------------------------------------------------------------------------
    def _start(self, kind, fn):
        with self.lock:
            if self.busy:
                raise Busy(f"the service is busy ({self.busy})")
            self.busy = kind
            self.stop_event.clear()

        def body():
            try:
                fn()
            except Exception as e:                           # noqa: BLE001 (report it, keep serving)
                self.error = f"{kind}: {type(e).__name__}: {e}"
                traceback.print_exc()
            finally:
                with self.lock:
                    self.busy = None
                try:
                    self.arm.hold()
                except Exception:                            # noqa: BLE001
                    pass
        threading.Thread(target=body, daemon=True, name=kind).start()

    def _ready(self):
        if self.cam is None:
            raise Busy("the overhead camera is not calibrated: run Calibrate first")
        try:
            st = self.arm.state()
        except A.ArmError as e:
            raise Busy(str(e)) from None
        if not st["fresh"]:
            raise Busy("the arm is not sending feedback (is it powered and on CAN?)")

    def start_run(self, instruction, model, mode):
        if mode not in ("skills", "direct"):
            raise ValueError("mode must be skills or direct")
        if model not in [m["id"] for m in self.models()]:
            raise ValueError(f"unknown model {model!r}")
        instruction = (instruction or "").strip()
        if not instruction:
            raise ValueError("type an instruction first")
        self._ready()
        llm = R.make_llm(model)
        run = R.Run(model, instruction, mode, on_update=self._on_run)
        self.run = run.d
        context = (D.load_context() if mode == "direct" else T.load_context())[0]
        self._start("run", lambda: R.run_cycle(self.make_world(), llm, instruction, context, run, self.stop_event, mode=mode))
        return run.d["id"]

    def _on_run(self, d):
        self.run = d

    def start_bench(self, models, n, instruction, mode):
        if not models or any(m not in [x["id"] for x in self.models()] for m in models):
            raise ValueError("models must be a list of known model ids")
        if not 1 <= int(n) <= 100:
            raise ValueError("runs_per_model must be 1 to 100")
        if mode not in ("skills", "direct"):
            raise ValueError("mode must be skills or direct")
        self._ready()
        b = R.Bench(models, int(n), (instruction or "").strip() or T.DEFAULT_INSTRUCTION, mode)
        self.bench = b
        context = (D.load_context() if mode == "direct" else T.load_context())[0]
        self._start("bench", lambda: b.run(self.make_world, context, self.stop_event, self._on_run, lambda m: None))
        return b.d["id"]

    def resume_bench(self):
        if not self.bench or self.bench.d["state"] != "paused":
            raise Busy("no benchmark is waiting")
        self.bench.resume_event.set()

    def stop(self):
        self.stop_event.set()
        if self.bench:
            self.bench.resume_event.set()
        try:
            self.arm.hold()
        except A.ArmError:
            pass

    def home(self):
        def go():
            w = self.make_world()
            tool, _ = w.tool()
            if tool[2] < w.cfg.travel_z - 15.0:
                w._go(tool[0], tool[1], w.cfg.travel_z, min_tip_z=tool[2] - 30.0)
            x, y, z = w.cfg.park[0]
            w._go(x, y, z, lean=40.0, min_tip_z=w.cfg.travel_z - 60.0)
        self._ready()
        self._start("home", go)

    def look(self):
        def go():
            self.make_world().observe()
        self._start("look", go)

    def calibrate(self):
        def go():
            cam = C.run(A.Arm(speed=self.cfg.speed, floor_z_mm=60.0, stop_event=self.stop_event), self.overhead,
                        log=print, out_dir="/tmp/gptarm-calib")
            self.cam = cam
        if self.sim:
            raise Busy("there is nothing to calibrate in the simulator")
        self._start("calibrate", go)

    # ---- what the portal reads -------------------------------------------------------------------------------
    def status(self):
        arm = {"reachable": False, "enabled": False, "status": None, "tool_xyz_mm": None, "lean_deg": None,
               "joints_deg": None, "gripper_mm": None, "holding": False, "moving": False, "guard": None, "error": None}
        try:
            st = self.arm.state()
            arm.update(reachable=True, enabled=st["enabled"], status=st["status"], tool_xyz_mm=st["tool"],
                       lean_deg=st["lean"], joints_deg=st["joints"], gripper_mm=st["gripper_mm"],
                       holding=bool(self.world.holding) if self.world else False, moving=st["moving"], guard=st["guard"])
            if not st["fresh"]:
                arm["error"] = "no feedback from the arm"
        except A.ArmError as e:
            arm["error"] = str(e)
        cams = {}
        try:
            i = self.overhead.info()
            cams["overhead"] = {"ok": (i.get("age_s") or 99) < 5, "age_s": i.get("age_s"), "error": None,
                                "size": [i.get("width"), i.get("height")]}
        except CameraError as e:
            cams["overhead"] = {"ok": False, "age_s": None, "error": str(e), "size": None}
        wt = getattr(self.wrist, "t", time.time())
        werr = getattr(self.wrist, "error", None)
        cams["wrist"] = {"ok": werr is None and time.time() - wt < 3, "age_s": round(time.time() - wt, 2),
                         "error": werr, "size": [1280, 720]}
        cal = {"ok": self.cam is not None, "points": len(self.cam.points) if self.cam else 0,
               "residual_px": self.cam.residual_px if self.cam else None, "when": self.cam.when if self.cam else None}
        scene = None
        if self.world is not None:
            try:
                scene = self.world.state()
            except Exception:                                # noqa: BLE001
                scene = None
        bench = self.bench.d if self.bench else None
        return {
            "version": __version__, "time": round(time.time(), 2), "arm": arm, "cameras": cams, "calibration": cal,
            "scene": scene, "models": self.models(), "model": "scripted" if self.sim else "gpt-6.1-sol", "modes": MODES, "mode": "direct",
            "default_instruction": T.DEFAULT_INSTRUCTION, "busy": self.busy, "error": self.error,
            "run": self.run, "bench": bench,
            "specs": {"arm": "AgileX PiPER (6 joints, gripper 0-75 mm)", "driver": "smooth (own IK, Ruckig, 100 Hz)",
                      "speed_pct": self.cfg.speed, "joint_speed_deg_s": self.cfg.joint_speed_deg_s,
                      "workspace_mm": {"x": [60, 480], "y": [-320, 320], "z": [80, 600]},
                      "floor_mm": {"skills": 20, "direct": self.cfg.floor_direct},
                      "overhead": "Surface Pro 9 rear camera, 1600x1200" if not self.sim else "simulated",
                      "wrist": "Orbbec Dabai DC1 colour, 1280x720" if not self.sim else "simulated",
                      "llm_route": L.ROUTE, "max_steps": {"skills": self.cfg.max_steps, "direct": self.cfg.max_steps_direct},
                      "backend": "simulator" if self.sim else "real arm"},
        }

    def overhead_jpeg(self, annotated):
        if not annotated:
            return self.overhead.jpeg()
        t, data = self._ann
        if data and time.time() - t < 1.5:
            return data
        img = self.overhead.frame()
        w = self.world
        joints = None
        try:
            joints = self.arm.state()["joints"]
        except A.ArmError:
            pass
        img = D.grid(img, self.cam) if self.cam is not None else img
        if self.cam is not None:
            if w is not None and self.busy in ("run", "bench"):
                sc = w._scene_for_drawing()
                start = w.start_xy
            else:
                sc = (sim.perceive(self.table)(img, joints) if self.sim else P.look(img, self.cam, joints))
                start = None
            tool = None
            try:
                tool = self.arm.state()["tool"]
            except A.ArmError:
                pass
            img = P.draw(img, self.cam, sc, tool, start)
        data = to_jpeg(img, 80, 1280)
        self._ann = (time.time(), data)
        return data

    def wrist_jpeg(self):
        return to_jpeg(self.wrist.frame(), 80, 960)

    def runs(self, limit):
        out = []
        if R.RUNS.exists():
            for d in sorted((p for p in R.RUNS.iterdir() if p.is_dir() and not p.name.startswith("bench-")), reverse=True):
                f = d / "run.json"
                if not f.exists():
                    continue
                try:
                    r = json.loads(f.read_text())
                except ValueError:
                    continue
                r["n_steps"] = len(r.get("steps") or [])
                r.pop("steps", None)
                out.append(r)
                if len(out) >= limit:
                    break
        return out


class Handler(BaseHTTPRequestHandler):
    svc: Service = None

    def log_message(self, *a):
        pass

    def send(self, code, body, ctype="application/json"):
        data = body if isinstance(body, (bytes, bytearray)) else json.dumps(body).encode()
        self.send_response(code)
        self.send_header("Content-Type", ctype)
        self.send_header("Content-Length", str(len(data)))
        self.send_header("Cache-Control", "no-store")
        self.end_headers()
        self.wfile.write(data)

    def body(self):
        n = int(self.headers.get("Content-Length") or 0)
        if not n:
            return {}
        d = json.loads(self.rfile.read(n))
        if not isinstance(d, dict):
            raise ValueError("the body must be a JSON object")
        return d

    def do_GET(self):
        u = urlparse(self.path)
        path, q = unquote(u.path), parse_qs(u.query)
        s = self.svc
        try:
            if path == "/api/status":
                return self.send(200, s.status())
            m = re.fullmatch(r"/api/camera/(overhead|wrist)\.jpg", path)
            if m:
                try:
                    data = s.overhead_jpeg(q.get("annotated", ["0"])[0] == "1") if m.group(1) == "overhead" else s.wrist_jpeg()
                except CameraError as e:
                    return self.send(404, {"error": str(e)})
                return self.send(200, data, "image/jpeg")
            if path == "/api/runs":
                return self.send(200, {"runs": s.runs(int(q.get("limit", ["50"])[0]))})
            m = re.fullmatch(r"/api/runs/([A-Za-z0-9._-]+)", path)
            if m:
                f = R.RUNS / m.group(1) / "run.json"
                if not f.exists():
                    return self.send(404, {"error": "no such run"})
                return self.send(200, f.read_bytes())
            m = re.fullmatch(r"/api/runs/([A-Za-z0-9._-]+)/([A-Za-z0-9._-]+\.jpg)", path)
            if m:
                f = R.RUNS / m.group(1) / m.group(2)
                if not f.exists():
                    return self.send(404, {"error": "no such image"})
                return self.send(200, f.read_bytes(), "image/jpeg")
            if path == "/api/bench":
                b = s.bench.d if s.bench else {"state": "idle", "instruction": "", "runs_per_model": 0, "started": None,
                                               "current": None, "message": "", "results": [], "run_ids": []}
                return self.send(200, b)
            if path == "/api/context":
                mode = q.get("mode", ["direct"])[0]
                text, default = (D.load_context() if mode == "direct" else T.load_context())
                return self.send(200, {"text": text, "default": default, "mode": mode})
            if path.startswith("/api/"):
                return self.send(404, {"error": "no such endpoint"})
            name = "index.html" if path in ("/", "") else path.lstrip("/")
            f = (WEB / name).resolve()
            if WEB.resolve() not in f.parents or not f.is_file():
                return self.send(404, {"error": "not found"})
            return self.send(200, f.read_bytes(), mimetypes.guess_type(f.name)[0] or "application/octet-stream")
        except Exception as e:                               # noqa: BLE001
            traceback.print_exc()
            return self.send(500, {"error": f"{type(e).__name__}: {e}"})

    def do_POST(self):
        path = urlparse(self.path).path
        s = self.svc
        try:
            b = self.body()
            if path == "/api/stop":
                s.stop()
                return self.send(200, {"ok": True})
            if path == "/api/estop":
                s.stop()
                s.arm.estop()
                return self.send(200, {"ok": True})
            if path == "/api/run":
                rid = s.start_run(b.get("instruction"), b.get("model") or "gpt-6.1-sol", b.get("mode") or "direct")
                return self.send(200, {"ok": True, "id": rid})
            if path == "/api/bench/start":
                bid = s.start_bench(b.get("models") or [], b.get("runs_per_model") or 1, b.get("instruction"),
                                    b.get("mode") or "direct")
                return self.send(200, {"ok": True, "id": bid})
            if path == "/api/bench/resume":
                s.resume_bench()
                return self.send(200, {"ok": True})
            if path == "/api/home":
                s.home()
                return self.send(200, {"ok": True})
            if path == "/api/look":
                s.look()
                return self.send(200, {"ok": True})
            if path == "/api/calibrate":
                s.calibrate()
                return self.send(200, {"ok": True})
            if path == "/api/context":
                mode = b.get("mode") or "direct"
                save, load = (D.save_context, D.load_context) if mode == "direct" else (T.save_context, T.load_context)
                if b.get("reset"):
                    save(None)
                elif isinstance(b.get("text"), str) and b["text"].strip():
                    save(b["text"])
                else:
                    return self.send(400, {"error": "send {\"text\": ...} or {\"reset\": true}"})
                text, default = load()
                return self.send(200, {"ok": True, "text": text, "default": default, "mode": mode})
            return self.send(404, {"error": "no such endpoint"})
        except Busy as e:
            return self.send(409, {"error": str(e)})
        except (ValueError, KeyError, L.LLMError) as e:
            return self.send(400, {"error": str(e)})
        except A.ArmError as e:
            return self.send(409, {"error": str(e)})
        except Exception as e:                               # noqa: BLE001
            traceback.print_exc()
            return self.send(500, {"error": f"{type(e).__name__}: {e}"})


def main():
    ap = argparse.ArgumentParser(description="GPT Arm service (the portal's backend)")
    ap.add_argument("--port", type=int, default=8800)
    ap.add_argument("--sim", action="store_true", help="pretend arm, table and cameras")
    a = ap.parse_args()
    Handler.svc = Service(use_sim=a.sim)
    srv = ThreadingHTTPServer(("127.0.0.1", a.port), Handler)
    srv.daemon_threads = True
    print(f"GPT Arm service on http://127.0.0.1:{a.port} ({'simulator' if a.sim else 'real arm'})", flush=True)
    srv.serve_forever()


if __name__ == "__main__":
    main()
