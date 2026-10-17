#!/usr/bin/env python3
"""
DimOS Viewer API — read-only HTTP window into the running go2_fleet instances.

Same shape as piper_webctl.py (poll-friendly JSON over HTTP on localhost),
so it slots into a Nervus Service/App pair the same way ArmServerService /
RobotArmApp wrap piper_webctl.py.

Reads what go2_fleet.py already writes (~/.cache/go2_fleet/*.json) and briefly
samples each instance's own LCM bus for live odom — no new state of its own.

Endpoints:
    GET  /api/fleet          list of launched instances (role, serial, ip,
                              lcm_port, pid, alive)
    GET  /api/state/<port>   {connected, odom:{x,y}, rates:{topic:hz}} for one
                              LCM bus, sampled for ~1s
    POST /api/launch         {"leader": "<ip_or_serial>", "follower": "<...>"}
    POST /api/stop           stop all instances (go2_fleet.py --stop)
"""

import json
import os
import subprocess
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

ROBOTS_DIR = Path.home() / "NervusOS/robots"
FLEET_PY = ROBOTS_DIR / ".venv/bin/python"
FLEET_SCRIPT = ROBOTS_DIR / "scripts/go2_fleet.py"
STATE_DIR = Path.home() / ".cache/go2_fleet"
DIMOS_PY = ROBOTS_DIR / "vendor/dimos/.venv/bin/python"
MCAST = "239.255.76.67"
PORT = 8771
SAMPLE_SECS = 1.0

# Direct-link-first bind, same reasoning as piper_webctl.py: never 0.0.0.0.
DIRECT_LINK_IP = "192.168.127.10"


def pick_bind_address() -> str:
    import socket
    try:
        s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
        s.bind((DIRECT_LINK_IP, 0))
        s.close()
        return DIRECT_LINK_IP
    except OSError:
        return "127.0.0.1"


def pid_alive(pid: int) -> bool:
    try:
        os.kill(pid, 0)
        return True
    except OSError:
        return False


def fleet_list() -> list[dict]:
    out = []
    if not STATE_DIR.exists():
        return out
    for meta in sorted(STATE_DIR.glob("*.json")):
        if meta.name == "aes_keys.json":
            continue
        try:
            d = json.loads(meta.read_text())
        except Exception:
            continue
        d["alive"] = pid_alive(d.get("pid", -1))
        lcm_port = d.get("run_id", "").split("-")[-1] if "lcm_port" not in d else d["lcm_port"]
        out.append(d)
    return out


def sample_bus(port: int, secs: float = SAMPLE_SECS) -> dict:
    """Best-effort: one short-lived subprocess per call. Simple over clever —
    this endpoint is polled at a few Hz at most, never in a hot loop."""
    code = f"""
import lcm, time, json, sys
sys.path.insert(0, "{DIMOS_PY.parent.parent}/lib/python3.12/site-packages")
from dimos_lcm.geometry_msgs import PoseStamped
lc = lcm.LCM("udpm://{MCAST}:{port}?ttl=0")
counts = {{}}
odom = {{"x": None, "y": None}}
def on_odom(ch, data):
    p = PoseStamped.lcm_decode(data)
    odom["x"] = p.pose.position.x
    odom["y"] = p.pose.position.y
def on_any(ch, data):
    counts[ch] = counts.get(ch, 0) + 1
lc.subscribe(".*#geometry_msgs.PoseStamped", on_odom)
lc.subscribe(".*", on_any)
t = time.time()
while time.time() - t < {secs}:
    lc.handle_timeout(150)
print(json.dumps({{"rates": {{k: round(v/{secs}, 1) for k, v in counts.items()}}, "odom": odom}}))
"""
    try:
        r = subprocess.run([str(DIMOS_PY), "-c", code], capture_output=True, text=True, timeout=secs + 4)
        data = json.loads(r.stdout.strip().splitlines()[-1]) if r.stdout.strip() else {}
    except Exception as e:
        return {"connected": False, "error": str(e)}
    connected = bool(data.get("rates"))
    return {"connected": connected, **data}


class Handler(BaseHTTPRequestHandler):
    def log_message(self, *a):
        pass

    def _json(self, obj, code=200):
        body = json.dumps(obj).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        if self.path == "/api/fleet":
            return self._json({"instances": fleet_list()})
        if self.path.startswith("/api/state/"):
            try:
                port = int(self.path.rsplit("/", 1)[-1])
            except ValueError:
                return self._json({"error": "bad port"}, 400)
            return self._json(sample_bus(port))
        self._json({"error": "not found"}, 404)

    def do_POST(self):
        length = int(self.headers.get("Content-Length", 0))
        body = json.loads(self.rfile.read(length) or b"{}")

        if self.path == "/api/launch":
            leader, follower = body.get("leader"), body.get("follower")
            if not leader or not follower:
                return self._json({"error": "leader and follower required"}, 400)
            subprocess.Popen(
                [str(FLEET_PY), str(FLEET_SCRIPT), "--leader", leader, "--follower", follower],
                cwd=ROBOTS_DIR, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
            )
            return self._json({"ok": True})

        if self.path == "/api/stop":
            r = subprocess.run([str(FLEET_PY), str(FLEET_SCRIPT), "--stop"],
                               cwd=ROBOTS_DIR, capture_output=True, text=True, timeout=20)
            return self._json({"ok": True, "output": r.stdout})

        self._json({"error": "not found"}, 404)


def main():
    bind = pick_bind_address()
    srv = ThreadingHTTPServer((bind, PORT), Handler)
    print(f"DimOS viewer API on http://{bind}:{PORT}")
    srv.serve_forever()


if __name__ == "__main__":
    main()
