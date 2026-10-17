#!/usr/bin/env python3
"""
PiPER Cartesian teleop — WASD/arrow keys drive end-effector translation and
rotation, served as a local web panel.

Runs on whatever machine owns the CAN bus. On the Mac that is a candleLight
USB-CAN stick (piper_mac_can shims piper_sdk's hardcoded socketcan onto the
gs_usb backend); on the RDK it is can0 directly.

Key map (verified against the real arm):
    W / S       +X / -X     forward / back        (arrows Up/Down duplicate)
    A / D       +Y / -Y     left / right          (arrows Left/Right duplicate)
    Q / E       +Z / -Z     up / down
    U / O       +RX / -RX   roll
    I / K       +RY / -RY   pitch
    J / L       +RZ / -RZ   yaw
    [ / ]       gripper close / open

Control design — "leashed" closed-loop jog. Each 20 Hz tick reads the ACTUAL end
pose and commands `target = actual + small_lead` on held axes only. An open-loop
integrator would let the commanded target drift past the workspace and trip
TARGET_POS_EXCEEDS_LIMIT, and a single blocked axis would freeze the rest; the
leash cannot outrun the arm, so neither happens.

Safety:
  * Motors start DISABLED. Nothing moves until ENABLE is pressed.
  * The browser sends a heartbeat; >1.5 s of silence halts the arm. Closing the
    tab, or the laptop sleeping, stops it.
  * Releasing a key clears that axis; keyup is also forced on window blur.
  * E-STOP sends EmergencyStop (CAN 0x150).
  * Binds to loopback by default — arm control is never put on a network.

    python3 piper_cartesian_ctl.py [--port 8770] [--iface candle] [--speed 15]
"""

import argparse
import json
import platform
import sys
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

sys.path.insert(0, str(__import__("pathlib").Path(__file__).resolve().parent))

if platform.system() == "Darwin":
    import piper_mac_can
    piper_mac_can.install()

try:
    from piper_sdk import C_PiperInterface_V2
except ImportError:
    raise SystemExit("piper_sdk not importable — use /Users/ianzhang/agilex/.venv/bin/python")

MDEG = 1000.0          # arm speaks 0.001 deg
MMM = 1000.0           # ...and 0.001 mm
GRIP_UNIT = 1000.0
GRIPPER_MM = (0.0, 70.0)

# Per-tick lead. Big enough to keep moving against gravity/friction, small enough
# that the commanded pose stays inside the workspace even at a hard stop.
JOG_LEAD_MM = 6.0
JOG_LEAD_DEG = 5.0

STREAM_HZ = 20
HEARTBEAT_TIMEOUT = 1.5
SPEED_MIN, SPEED_MAX = 1, 100

AXES = ["x", "y", "z", "rx", "ry", "rz"]

state_lock = threading.Lock()
STATE = {
    "held": {a: 0 for a in AXES},   # axis -> -1 / 0 / +1
    "gripper_mm": 0.0,
    "enabled": False,
    "estopped": False,
    "last_heartbeat": 0.0,
    "speed": 15,
    "pose": [0.0] * 6,
    "log": [],
}


def log_call(fn, params):
    entry = {"t": time.strftime("%H:%M:%S"), "fn": fn, "params": params}
    with state_lock:
        STATE["log"].insert(0, entry)
        del STATE["log"][40:]


