"""Full smoke test of the unified portal in mock mode: policy flow + recorder flow + correct + mark."""
import json, time, threading, urllib.request, urllib.error
B = "http://127.0.0.1:8790"


def post(route, body=None):
    req = urllib.request.Request(B + "/api/" + route, data=json.dumps(body or {}).encode(),
                                 headers={"Content-Type": "application/json"}, method="POST")
    try:
        with urllib.request.urlopen(req, timeout=60) as r:
            return json.loads(r.read())
    except urllib.error.HTTPError as e:
        return json.loads(e.read())


def get(p):
    with urllib.request.urlopen(B + p, timeout=30) as r:
        return json.loads(r.read())


def wait(pred, t, what):
    t0 = time.time()
    while time.time() - t0 < t:
        try:
            s = get("/api/state")
            if pred(s):
                return s
        except Exception:
            pass
        time.sleep(0.25)
    raise SystemExit(f"timeout: {what}")


for _ in range(120):
    try:
        get("/api/record/status"); break
    except Exception:
        time.sleep(0.5)

# heartbeat thread (page does this), after the portal is up
alive = True


def beat():
    while alive:
        try:
            post("heartbeat")
        except Exception:
            pass
        time.sleep(0.5)


threading.Thread(target=beat, daemon=True).start()

print("record status:", {k: get("/api/record/status")[k] for k in ("state", "dataset", "arm_connected", "mock")})
print("connect:", post("connect"))
s = wait(lambda s: s["state"] == "connected", 20, "connect")
print("  connected; camera fps:", s["camera"]["fps"])
print("load:", post("load", {"checkpoint": "act_pick_place_pt1_100000"}))
s = wait(lambda s: s["policy"]["loaded"], 300, "load")
print("  loaded", s["policy"]["name"], "chunk", s["policy"]["chunk_size"], "lat", s["policy"]["latency_ms"])
if not get("/api/state")["mock"]["arm"]:  # this test enables the motors and runs the policy
    raise SystemExit("refusing: the portal is not running with --mock-arm")
print("enable:", post("enable"))
print("run:", post("run"))
wait(lambda s: s["state"] == "running", 15, "run")
time.sleep(3)
s = get("/api/state")
print("  running: steps", s["run"]["steps"], "segment", s["run"]["segment"], "cam fps", s["camera"]["fps"])
print("correct:", post("correct"))
wait(lambda s: s["run"].get("segment") == "human", 5, "human segment")
print("  segment ->", get("/api/state")["run"]["segment"], "interventions", get("/api/state")["run"]["interventions"])
print("stop:", post("stop"))
wait(lambda s: s["state"] != "running", 10, "stop")
print("mark:", post("mark", {"outcome": "fail", "case": "A", "note": "unified smoke test"}))
print("  message:", get("/api/state")["message"])

# recorder flow (mock -> temp dataset, not the real one)
print("record start:", post("record/start", {"task": "smoke"}))
time.sleep(3)
rs = get("/api/record/status")
print("  recording:", rs["recording"], "frames", rs["episode_frames"], "fps", rs["fps_measured"])
print("record stop(discard):", post("record/stop", {"discard": True}))
time.sleep(2)
print("  state:", get("/api/record/status")["state"], "| saved:", get("/api/record/status")["saved_episodes"])

alive = False
post("shutdown")
print("done")
