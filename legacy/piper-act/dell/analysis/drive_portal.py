"""Drive the policy portal over HTTP like the page does (with heartbeats). Usage: drive_portal.py PORT MODE
MODE dry  : connect, load, enable, start pose, run 25 s, stop, mark, shutdown
MODE ready: connect + load only (no torque, no motion) and leave the portal running
"""
import json
import sys
import threading
import time
import urllib.request

port, mode = int(sys.argv[1]), sys.argv[2]
ckpt = sys.argv[3] if len(sys.argv) > 3 else "act_pick_place_v1_100000"
B = f"http://127.0.0.1:{port}"


def post(route, body=None):
    req = urllib.request.Request(B + "/api/" + route, data=json.dumps(body or {}).encode(),
                                 headers={"Content-Type": "application/json"}, method="POST")
    try:
        with urllib.request.urlopen(req, timeout=30) as r:
            return json.loads(r.read())
    except urllib.error.HTTPError as e:
        return json.loads(e.read())


def state():
    with urllib.request.urlopen(B + "/api/state", timeout=10) as r:
        return json.loads(r.read())


def wait(pred, timeout, what):
    t = time.time()
    while time.time() - t < timeout:
        s = state()
        if pred(s):
            return s
        time.sleep(0.25)
    raise SystemExit(f"timeout waiting for {what}: {state()['message']}")


for _ in range(120):
    try:
        state(); break
    except Exception:
        time.sleep(0.5)

alive = True
threading.Thread(target=lambda: [(post("heartbeat"), time.sleep(0.5)) for _ in iter(lambda: alive, False)], daemon=True).start()

print("connect", post("connect"))
s = wait(lambda s: s["camera"]["fps"] > 0 or s["camera"]["error"], 60, "camera")
print("  arm:", s["arm"]["ctrl_mode"], "live", s["arm"]["feedback_live"], s["arm"]["feedback_age_ms"], "| camera", s["camera"])
print("load", post("load", {"checkpoint": ckpt}))
s = wait(lambda s: s["policy"]["loaded"] or "failed" in s["message"], 180, "policy")
print("  ", s["message"])
for _ in range(3):
    pv = post("preview")
    print("preview", pv.get("preview", pv))
if mode == "ready":
    alive = False
    sys.exit(0)

if not state()["mock"]["arm"]:  # "dry" mode enables the motors and runs the policy
    raise SystemExit("refusing: 'dry' mode needs a portal running with --mock-arm")
print("enable", post("enable"))
print("start_pose", post("start_pose"))
wait(lambda s: s["state"] == "enabled", 30, "start pose")
print("run", post("run"))
t0 = time.time()
peak = None
while time.time() - t0 < 25:
    s = state()
    if s["state"] != "running":
        break
    peak = s
    time.sleep(1)
print("  during run:", json.dumps(peak["run"]), "policy", json.dumps(peak["policy"]), "cam fps", peak["camera"]["fps"])
print("stop", post("stop"))
s = wait(lambda s: s["state"] != "running", 10, "stop")
print("  after stop:", s["state"], "|", s["message"], "|", json.dumps(s["run"]))
print("params int-cast fix:", post("params", {"replan_every": "12.0", "gripper_squeeze": "5"}), state()["params"])
post("params", {"replan_every": 10})
print("mark", post("mark", {"outcome": "fail", "case": "A", "note": "DRY RUN - delete"}))
alive = False
post("shutdown")