class Arm:
    def __init__(self, iface, speed):
        self.piper = C_PiperInterface_V2(iface, judge_flag=False)
        self.piper.ConnectPort()
        for _ in range(50):
            if self.piper.isOk():
                break
            time.sleep(0.1)
        if not self.piper.isOk():
            raise SystemExit(
                f"CAN thread unhealthy on '{iface}'.\n"
                "  * Is the arm powered on?\n"
                "  * Is the CAN stick's DB9/terminal wired to the arm and terminated?\n"
                "  * Run piper_can_check.py to see whether the arm is on the bus."
            )
        time.sleep(0.5)
        self.speed = speed
        threading.Thread(target=self._loop, daemon=True).start()

    def end_pose(self):
        """Actual end pose as [X mm, Y mm, Z mm, RX deg, RY deg, RZ deg]."""
        e = self.piper.GetArmEndPoseMsgs().end_pose
        return [e.X_axis / MMM, e.Y_axis / MMM, e.Z_axis / MMM,
                e.RX_axis / MDEG, e.RY_axis / MDEG, e.RZ_axis / MDEG]

    def set_speed(self, v):
        self.speed = int(v)
        # move_mode 0x00 = MOVE P, the Cartesian point-to-point mode EndPoseCtrl needs.
        self.piper.MotionCtrl_2(0x01, 0x00, self.speed, 0x00)
        log_call("MotionCtrl_2", {"ctrl_mode": "0x01 CAN", "move_mode": "0x00 MOVE P",
                                  "move_spd_rate_ctrl": self.speed, "can_id": "0x151"})

    def enable(self):
        self.piper.EnableArm(7, 0x02)
        log_call("EnableArm", {"motor_num": 7, "enable_flag": "0x02", "can_id": "0x471"})
        time.sleep(1.2)
        self.set_speed(self.speed)
        with state_lock:
            STATE["enabled"] = True
            STATE["estopped"] = False
            STATE["held"] = {a: 0 for a in AXES}

    def disable(self):
        with state_lock:
            STATE["enabled"] = False
            STATE["held"] = {a: 0 for a in AXES}
        self.piper.DisableArm(7, 0x01)
        log_call("DisableArm", {"motor_num": 7, "enable_flag": "0x01", "can_id": "0x471"})

    def estop(self):
        with state_lock:
            STATE["enabled"] = False
            STATE["estopped"] = True
            STATE["held"] = {a: 0 for a in AXES}
        self.piper.EmergencyStop(0x01)
        log_call("EmergencyStop", {"flag": "0x01", "can_id": "0x150"})

    def set_gripper(self, mm):
        mm = max(GRIPPER_MM[0], min(GRIPPER_MM[1], mm))
        with state_lock:
            STATE["gripper_mm"] = mm
        self.piper.GripperCtrl(int(mm * GRIP_UNIT), 1000, 0x01, 0)
        log_call("GripperCtrl", {"angle_mm": round(mm, 1), "can_id": "0x159"})

    def _loop(self):
        """Leashed Cartesian jog: command actual-pose + lead on held axes only."""
        period = 1.0 / STREAM_HZ
        was_jogging = False
        while True:
            time.sleep(period)
            try:
                pose = self.end_pose()
            except Exception:
                continue
            with state_lock:
                STATE["pose"] = pose
                enabled = STATE["enabled"]
                fresh = (time.time() - STATE["last_heartbeat"]) < HEARTBEAT_TIMEOUT
                if enabled and not fresh and any(STATE["held"].values()):
                    STATE["held"] = {a: 0 for a in AXES}
                    log_call("(deadman)", {"reason": "heartbeat lost", "action": "jog cleared"})
                held = dict(STATE["held"])

            if not (enabled and fresh) or not any(held.values()):
                was_jogging = False
                continue

            target = list(pose)
            for i, ax in enumerate(AXES):
                d = held[ax]
                if d:
                    target[i] += d * (JOG_LEAD_MM if i < 3 else JOG_LEAD_DEG)

            self.piper.EndPoseCtrl(
                int(round(target[0] * MMM)), int(round(target[1] * MMM)),
                int(round(target[2] * MMM)), int(round(target[3] * MDEG)),
                int(round(target[4] * MDEG)), int(round(target[5] * MDEG)))

            if not was_jogging:
                was_jogging = True
                log_call("EndPoseCtrl", {"axes": [a for a in AXES if held[a]],
                                         "mode": "leashed +lead", "can_id": "0x152-0x154"})


