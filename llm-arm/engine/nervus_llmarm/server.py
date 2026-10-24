"""HTTP API on loopback (default 127.0.0.1:8796) for the LLM Arm app.

    GET  /api/status                 arm, cameras, provider and key state, calibration, settings, the current job
    GET  /api/camera/<role>.jpg      latest frame (scene | wrist), with detections drawn on the scene frame
    GET  /api/scene                  what get_scene would tell the model
    GET  /api/job                    the current job's log and any step waiting for approval
    POST /api/chat {text}            start a request
    POST /api/approve {ok}           answer the waiting step
    POST /api/stop                   end the job and hold the arm (motors stay on)
    POST /api/estop                  the driver's damped emergency stop
    POST /api/heartbeat              the app is alive (the arm stops if this lapses for 2 s during a job)
    POST /api/settings {...}         change settings (provider, models, speed, confirm_moves, ...)
    POST /api/keys/reload            read the key files again
    POST /api/calibrate {points, table_z_mm}

Like the ACT engine and the PiPER driver it has no authentication, so it refuses to listen anywhere but loopback.
"""
from __future__ import annotations

import argparse
import ipaddress
import json
import signal
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from urllib.parse import urlparse

import cv2

from . import __version__, config as C
from .agent import Agent
from .driver import DriverClient, DriverError, SimArm
from .llm import ChatClient, ScriptedLLM
from .vision import ROLES, Calibration, Frames, SimCameras, act_engine_source, decode, detect


class Service:
    def __init__(self, config_dir: Path, driver: str, act: str, sim: bool, llm=None):
        self.sim = sim
        self.config_dir = Path(config_dir)
        sfx = "-sim" if sim else ""
        self.settings = C.Settings(self.config_dir / f"llm-arm{sfx}.json")
        if sim:
            self.arm = SimArm()
            cams = SimCameras(self.arm)
            self.calib = Calibration(None, preset=cams.calibration())
            source = cams
        else:
            self.arm = DriverClient(driver)
            self.calib = Calibration(self.config_dir / "llm-arm-calib.json")
            source = act_engine_source(act)
        self.frames = Frames(source, lambda: self.settings["frame_interval_s"])
        self.keys = {}
        self.reload_keys()
        self._llm_override = llm
        self.agent = Agent(self.arm, self.frames, self.calib, self.settings, self.make_llm, sim=sim)

    def reload_keys(self):
        self.keys = {p: C.load_key(self.config_dir, p) for p in C.PROVIDERS}

    def make_llm(self):
        if self._llm_override is not None:
            return self._llm_override
        p = self.settings["provider"]
        key, st = self.keys[p]
        if not key:
            raise RuntimeError(st["message"])
        return ChatClient(p, key, self.settings[f"{p}_model"], self.settings[f"{p}_base_url"],
                          self.settings["llm_timeout_s"])

    def status(self) -> dict:
        try:
            arm = self.arm.state()
        except DriverError as e:
            arm = {"reachable": False, "error": str(e)}
        p = self.settings["provider"]
        return {
            "version": __version__, "sim": self.sim, "time": time.time(),
            "arm": arm, "cameras": self.frames.info(), "calibration": self.calib.info(),
            "provider": p,
            "providers": {k: {"label": v["label"], "model": self.settings[f"{k}_model"],
                              "key": self.keys[k][1]} for k, v in C.PROVIDERS.items()},
            "llm": "scripted" if self._llm_override is not None else self.keys[p][1]["state"],
            "settings": self.settings.describe(),
            "job": self.agent.job.view() if self.agent.job else None,
        }

    def frame(self, role: str):
        jpg, _, _ = self.frames.get(role)
        if jpg is None or role != "scene":
            return jpg
        img = decode(jpg)
        for o in detect(img):
            cv2.circle(img, (int(o["u"]), int(o["v"])), 12, (255, 255, 255), 2)
            cv2.putText(img, o["colour"], (int(o["u"]) + 14, int(o["v"]) + 4), cv2.FONT_HERSHEY_SIMPLEX, 0.45,
                        (255, 255, 255), 1, cv2.LINE_AA)
        ok, out = cv2.imencode(".jpg", img, [cv2.IMWRITE_JPEG_QUALITY, 75])
        return out.tobytes() if ok else jpg

    def post(self, path: str, d: dict):
        a = self.agent
        if path == "/api/heartbeat":
            a.last_app_beat = time.time()
            return {"ok": True}
        if path == "/api/stop":
            a.stop()
            return {"ok": True, "message": "Stopped; the arm holds where it is"}
        if path == "/api/estop":
            a.stop()
            self.arm.estop()
            return {"ok": True, "message": "Emergency stop"}
        if path == "/api/chat":
            text = str(d.get("text") or "").strip()
            if not text:
                raise ValueError("say what the arm should do")
            return {"ok": True, "job": a.start(text[:2000]).id}
        if path == "/api/approve":
            a.approve(bool(d.get("ok")))
            return {"ok": True}
        if path == "/api/settings":
            return {"ok": True, "settings": self.settings.update(d)}
        if path == "/api/keys/reload":
            self.reload_keys()
            return {"ok": True, "message": self.keys[self.settings["provider"]][1]["message"]}
        if path == "/api/calibrate":
            err = self.calib.set_points(d.get("points") or [], float(d.get("table_z_mm")))
            return {"ok": True, "message": f"Calibrated; worst point is off by {err} mm"}
        if path == "/api/clear":
            a.history = []
            return {"ok": True, "message": "Conversation cleared"}
        return None


