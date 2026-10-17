"""Dry-run test of the explicit correction trigger: connect, load, enable, run, then POST /api/correct, verify
segment switches to human, then stop + mark + shutdown."""
import json, sys, time, threading, urllib.request
B = "http://127.0.0.1:8792"


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


def wait(pred, t, what):
    t0 = time.time()
    while time.time() - t0 < t:
        s = state()
        if pred(s):
            return s
        time.sleep(0.2)
    raise SystemExit(f"timeout waiting {what}: {state().get('message')}")


for _ in range(120):
    try:
        state(); break
    except Exception:
        time.sleep(0.5)

# heartbeat like the page does, so the run is not killed on heartbeat_s
alive = True
threading.Thread(target=lambda: [(post("heartbeat"), time.sleep(0.5)) for _ in iter(lambda: alive, False)], daemon=True).start()

post("connect")
wait(lambda s: s["camera"]["fps"] > 0 or s["camera"]["error"], 60, "camera")
post("load", {"checkpoint": "act_pick_place_v1_100000"})
wait(lambda s: s["policy"]["loaded"], 180, "load")
if not state()["mock"]["arm"]:  # this test enables the motors and runs the policy
    raise SystemExit("refusing: the portal is not running with --mock-arm")
post("enable")
post("start_pose")
wait(lambda s: s["state"] == "enabled", 30, "start pose")
post("run")
wait(lambda s: s["state"] == "running", 15, "run")
time.sleep(2.0)
s = state()
print("before correct: segment=", s["run"].get("segment"), "interventions=", s["run"].get("interventions"))
print("correct:", post("correct"))
wait(lambda s: s["run"].get("segment") == "human", 5, "human segment")
s = state()
print("after correct: segment=", s["run"].get("segment"), "interventions=", s["run"].get("interventions"),
      "msg=", s["message"])
# verify a couple of rows in the rollout have segment=human
import glob, os
d = sorted(glob.glob(os.path.join(os.environ["TEMP"], "piper_posttrain_mock", "rollouts", "*")))[-1]
rows = [json.loads(l) for l in open(os.path.join(d, "episode.jsonl"))]
hum = sum(1 for r in rows if r.get("segment") == "human")
print(f"rollout {os.path.basename(d)}: {len(rows)} rows, human={hum}, sample st={rows[-1].get('st')}")
post("stop")
wait(lambda s: s["state"] != "running", 10, "stop")
post("mark", {"outcome": "fail", "case": "A", "note": "DRY correct test - delete"})
alive = False
post("shutdown")
print("done")