class MockArm:
    """Simulated arm — same surface as Arm, no CAN. For driving the UI dry.

    Integrates held axes at the same per-tick lead the real leash uses, so the
    feel of the key mapping matches the hardware. Clamped to a plausible PiPER
    workspace so jogging into a wall behaves like the real thing.
    """

    LIMITS = [(-120.0, 700.0), (-520.0, 520.0), (-250.0, 720.0),
              (-180.0, 180.0), (-180.0, 180.0), (-180.0, 180.0)]

    def __init__(self, iface, speed):
        self.speed = speed
        # Roughly the real arm's pose at the zero joint position.
        self.pose = [57.0, 0.0, 214.0, 0.0, 85.0, 0.0]
        log_call("(mock)", {"note": "simulated arm, no CAN bus"})
        threading.Thread(target=self._loop, daemon=True).start()

    def end_pose(self):
        return list(self.pose)

    def set_speed(self, v):
        self.speed = int(v)
        log_call("MotionCtrl_2", {"ctrl_mode": "0x01 CAN", "move_mode": "0x00 MOVE P",
                                  "move_spd_rate_ctrl": self.speed, "can_id": "0x151",
                                  "mock": True})

    def enable(self):
        with state_lock:
            STATE["enabled"] = True
            STATE["estopped"] = False
            STATE["held"] = {a: 0 for a in AXES}
        log_call("EnableArm", {"motor_num": 7, "enable_flag": "0x02",
                               "can_id": "0x471", "mock": True})

    def disable(self):
        with state_lock:
            STATE["enabled"] = False
            STATE["held"] = {a: 0 for a in AXES}
        log_call("DisableArm", {"motor_num": 7, "can_id": "0x471", "mock": True})

    def estop(self):
        with state_lock:
            STATE["enabled"] = False
            STATE["estopped"] = True
            STATE["held"] = {a: 0 for a in AXES}
        log_call("EmergencyStop", {"flag": "0x01", "can_id": "0x150", "mock": True})

    def set_gripper(self, mm):
        mm = max(GRIPPER_MM[0], min(GRIPPER_MM[1], mm))
        with state_lock:
            STATE["gripper_mm"] = mm
        log_call("GripperCtrl", {"angle_mm": round(mm, 1), "can_id": "0x159", "mock": True})

    def _loop(self):
        period = 1.0 / STREAM_HZ
        was_jogging = False
        while True:
            time.sleep(period)
            with state_lock:
                enabled = STATE["enabled"]
                fresh = (time.time() - STATE["last_heartbeat"]) < HEARTBEAT_TIMEOUT
                if enabled and not fresh and any(STATE["held"].values()):
                    STATE["held"] = {a: 0 for a in AXES}
                    log_call("(deadman)", {"reason": "heartbeat lost", "action": "jog cleared"})
                held = dict(STATE["held"])

            if not (enabled and fresh) or not any(held.values()):
                was_jogging = False
                with state_lock:
                    STATE["pose"] = list(self.pose)
                continue

            # Speed scales the simulated travel the way move_spd_rate_ctrl does.
            scale = max(0.1, self.speed / 15.0)
            for i, ax in enumerate(AXES):
                d = held[ax]
                if d:
                    step = (JOG_LEAD_MM if i < 3 else JOG_LEAD_DEG) * d * scale * 0.35
                    lo, hi = self.LIMITS[i]
                    self.pose[i] = max(lo, min(hi, self.pose[i] + step))
            with state_lock:
                STATE["pose"] = list(self.pose)

            if not was_jogging:
                was_jogging = True
                log_call("EndPoseCtrl", {"axes": [a for a in AXES if held[a]],
                                         "mode": "leashed +lead", "can_id": "0x152-0x154",
                                         "mock": True})


ARM = None
MOCK = False