def make_handler(svc: Service):
    class H(BaseHTTPRequestHandler):
        protocol_version = "HTTP/1.1"

        def log_message(self, *a):
            pass

        def _send(self, code, body, ctype="application/json"):
            raw = body if isinstance(body, bytes) else body.encode()
            self.send_response(code)
            self.send_header("Content-Type", ctype)
            self.send_header("Content-Length", str(len(raw)))
            self.send_header("Cache-Control", "no-store")
            self.end_headers()
            self.wfile.write(raw)

        def _json(self, obj, code=200):
            self._send(code, json.dumps(obj))

        def do_GET(self):
            u = urlparse(self.path)
            parts = [p for p in u.path.split("/") if p]
            try:
                if u.path == "/api/status":
                    return self._json(svc.status())
                if u.path == "/api/scene":
                    return self._json(svc.agent.scene())
                if u.path == "/api/job":
                    return self._json(svc.agent.job.view() if svc.agent.job else None)
                if len(parts) == 3 and parts[:2] == ["api", "camera"] and parts[2][:-4] in ROLES:
                    jpg = svc.frame(parts[2][:-4])
                    return self._send(200, jpg, "image/jpeg") if jpg else self._json({"error": "no frame"}, 404)
                return self._json({"error": "not found"}, 404)
            except Exception as e:
                return self._json({"error": str(e)}, 500)

        def do_POST(self):
            n = int(self.headers.get("Content-Length") or 0)
            try:
                d = json.loads(self.rfile.read(n) or b"{}")
            except ValueError:
                return self._json({"error": "body is not JSON"}, 400)
            try:
                r = svc.post(urlparse(self.path).path, d)
                return self._json(r) if r is not None else self._json({"error": "not found"}, 404)
            except (ValueError, TypeError) as e:
                return self._json({"error": str(e)}, 400)
            except DriverError as e:
                return self._json({"error": str(e)}, 409)
            except Exception as e:
                return self._json({"error": str(e)}, 500)
    return H


def main(argv=None):
    ap = argparse.ArgumentParser(description="NervusOS LLM Arm service")
    ap.add_argument("--bind", default="127.0.0.1")
    ap.add_argument("--port", type=int, default=8796)
    ap.add_argument("--driver", default="http://127.0.0.1:8770")
    ap.add_argument("--act", default="http://127.0.0.1:8795", help="ACT engine, which owns the cameras")
    ap.add_argument("--config", default=str(C.DEFAULT_CONFIG_DIR))
    ap.add_argument("--sim", action="store_true", help="simulated arm, blocks and cameras")
    ap.add_argument("--demo", action="store_true", help="with --sim: a scripted model instead of a real one")
    args = ap.parse_args(argv)
    if not ipaddress.ip_address(args.bind).is_loopback:
        raise SystemExit("refusing to listen on a non-loopback address: this API moves the arm and has no login")
    llm = None
    if args.demo:
        llm = ScriptedLLM([[("get_scene", {})], [("pick", {"object_id": "red-1"})],
                           [("place_on", {"object_id": "blue-1"})], [("go_home", {})], "Put the red block on the blue one."])
    svc = Service(Path(args.config), args.driver, args.act, args.sim, llm)
    srv = ThreadingHTTPServer((args.bind, args.port), make_handler(svc))
    srv.daemon_threads = True
    signal.signal(signal.SIGTERM, lambda *_: threading.Thread(target=srv.shutdown, daemon=True).start())
    print(f"LLM Arm {__version__} on http://{args.bind}:{args.port} ({'simulated' if args.sim else 'real'} arm)",
          flush=True)
    try:
        srv.serve_forever()
    finally:
        svc.agent.stop()