class Handler(BaseHTTPRequestHandler):
    def log_message(self, *a):
        pass

    def _send(self, code, body, ctype="application/json"):
        raw = body.encode() if isinstance(body, str) else body
        self.send_response(code)
        self.send_header("Content-Type", ctype)
        self.send_header("Content-Length", str(len(raw)))
        self.end_headers()
        self.wfile.write(raw)

    def do_GET(self):
        if self.path in ("/", "/index.html"):
            return self._send(200, HTML, "text/html; charset=utf-8")
        if self.path == "/api/state":
            with state_lock:
                return self._send(200, json.dumps({
                    "pose": [round(v, 2) for v in STATE["pose"]],
                    "held": STATE["held"], "gripper_mm": STATE["gripper_mm"],
                    "enabled": STATE["enabled"], "estopped": STATE["estopped"],
                    "speed": STATE["speed"], "gripper_range": GRIPPER_MM,
                    "speed_range": [SPEED_MIN, SPEED_MAX], "log": STATE["log"][:14],
                    "mock": MOCK,
                }))
        return self._send(404, json.dumps({"error": "not found"}))

    def do_POST(self):
        n = int(self.headers.get("Content-Length", 0))
        try:
            data = json.loads(self.rfile.read(n) or b"{}")
        except Exception:
            data = {}

        if self.path == "/api/heartbeat":
            with state_lock:
                STATE["last_heartbeat"] = time.time()
            return self._send(200, json.dumps({"ok": True}))

        if self.path == "/api/jog":
            ax = str(data.get("axis", ""))
            if ax not in AXES:
                return self._send(400, json.dumps({"error": "bad axis"}))
            on = bool(data.get("on"))
            d = int(data.get("dir", 0))
            with state_lock:
                STATE["held"][ax] = (1 if d > 0 else -1) if on else 0
            return self._send(200, json.dumps({"ok": True}))

        if self.path == "/api/jogstop":
            with state_lock:
                STATE["held"] = {a: 0 for a in AXES}
            return self._send(200, json.dumps({"ok": True}))

        if self.path == "/api/gripper":
            ARM.set_gripper(float(data.get("mm", 0.0)))
            return self._send(200, json.dumps({"ok": True}))

        if self.path == "/api/enable":
            ARM.enable() if data.get("on") else ARM.disable()
            return self._send(200, json.dumps({"ok": True}))

        if self.path == "/api/speed":
            v = int(max(SPEED_MIN, min(SPEED_MAX, int(data.get("speed", 15)))))
            with state_lock:
                STATE["speed"] = v
            ARM.set_speed(v)
            return self._send(200, json.dumps({"ok": True, "speed": v}))

        if self.path == "/api/estop":
            ARM.estop()
            return self._send(200, json.dumps({"ok": True}))

        return self._send(404, json.dumps({"error": "not found"}))


HTML = r"""<!doctype html>
<html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>PiPER Cartesian Teleop · Nervus OS</title>
<style>
:root{--primary:#3B82F6;--success:#16A34A;--danger:#DC2626;--warn:#D97706;
 --surface:#FFF;--text:#111827;--muted:#6B7280;--line:#D1D5DB;--bg:#F3F4F6;
 --mono:ui-monospace,SFMono-Regular,Menlo,monospace;
 --disp:"Space Grotesk",system-ui,sans-serif;}
@media (prefers-color-scheme:dark){:root{--surface:#111827;--text:#F9FAFB;
 --bg:#0B0F19;--line:#374151;--muted:#9CA3AF;}}
*{box-sizing:border-box}
body{margin:0;font-family:system-ui,-apple-system,sans-serif;font-size:16px;
 color:var(--text);background:var(--bg);}
.wrap{max-width:940px;margin:0 auto;padding:24px 16px}
header{display:flex;align-items:baseline;gap:12px;flex-wrap:wrap;margin-bottom:20px}
h1{font-family:var(--disp);font-size:30px;margin:0;letter-spacing:-.02em}
.sub{color:var(--muted);font-size:13px;font-family:var(--mono)}
.panel{background:var(--surface);border:2px solid var(--text);padding:16px;
 margin-bottom:16px;box-shadow:4px 4px 0 var(--text)}
h2{font-family:var(--disp);font-size:15px;text-transform:uppercase;
 letter-spacing:.08em;margin:0 0 12px;color:var(--muted)}
.bar{display:flex;gap:8px;flex-wrap:wrap;align-items:center;margin-bottom:16px}
button{font-family:var(--disp);font-size:15px;font-weight:600;padding:8px 16px;
 border:2px solid var(--text);background:var(--surface);color:var(--text);
 cursor:pointer;box-shadow:2px 2px 0 var(--text)}
button:hover{transform:translate(1px,1px);box-shadow:1px 1px 0 var(--text)}
button.on{background:var(--success);color:#fff}
button.off{background:var(--muted);color:#fff}
button.stop{background:var(--danger);color:#fff;font-size:17px}
.pose{display:grid;grid-template-columns:repeat(3,1fr);gap:10px}
.cell{border:1px solid var(--line);padding:10px}
.cell .k{font-family:var(--mono);font-size:11px;color:var(--muted);
 text-transform:uppercase;letter-spacing:.06em}
.cell .v{font-family:var(--mono);font-size:20px;font-variant-numeric:tabular-nums}
.cell.live{border-color:var(--primary);background:color-mix(in srgb,var(--primary) 10%,transparent)}
.kbd{display:flex;gap:28px;flex-wrap:wrap;justify-content:center}
.clus{text-align:center}
.clus .lab{font-family:var(--mono);font-size:11px;color:var(--muted);
 text-transform:uppercase;letter-spacing:.06em;margin-bottom:8px}
.row{display:flex;gap:6px;justify-content:center;margin-bottom:6px}
.key{width:58px;height:52px;border:2px solid var(--text);background:var(--surface);
 display:flex;flex-direction:column;align-items:center;justify-content:center;
 font-family:var(--mono);cursor:pointer;user-select:none;
 box-shadow:2px 2px 0 var(--text);transition:none}
.key .ch{font-size:17px;font-weight:700;line-height:1}
.key .fn{font-size:9px;color:var(--muted);margin-top:3px;line-height:1}
.key.down{background:var(--primary);color:#fff;transform:translate(2px,2px);
 box-shadow:0 0 0 var(--text)}
.key.down .fn{color:#fff}
.key.spacer{border:none;background:none;box-shadow:none;cursor:default}
.log{font-family:var(--mono);font-size:11px;color:var(--muted);
 max-height:150px;overflow-y:auto;line-height:1.6}
.pill{font-family:var(--mono);font-size:12px;padding:3px 10px;border:1px solid var(--line)}
.pill.ok{background:var(--success);color:#fff;border-color:var(--success)}
.pill.bad{background:var(--danger);color:#fff;border-color:var(--danger)}
input[type=range]{width:150px;vertical-align:middle}
</style></head><body>
<div class="wrap">
<header><h1>PiPER Cartesian Teleop</h1>
<span class="sub">leashed end-effector jog · 20 Hz · deadman 1.5 s</span></header>

<div class="panel">
  <div class="bar">
    <button id="en" class="off">ENABLE</button>
    <button id="stop" class="stop">E-STOP</button>
    <span class="pill" id="status">connecting…</span>
    <span class="pill" id="mock" hidden style="background:var(--warn);color:#fff;border-color:var(--warn)">MOCK — simulated</span>
    <span style="flex:1"></span>
    <label class="sub">speed <input type="range" id="spd" min="1" max="100" value="15">
      <span id="spdv" class="sub">15%</span></label>
  </div>
  <div class="bar">
    <label class="sub">gripper <input type="range" id="grip" min="0" max="70" step="1" value="0">
      <span id="gripv" class="sub">0 mm</span></label>
    <span class="sub">&nbsp;[ close &nbsp; ] open</span>
  </div>
</div>

<div class="panel">
  <h2>End-effector pose</h2>
  <div class="pose" id="pose"></div>
</div>

<div class="panel">
  <h2>Keyboard — hold a key to jog</h2>
  <div class="kbd">
    <div class="clus"><div class="lab">Translate</div>
      <div class="row">
        <div class="key spacer"></div>
        <div class="key" data-key="w"><span class="ch">W</span><span class="fn">+X fwd</span></div>
        <div class="key spacer"></div>
        <div class="key" data-key="q"><span class="ch">Q</span><span class="fn">+Z up</span></div>
      </div>
      <div class="row">
        <div class="key" data-key="a"><span class="ch">A</span><span class="fn">+Y left</span></div>
        <div class="key" data-key="s"><span class="ch">S</span><span class="fn">&minus;X back</span></div>
        <div class="key" data-key="d"><span class="ch">D</span><span class="fn">&minus;Y right</span></div>
        <div class="key" data-key="e"><span class="ch">E</span><span class="fn">&minus;Z down</span></div>
      </div>
    </div>
    <div class="clus"><div class="lab">Rotate</div>
      <div class="row">
        <div class="key" data-key="u"><span class="ch">U</span><span class="fn">+roll</span></div>
        <div class="key" data-key="i"><span class="ch">I</span><span class="fn">+pitch</span></div>
        <div class="key" data-key="j"><span class="ch">J</span><span class="fn">+yaw</span></div>
      </div>
      <div class="row">
        <div class="key" data-key="o"><span class="ch">O</span><span class="fn">&minus;roll</span></div>
        <div class="key" data-key="k"><span class="ch">K</span><span class="fn">&minus;pitch</span></div>
        <div class="key" data-key="l"><span class="ch">L</span><span class="fn">&minus;yaw</span></div>
      </div>
    </div>
    <div class="clus"><div class="lab">Arrows</div>
      <div class="row">
        <div class="key spacer"></div>
        <div class="key" data-key="arrowup"><span class="ch">&uarr;</span><span class="fn">+X</span></div>
        <div class="key spacer"></div>
      </div>
      <div class="row">
        <div class="key" data-key="arrowleft"><span class="ch">&larr;</span><span class="fn">+Y</span></div>
        <div class="key" data-key="arrowdown"><span class="ch">&darr;</span><span class="fn">&minus;X</span></div>
        <div class="key" data-key="arrowright"><span class="ch">&rarr;</span><span class="fn">&minus;Y</span></div>
      </div>
    </div>
  </div>
</div>

<div class="panel"><h2>CAN calls</h2><div class="log" id="log"></div></div>
</div>

<script>
// key -> [axis, direction]. Arrows duplicate W/S/A/D so either hand works.
const MAP = {
  w:["x",1], s:["x",-1], a:["y",1], d:["y",-1], q:["z",1], e:["z",-1],
  u:["rx",1], o:["rx",-1], i:["ry",1], k:["ry",-1], j:["rz",1], l:["rz",-1],
  arrowup:["x",1], arrowdown:["x",-1], arrowleft:["y",1], arrowright:["y",-1],
};
const POSE_LABELS = [["X","mm"],["Y","mm"],["Z","mm"],["RX","deg"],["RY","deg"],["RZ","deg"]];
const down = new Set();

const post = (p, b) => fetch(p, {method:"POST", headers:{"Content-Type":"application/json"},
  body: JSON.stringify(b||{})});

function press(k){
  if(!MAP[k] || down.has(k)) return;
  down.add(k);
  document.querySelectorAll(`[data-key="${k}"]`).forEach(e=>e.classList.add("down"));
  post("/api/jog", {axis:MAP[k][0], dir:MAP[k][1], on:true});
}
function release(k){
  if(!MAP[k] || !down.has(k)) return;
  down.delete(k);
  document.querySelectorAll(`[data-key="${k}"]`).forEach(e=>e.classList.remove("down"));
  // Only clear the axis if no other held key still drives it (e.g. W and Up).
  const ax = MAP[k][0];
  const other = [...down].find(o => MAP[o][0] === ax);
  if(other) post("/api/jog", {axis:ax, dir:MAP[other][1], on:true});
  else post("/api/jog", {axis:ax, dir:0, on:false});
}
function releaseAll(){ [...down].forEach(release); post("/api/jogstop"); }

addEventListener("keydown", e => {
  if(e.metaKey || e.ctrlKey || e.altKey) return;
  const k = e.key.toLowerCase();
  if(k === "[" ){ nudgeGrip(-8); return; }
  if(k === "]" ){ nudgeGrip(+8); return; }
  if(MAP[k]){ e.preventDefault(); press(k); }
});
addEventListener("keyup", e => {
  const k = e.key.toLowerCase();
  if(MAP[k]){ e.preventDefault(); release(k); }
});
// A key held while the window loses focus never fires keyup — stop the arm.
addEventListener("blur", releaseAll);
document.addEventListener("visibilitychange", () => { if(document.hidden) releaseAll(); });

// Virtual keyboard: same path as the physical keys.
document.querySelectorAll(".key[data-key]").forEach(el => {
  const k = el.dataset.key;
  const go = e => { e.preventDefault(); press(k); };
  const st = e => { e.preventDefault(); release(k); };
  el.addEventListener("mousedown", go); el.addEventListener("mouseup", st);
  el.addEventListener("mouseleave", st);
  el.addEventListener("touchstart", go, {passive:false});
  el.addEventListener("touchend", st);
});

const grip = document.getElementById("grip"), gripv = document.getElementById("gripv");
function nudgeGrip(d){
  grip.value = Math.max(0, Math.min(70, (+grip.value) + d));
  gripv.textContent = grip.value + " mm";
  post("/api/gripper", {mm:+grip.value});
}
grip.addEventListener("input", () => {
  gripv.textContent = grip.value + " mm";
  post("/api/gripper", {mm:+grip.value});
});

const spd = document.getElementById("spd"), spdv = document.getElementById("spdv");
spd.addEventListener("change", () => {
  spdv.textContent = spd.value + "%";
  post("/api/speed", {speed:+spd.value});
});
spd.addEventListener("input", () => { spdv.textContent = spd.value + "%"; });

document.getElementById("en").addEventListener("click", e => {
  post("/api/enable", {on: e.target.classList.contains("off")});
});
document.getElementById("stop").addEventListener("click", () => { releaseAll(); post("/api/estop"); });

setInterval(() => post("/api/heartbeat"), 400);

async function poll(){
  try{
    const s = await (await fetch("/api/state")).json();
    document.getElementById("pose").innerHTML = POSE_LABELS.map((L,i) => {
      const ax = ["x","y","z","rx","ry","rz"][i];
      const live = s.held[ax] ? " live" : "";
      return `<div class="cell${live}"><div class="k">${L[0]} (${L[1]})</div>
              <div class="v">${s.pose[i].toFixed(1)}</div></div>`;
    }).join("");
    const en = document.getElementById("en");
    en.className = s.enabled ? "on" : "off";
    en.textContent = s.enabled ? "ENABLED" : "ENABLE";
    const st = document.getElementById("status");
    st.textContent = s.estopped ? "E-STOPPED" : (s.enabled ? "live" : "motors off");
    st.className = "pill " + (s.estopped ? "bad" : (s.enabled ? "ok" : ""));
    document.getElementById("mock").hidden = !s.mock;
    document.getElementById("log").innerHTML = s.log.map(e =>
      `${e.t} <b>${e.fn}</b> ${JSON.stringify(e.params)}`).join("<br>");
  }catch(err){
    document.getElementById("status").textContent = "server lost";
    document.getElementById("status").className = "pill bad";
  }
}
setInterval(poll, 200); poll();
</script></body></html>
"""


def main():
    global ARM, MOCK
    default_iface = "candle" if platform.system() == "Darwin" else "can0"
    ap = argparse.ArgumentParser()
    ap.add_argument("--port", type=int, default=8770)
    ap.add_argument("--iface", default=default_iface)
    ap.add_argument("--speed", type=int, default=15)
    ap.add_argument("--bind", default="127.0.0.1")
    ap.add_argument("--mock", action="store_true",
                    help="simulate the arm; no CAN bus, no hardware needed")
    args = ap.parse_args()

    with state_lock:
        STATE["speed"] = args.speed
    MOCK = args.mock
    ARM = MockArm(args.iface, args.speed) if args.mock else Arm(args.iface, args.speed)
    srv = ThreadingHTTPServer((args.bind, args.port), Handler)
    print(f"\nPiPER Cartesian teleop on http://{args.bind}:{args.port}")
    if args.mock:
        print("  *** MOCK MODE — simulated arm, nothing physical will move ***")
    print("  W/S = +/-X   A/D = +/-Y   Q/E = +/-Z   (arrows duplicate X/Y)")
    print("  U/O = roll   I/K = pitch  J/L = yaw    [ / ] = gripper")
    print("  Press ENABLE in the page before anything moves.\n")
    try:
        srv.serve_forever()
    except KeyboardInterrupt:
        print("\nshutting down; disabling motors")
        try:
            ARM.disable()
        except Exception:
            pass


if __name__ == "__main__":
    main()
