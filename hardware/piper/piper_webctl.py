#!/usr/bin/env python3
"""
PiPER web control panel: per-joint sliders, Cartesian jog and gripper control.

This is the NervusOS hardware layer for the AgileX PiPER arm. It owns the SocketCAN
bus (can0) and serves a small HTTP API that NervusOS apps (the Robot Arm app) call.
On the Ubuntu host it runs as nervus-piper.service, starts and stops together with
NervusOS, and listens on loopback only, so arm control is never exposed to the
network. To use the browser panel from another machine, tunnel it:

    ssh -L 8770:localhost:8770 <ubuntu-host>
    open http://localhost:8770

Safety design:
  * Motors start DISABLED. Nothing moves until you press ENABLE.
  * The client sends a heartbeat; if it stops for >1.5 s the control loop stops
    streaming JointCtrl and the arm halts. Closing the tab stops the arm.
  * E-STOP button sends EmergencyStop (CAN 0x150).
  * Targets are clamped to the manual's joint limits, server-side.

Units: degrees are authoritative (the arm speaks 0.001 deg). Percent is only a
convenience mapping over each joint's asymmetric range.

    python3 piper_webctl.py [--bind 127.0.0.1] [--port 8770] [--iface can0] [--speed 15]
"""

import argparse
import json
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

try:
    from piper_sdk import C_PiperInterface_V2
except ImportError:
    raise SystemExit("piper_sdk not importable; use the venv at /opt/nervus/piper/venv")

MDEG = 1000.0
GRIP_UNIT = 1000.0
JOINT_LIMITS = {1: (-154.0, 154.0), 2: (0.0, 195.0), 3: (-175.0, 0.0),
                4: (-106.0, 106.0), 5: (-75.0, 75.0), 6: (-100.0, 100.0)}
GRIPPER_MM = (0.0, 70.0)
# move_spd_rate_ctrl (CAN 0x151) is a percentage of the arm's RATED speed.
# Floor is 1, not 0: zero stalls motion mid-travel, which the calibration probe
# would then misread as a blocked joint.
SPEED_MIN, SPEED_MAX = 1, 100
STREAM_HZ = 20
HEARTBEAT_TIMEOUT = 1.5

# Cartesian jog is LEASHED, closed-loop rate control: while a key is held, the
# streamed TCP target is only ever (measured pose + a small constant lead) on the
# held axis. Because the target is re-derived from the ACTUAL pose every tick, it
# can never run away from the arm, so a hand-pose near the workspace edge can no
# longer latch TARGET_POS_EXCEEDS_LIMIT from accumulated drift, and a blocked axis
# never freezes the others. The lead just has to be big enough to keep the servo
# from stalling exactly on target; the speed slider (move_spd_rate_ctrl) sets the
# actual velocity.
JOG_LEAD_MM = 6.0    # translation target leads the measured pose by 6 mm
JOG_LEAD_DEG = 5.0   # rotation target leads by 5 deg
# Coarse runaway guard on the leashed target (the arm still enforces its true,
# joint-limited workspace on top of this).
POSE_CLAMP_MM = 900.0
POSE_CLAMP_DEG = 185.0

# WRIST joint jog: held-key direct joint control (MOVE J) used to articulate the
# wrist (J4/J5/J6) while J1-3 stay put — the tip is NOT held fixed (unlike the
# Cartesian gimbal). Each tick advances the held joint's target by this many deg;
# clamped to the joint's limit, so it can never exceed the mechanical range.
JOINT_JOG_STEP = 0.6   # deg per 20 Hz tick -> ~12 deg/s while held

state_lock = threading.Lock()
STATE = {
    "targets": [0.0] * 6,
    "mode": "joint",          # "joint" (per-joint jog) | "cartesian" (WASD end-effector jog)
    "pose": [0.0] * 6,        # last streamed TCP target: X,Y,Z (mm), RX,RY,RZ (deg) — for display
    "jog": [0.0] * 6,         # per-axis held-key command: +lead / -lead / 0. Drives leashed cartesian jog.
    "jjog": [0.0] * 6,        # per-JOINT held-key command (deg/tick): drives wrist joint jog in MOVE J.
    "gripper_mm": 0.0,
    "enabled": False,
    "estopped": False,
    "last_heartbeat": 0.0,
    "speed": 15,
    # move_spd_rate_ctrl range. Never allow 0 — that stalls motion mid-travel.
    "speed_min": 1,
    "speed_max": 100,
    "log": [],
}


def log_call(fn, params):
    entry = {"t": time.strftime("%H:%M:%S"), "fn": fn, "params": params}
    with state_lock:
        STATE["log"].insert(0, entry)
        del STATE["log"][40:]


def clamp(j, deg):
    lo, hi = JOINT_LIMITS[j]
    return max(lo, min(hi, deg))


class Arm:
    def __init__(self, iface, speed):
        self.piper = C_PiperInterface_V2(iface)
        self.piper.ConnectPort()
        for _ in range(50):
            if self.piper.isOk():
                break
            time.sleep(0.1)
        if not self.piper.isOk():
            raise SystemExit(f"CAN thread unhealthy on {iface}")
        time.sleep(0.5)
        self.speed = speed
        threading.Thread(target=self._loop, daemon=True).start()

    def set_speed(self, v):
        """Change move_spd_rate_ctrl (CAN 0x151) live, mid-session."""
        self.speed = int(v)
        self.piper.MotionCtrl_2(0x01, 0x01, self.speed, 0x00)
        log_call("MotionCtrl_2", {"ctrl_mode": "0x01 CAN", "move_mode": "0x01 MOVE J",
                                  "move_spd_rate_ctrl": self.speed, "can_id": "0x151"})

    def joints(self):
        j = self.piper.GetArmJointMsgs().joint_state
        return [getattr(j, f"joint_{n}") / MDEG for n in range(1, 7)]

    def end_pose(self):
        """Current TCP pose: [X,Y,Z] mm, [RX,RY,RZ] deg (SDK reports 0.001 mm / 0.001 deg)."""
        e = self.piper.GetArmEndPoseMsgs().end_pose
        return [e.X_axis / 1000.0, e.Y_axis / 1000.0, e.Z_axis / 1000.0,
                e.RX_axis / 1000.0, e.RY_axis / 1000.0, e.RZ_axis / 1000.0]

    def arm_status(self):
        """Controller status string — surfaces e.g. TARGET_POS_EXCEEDS_LIMIT, which blocks
        Cartesian moves when the arm is hand-posed outside its valid workspace."""
        try:
            return str(getattr(self.piper.GetArmStatus().arm_status, "arm_status", "?"))
        except Exception:
            return None

    def enter_joint(self):
        """Per-joint jog (MOVE J). Re-seed joint targets from the live pose first."""
        with state_lock:
            STATE["mode"] = "joint"
            STATE["targets"] = self.joints()
            STATE["jog"] = [0.0] * 6   # drop any held Cartesian keys
            STATE["jjog"] = [0.0] * 6  # start wrist-jog clear (targets seeded to live joints)
        self.piper.MotionCtrl_2(0x01, 0x01, self.speed, 0x00)
        log_call("MotionCtrl_2", {"move_mode": "0x01 MOVE J", "can_id": "0x151"})

    def enter_cartesian(self):
        """Cartesian WASD jog (MOVE P). Seed the target from the live TCP pose so the
        arm does not jump when the mode flips."""
        with state_lock:
            STATE["mode"] = "cartesian"
            STATE["pose"] = self.end_pose()
            STATE["jog"] = [0.0] * 6   # start held-keys clear
            STATE["jjog"] = [0.0] * 6  # drop any held wrist-joint keys
        self.piper.MotionCtrl_2(0x01, 0x00, self.speed, 0x00)
        log_call("MotionCtrl_2", {"move_mode": "0x00 MOVE P", "can_id": "0x151"})

    def enable(self):
        self.piper.EnableArm(7, 0x02)
        log_call("EnableArm", {"motor_num": 7, "enable_flag": "0x02", "can_id": "0x471"})
        time.sleep(1.2)
        self.piper.MotionCtrl_2(0x01, 0x01, self.speed, 0x00)
        log_call("MotionCtrl_2", {"ctrl_mode": "0x01 CAN", "move_mode": "0x01 MOVE J",
                                  "speed": self.speed, "can_id": "0x151"})
        with state_lock:
            STATE["enabled"] = True
            STATE["estopped"] = False
            STATE["mode"] = "joint"
            STATE["targets"] = self.joints()
            STATE["pose"] = self.end_pose()

    def disable(self):
        with state_lock:
            STATE["enabled"] = False
        self.piper.DisableArm(7, 0x01)
        log_call("DisableArm", {"motor_num": 7, "enable_flag": "0x01", "can_id": "0x471"})

    def estop(self):
        with state_lock:
            STATE["enabled"] = False
            STATE["estopped"] = True
        self.piper.EmergencyStop(0x01)
        log_call("EmergencyStop", {"flag": "0x01", "can_id": "0x150"})

    def recover(self):
        """Clear a latched collision/fault WITHOUT unplugging the power cable.

        A collision trips Piper's protection and latches a fault that EnableArm
        alone cannot clear — which is why a power-cycle was the only way back.
        ResetPiper (CAN 0x150) clears all errors and internal flags (the arm goes
        limp for a moment, exactly like a power-cycle), then we re-enable and
        resume streaming.
        """
        with state_lock:
            STATE["enabled"] = False
            STATE["estopped"] = False
        self.piper.ResetPiper()
        log_call("ResetPiper", {"note": "clear all errors + flags", "can_id": "0x150"})
        time.sleep(0.5)
        self.enable()

    def set_gripper(self, mm):
        mm = max(GRIPPER_MM[0], min(GRIPPER_MM[1], mm))
        with state_lock:
            STATE["gripper_mm"] = mm
        self.piper.GripperCtrl(int(mm * GRIP_UNIT), 1000, 0x01, 0)
        log_call("GripperCtrl", {"angle_mm": round(mm, 1), "effort": 1000,
                                 "code": "0x01", "can_id": "0x159"})

    def _loop(self):
        """Stream JointCtrl only while enabled AND the browser heartbeat is fresh."""
        period = 1.0 / STREAM_HZ
        streaming = False
        while True:
            time.sleep(period)
            with state_lock:
                enabled = STATE["enabled"]
                fresh = (time.time() - STATE["last_heartbeat"]) < HEARTBEAT_TIMEOUT
                mode = STATE["mode"]
                tgt = list(STATE["targets"])
                jog = list(STATE["jog"])
                jjog = list(STATE["jjog"])
            if enabled and fresh:
                if mode == "cartesian":
                    # LEASHED closed-loop jog: re-derive the target from the arm's
                    # MEASURED pose every tick, adding a small lead only on axes whose
                    # key is held. Non-held axes are commanded to hold their current
                    # value. The target therefore stays within one lead of reality —
                    # it cannot drift out of the workspace and latch
                    # TARGET_POS_EXCEEDS_LIMIT, and a blocked axis can't freeze the rest.
                    actual = self.end_pose()
                    target = []
                    for ax in range(6):
                        v = actual[ax] + jog[ax]
                        if ax < 3:
                            v = max(-POSE_CLAMP_MM, min(POSE_CLAMP_MM, v))
                        else:
                            v = max(-POSE_CLAMP_DEG, min(POSE_CLAMP_DEG, v))
                        target.append(v)
                    with state_lock:
                        STATE["pose"] = target
                    # EndPoseCtrl (CAN 0x152-0x154). MOVE P armed on mode entry.
                    # Units: 0.001 mm / 0.001 deg.
                    self.piper.EndPoseCtrl(*[int(round(v * 1000)) for v in target])
                else:
                    # WRIST joint jog: advance each held joint's target by its per-tick
                    # step, clamped to the joint limit. Non-held joints (jjog==0) hold —
                    # so holding a wrist key moves J4/J5/J6 while J1-3 stay put.
                    if any(jjog):
                        with state_lock:
                            for jn in range(6):
                                if jjog[jn]:
                                    STATE["targets"][jn] = clamp(jn + 1, STATE["targets"][jn] + jjog[jn])
                            tgt = list(STATE["targets"])
                    self.piper.JointCtrl(*[int(round(v * MDEG)) for v in tgt])
                streaming = True
            elif streaming:
                streaming = False
                if enabled and not fresh:
                    log_call("(deadman)", {"reason": "heartbeat lost", "action": "stopped streaming"})


ARM = None

# Address of a direct point-to-point Ethernet link to a laptop, if there is one
# (the RDK used 10.90.0.153 on eth0). Only `--bind auto` uses it. It must never be
# 0.0.0.0: that publishes arm control on every network, including shared Wi-Fi.
DIRECT_LINK_IP = "10.90.0.153"


def pick_bind_address():
    """Prefer the direct Ethernet link; fall back to loopback if it isn't up.

    Deliberately NOT 0.0.0.0: that would also publish arm control onto the venue
    Wi-Fi, where other people are.
    """
    import socket
    try:
        s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
        s.bind((DIRECT_LINK_IP, 0))
        s.close()
        return DIRECT_LINK_IP
    except OSError:
        return "127.0.0.1"


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
            try:
                cur = ARM.joints()
                pose_cur = ARM.end_pose()
                arm_st = ARM.arm_status()
            except Exception as e:
                return self._send(500, json.dumps({"error": str(e)}))
            with state_lock:
                return self._send(200, json.dumps({
                    "current": cur, "targets": STATE["targets"],
                    "mode": STATE["mode"], "pose": STATE["pose"], "pose_current": pose_cur,
                    "arm_status": arm_st,
                    "gripper_mm": STATE["gripper_mm"], "enabled": STATE["enabled"],
                    "estopped": STATE["estopped"], "speed": STATE["speed"],
                    "limits": JOINT_LIMITS, "gripper_range": GRIPPER_MM,
                    "speed_range": [SPEED_MIN, SPEED_MAX],
                    "log": STATE["log"][:14],
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

        if self.path == "/api/joint":
            j, deg = int(data.get("joint", 0)), float(data.get("deg", 0.0))
            if j not in JOINT_LIMITS:
                return self._send(400, json.dumps({"error": "bad joint"}))
            # A joint jog implies JOINT mode — auto-switch so it isn't silently ignored
            # while the arm is in Cartesian mode (the loop only streams JointCtrl in joint mode).
            if STATE["mode"] != "joint":
                ARM.enter_joint()
            v = clamp(j, deg)
            with state_lock:
                STATE["targets"][j - 1] = v
            log_call("JointCtrl", {f"joint_{j}": round(v, 2), "unit": "0.001 deg",
                                   "can_id": "0x155-0x157", "note": "streamed @20Hz"})
            return self._send(200, json.dumps({"ok": True, "deg": v}))

        if self.path == "/api/gripper":
            ARM.set_gripper(float(data.get("mm", 0.0)))
            return self._send(200, json.dumps({"ok": True}))

        if self.path == "/api/enable":
            ARM.enable() if data.get("on") else ARM.disable()
            return self._send(200, json.dumps({"ok": True}))

        if self.path == "/api/speed":
            # move_spd_rate_ctrl is a 0-100 percentage of the arm's rated speed
            # (CAN 0x151). Clamped to SPEED_MIN so a stray 0 can't stall a move
            # mid-travel and read as a blocked joint.
            v = int(max(SPEED_MIN, min(SPEED_MAX, int(data.get("speed", 15)))))
            with state_lock:
                STATE["speed"] = v
            ARM.set_speed(v)
            return self._send(200, json.dumps({"ok": True, "speed": v}))

        if self.path == "/api/estop":
            ARM.estop()
            return self._send(200, json.dumps({"ok": True}))

        if self.path == "/api/recover":
            ARM.recover()
            return self._send(200, json.dumps({"ok": True}))

        if self.path == "/api/mode":
            m = "cartesian" if data.get("mode") == "cartesian" else "joint"
            (ARM.enter_cartesian if m == "cartesian" else ARM.enter_joint)()
            return self._send(200, json.dumps({"ok": True, "mode": m}))

        if self.path == "/api/jog":
            # Held-key Cartesian jog. {axis:0-5, dir:+1|-1, on:true|false}
            # on=true  -> lead the measured pose by +/-lead on that axis (see _loop)
            # on=false -> release, but only if this key owns the current direction
            #             (so releasing one key can't cancel the opposite key's hold).
            axis = int(data.get("axis", -1))
            direction = 1.0 if float(data.get("dir", 0)) >= 0 else -1.0
            on = bool(data.get("on", False))
            if not 0 <= axis <= 5:
                return self._send(400, json.dumps({"error": "bad axis"}))
            if STATE["mode"] != "cartesian":
                ARM.enter_cartesian()
            lead = JOG_LEAD_MM if axis < 3 else JOG_LEAD_DEG
            with state_lock:
                if on:
                    STATE["jog"][axis] = direction * lead
                elif STATE["jog"][axis] * direction > 0:
                    STATE["jog"][axis] = 0.0
                jog = list(STATE["jog"])
            return self._send(200, json.dumps({"ok": True, "jog": jog}))

        if self.path == "/api/jointjog":
            # Held-key WRIST jog. {joint:0-5, dir:+1|-1, on:true|false}. Moves that
            # joint's target directly in MOVE J; J1-3 stay put unless jogged.
            jn = int(data.get("joint", -1))
            direction = 1.0 if float(data.get("dir", 0)) >= 0 else -1.0
            on = bool(data.get("on", False))
            if not 0 <= jn <= 5:
                return self._send(400, json.dumps({"error": "bad joint"}))
            # Wrist jog implies JOINT mode — auto-switch (seeds targets from live joints,
            # so J1-3 hold their current pose while the wrist articulates).
            if STATE["mode"] != "joint":
                ARM.enter_joint()
            with state_lock:
                if on:
                    STATE["jjog"][jn] = direction * JOINT_JOG_STEP
                elif STATE["jjog"][jn] * direction > 0:
                    STATE["jjog"][jn] = 0.0
                jjog = list(STATE["jjog"])
            return self._send(200, json.dumps({"ok": True, "jjog": jjog}))

        if self.path == "/api/jogstop":
            # Safety: clear every held key (Cartesian AND wrist) — blur / tab-hide / focus loss.
            with state_lock:
                STATE["jog"] = [0.0] * 6
                STATE["jjog"] = [0.0] * 6
            return self._send(200, json.dumps({"ok": True}))

        if self.path == "/api/home":
            # HOME must run in JOINT mode: in Cartesian mode the loop streams
            # EndPoseCtrl and ignores joint targets, so "home" silently did nothing.
            # enter_joint() switches to MOVE J (and seeds targets), then we zero them.
            if STATE["mode"] != "joint":
                ARM.enter_joint()
            with state_lock:
                STATE["targets"] = [0.0] * 6
                STATE["jjog"] = [0.0] * 6
            log_call("JointCtrl", {"all": 0.0, "note": "home to zero (forced MOVE J)"})
            return self._send(200, json.dumps({"ok": True}))

        return self._send(404, json.dumps({"error": "not found"}))


HTML = r"""<!doctype html>
<html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>PiPER Control · Nervus OS</title>
<style>
:root{
  --primary:#3B82F6; --secondary:#8B5CF6; --success:#16A34A;
  --warning:#D97706; --danger:#DC2626; --surface:#FFFFFF; --text:#111827;
  --muted:#6B7280; --line:#D1D5DB; --bg:#F3F4F6;
  --s1:4px; --s2:8px; --s3:12px; --s4:16px; --s6:24px; --s8:32px;
  --mono:"IBM Plex Mono",ui-monospace,SFMono-Regular,Menlo,monospace;
  --disp:"Space Grotesk","Open Sans",system-ui,sans-serif;
  --body:"Open Sans",system-ui,-apple-system,sans-serif;
}
@media (prefers-color-scheme:dark){:root{
  --surface:#111827; --text:#F9FAFB; --bg:#0B0F19; --line:#374151; --muted:#9CA3AF;}}
*{box-sizing:border-box}
body{margin:0;font-family:var(--body);font-size:16px;color:var(--text);
  background:var(--bg);
  background-image:radial-gradient(var(--line) 1px,transparent 1px);
  background-size:4px 4px;}
.wrap{max-width:940px;margin:0 auto;padding:var(--s6) var(--s4)}
header{display:flex;align-items:baseline;gap:var(--s3);flex-wrap:wrap;
  margin-bottom:var(--s6)}
h1{font-family:var(--disp);font-size:32px;margin:0;letter-spacing:-.02em}
.sub{color:var(--muted);font-size:14px;font-family:var(--mono)}
.panel{background:var(--surface);border:2px solid var(--text);
  padding:var(--s4);margin-bottom:var(--s4);
  box-shadow:4px 4px 0 var(--text)}
.bar{display:flex;gap:var(--s2);flex-wrap:wrap;align-items:center;
  margin-bottom:var(--s4)}
button{font-family:var(--disp);font-size:16px;font-weight:600;padding:var(--s2) var(--s4);
  border:2px solid var(--text);background:var(--surface);color:var(--text);
  cursor:pointer;box-shadow:2px 2px 0 var(--text)}
button:hover{transform:translate(1px,1px);box-shadow:1px 1px 0 var(--text)}
button:focus-visible{outline:3px solid var(--primary);outline-offset:2px}
button.on{background:var(--success);color:#fff}
button.off{background:var(--muted);color:#fff}
button.stop{background:var(--danger);color:#fff;font-size:18px}
button.sm{font-size:14px;padding:var(--s1) var(--s2);font-family:var(--mono)}
.joint{display:grid;grid-template-columns:64px 1fr auto;gap:var(--s3);
  align-items:center;padding:var(--s3) 0;border-bottom:1px dashed var(--line)}
.jname{font-family:var(--disp);font-weight:700;font-size:18px}
.jrange{font-family:var(--mono);font-size:14px;color:var(--muted)}
input[type=range]{width:100%;accent-color:var(--primary);height:28px}
input[type=range]:focus-visible{outline:3px solid var(--primary);outline-offset:2px}
input[type=number]{font-family:var(--mono);font-size:14px;width:88px;
  padding:var(--s1) var(--s2);border:2px solid var(--text);
  background:var(--surface);color:var(--text)}
.ctl{display:flex;gap:var(--s1);align-items:center;flex-wrap:wrap}
.val{font-family:var(--mono);font-size:14px;min-width:120px;text-align:right}
.cur{color:var(--secondary);font-weight:700}
#log{font-family:var(--mono);font-size:14px;max-height:240px;overflow:auto;
  background:var(--bg);border:2px solid var(--line);padding:var(--s3)}
#log div{padding:2px 0;border-bottom:1px dotted var(--line);word-break:break-all}
.fn{color:var(--primary);font-weight:700}
.badge{font-family:var(--mono);font-size:14px;padding:2px var(--s2);
  border:2px solid var(--text)}
.badge.live{background:var(--success);color:#fff}
.badge.halt{background:var(--danger);color:#fff}
.empty{color:var(--muted);font-style:italic}
h2{font-family:var(--disp);font-size:18px;margin:0 0 var(--s3)}
/* ---- virtual keyboard (cartesian jog) ---- */
.kbd{display:flex;gap:var(--s8);flex-wrap:wrap;margin:var(--s4) 0 0}
.kcluster{display:flex;flex-direction:column;gap:var(--s2);align-items:center}
.kcname{font-family:var(--disp);font-weight:700;font-size:12px;color:var(--muted);
  letter-spacing:.12em;text-transform:uppercase}
.krow{display:flex;gap:var(--s2);justify-content:center}
.kcap{width:76px;height:66px;border:2px solid var(--text);box-shadow:3px 3px 0 var(--text);
  background:var(--surface);color:var(--text);display:flex;flex-direction:column;
  align-items:center;justify-content:center;border-radius:9px;cursor:pointer;
  user-select:none;-webkit-user-select:none;transition:transform .05s,background .05s,box-shadow .05s}
.kcap.blank{border:none;box-shadow:none;background:transparent;cursor:default}
.kcap .glyph{font-family:var(--disp);font-weight:800;font-size:22px;line-height:1}
.kcap .fn{font-family:var(--mono);font-size:11px;color:var(--muted);margin-top:4px;font-weight:600}
.kcap .ax{font-family:var(--mono);font-size:9px;color:var(--muted);opacity:.6;margin-top:1px}
.kcap.down{background:var(--primary);color:#fff;transform:translate(3px,3px);box-shadow:0 0 0 var(--text)}
.kcap.down .fn,.kcap.down .ax{color:#fff;opacity:1}
.kcap.rot.down{background:var(--secondary)}
.kcap.wrist.down{background:var(--warning)}
</style></head><body>
<div class="wrap">
<header>
  <h1>PiPER Control</h1>
  <span class="sub">Nervus OS · can0 · degrees authoritative</span>
  <span id="status" class="badge halt">DISABLED</span>
</header>

<div class="panel">
  <div class="bar">
    <button id="btnEnable" class="off">ENABLE MOTORS</button>
    <button id="btnHome">HOME → 0</button>
    <button id="btnEstop" class="stop">■ E-STOP</button>
    <button id="btnRecover" class="warn">↻ RECOVER</button>
    <button id="btnMode" class="ghost">MODE · JOINT</button>
    <span class="sub" id="hb">heartbeat —</span>
  </div>
  <div class="sub">Motors start disabled. Streaming stops if this tab closes (deadman).</div>
</div>

<div class="panel" id="cartHelp" style="display:none">
  <h2>Keyboard control — hold a key to move, release to stop</h2>
  <div class="sub"><b>Translate</b> (blue): arrows = X/Y, W/S = Z. &nbsp; <b>Gimbal</b> (violet, tip fixed): U/O roll · I/K pitch · J/L yaw. &nbsp; <b>Wrist joints</b> (amber, base fixed — tip arcs): A/D = J4 · Q/E = J5 · Z/C = J6. Mode switches automatically. Click the page once for keyboard focus; keep the workspace clear.</div>
  <div class="kbd" id="kbd"></div>
  <div class="sub" id="posenow" style="margin-top:var(--s4)">pose —</div>
</div>

<div class="panel">
  <h2>Max speed</h2>
  <div class="sub">Percentage of the arm's rated speed. Applies to every joint move.</div>
  <div class="row">
    <input type="range" id="ss" min="1" max="100" step="1" value="15"
      aria-label="Maximum speed percent">
    <input type="number" id="ns" min="1" max="100" step="1" value="15"
      aria-label="Maximum speed percent">
    <span class="val" id="vs"><span class="cur">15%</span></span>
  </div>
  <div class="row">
    <button id="sp5"  class="ghost">Slow · 5%</button>
    <button id="sp15" class="ghost">Safe · 15%</button>
    <button id="sp40" class="ghost">Brisk · 40%</button>
    <button id="sp100" class="warn">Max · 100%</button>
  </div>
</div>

<div class="panel">
  <h2>Joints</h2>
  <div id="joints"><div class="empty">Loading joint state…</div></div>
</div>

<div class="panel">
  <h2>Gripper</h2>
  <div id="grip"></div>
</div>

<div class="panel">
  <h2>CAN calls · function &amp; parameters</h2>
  <div id="log"><div class="empty">No calls yet.</div></div>
</div>
</div>

<script>
const J=[1,2,3,4,5,6]; let LIM={}, GR=[0,70], built=false, enabled=false;
const $=id=>document.getElementById(id);
const pct=(v,lo,hi)=>((v-lo)/(hi-lo))*100;
const deg=(p,lo,hi)=>lo+(p/100)*(hi-lo);

async function post(u,b){try{await fetch(u,{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(b||{})});}catch(e){}}

function build(){
  $('joints').innerHTML='';
  J.forEach(j=>{
    const [lo,hi]=LIM[j];
    const row=document.createElement('div'); row.className='joint';
    row.innerHTML=`<div><div class="jname">J${j}</div><div class="jrange">${lo}…${hi}°</div></div>
      <div><input type="range" id="s${j}" min="${lo}" max="${hi}" step="0.5" value="0"
        aria-label="Joint ${j} angle in degrees"></div>
      <div class="ctl">
        <button class="sm" data-j="${j}" data-d="-20">−20%</button>
        <button class="sm" data-j="${j}" data-d="-5">−5%</button>
        <button class="sm" data-j="${j}" data-d="5">+5%</button>
        <button class="sm" data-j="${j}" data-d="20">+20%</button>
        <input type="number" id="n${j}" step="0.5" min="${lo}" max="${hi}"
          aria-label="Joint ${j} target degrees">
        <span class="val" id="v${j}">—</span>
      </div>`;
    $('joints').appendChild(row);
  });
  $('grip').innerHTML=`<div class="joint">
      <div><div class="jname">GRIP</div><div class="jrange">${GR[0]}…${GR[1]}mm</div></div>
      <div><input type="range" id="sg" min="${GR[0]}" max="${GR[1]}" step="1" value="0"
        aria-label="Gripper opening in millimetres"></div>
      <div class="ctl">
        <button class="sm" id="gopen">OPEN</button><button class="sm" id="gclose">CLOSE</button>
        <input type="number" id="ng" step="1" min="${GR[0]}" max="${GR[1]}"
          aria-label="Gripper millimetres">
        <span class="val" id="vg">—</span>
      </div></div>`;

  J.forEach(j=>{
    const [lo,hi]=LIM[j];
    $('s'+j).addEventListener('input',e=>send(j,parseFloat(e.target.value)));
    $('n'+j).addEventListener('change',e=>send(j,parseFloat(e.target.value)));
  });
  document.querySelectorAll('button[data-j]').forEach(b=>{
    b.addEventListener('click',()=>{
      const j=+b.dataset.j,[lo,hi]=LIM[j];
      const cur=parseFloat($('s'+j).value);
      send(j, Math.max(lo,Math.min(hi, deg(pct(cur,lo,hi)+(+b.dataset.d),lo,hi))));
    });
  });
  function setSpeed(v){
    v=Math.max(1,Math.min(100,Math.round(v)));
    $('ss').value=v; $('ns').value=v;
    $('vs').innerHTML=`<span class="cur">${v}%</span>`;
    post('/api/speed',{speed:v});
  }
  $('ss').addEventListener('input',e=>setSpeed(+e.target.value));
  $('ns').addEventListener('change',e=>setSpeed(+e.target.value));
  $('sp5').onclick=()=>setSpeed(5);
  $('sp15').onclick=()=>setSpeed(15);
  $('sp40').onclick=()=>setSpeed(40);
  $('sp100').onclick=()=>{ if(confirm('Set MAXIMUM speed (100%)?\nThe arm will move at its full rated rate. Keep the e-stop in reach.')) setSpeed(100); };

  $('sg').addEventListener('input',e=>post('/api/gripper',{mm:+e.target.value}));
  $('ng').addEventListener('change',e=>post('/api/gripper',{mm:+e.target.value}));
  $('gopen').onclick=()=>{$('sg').value=GR[1];post('/api/gripper',{mm:GR[1]});};
  $('gclose').onclick=()=>{$('sg').value=GR[0];post('/api/gripper',{mm:GR[0]});};
  buildKeyboard();
  built=true;
}

function send(j,d){const [lo,hi]=LIM[j];d=Math.max(lo,Math.min(hi,d));
  $('s'+j).value=d;$('n'+j).value=d.toFixed(1);post('/api/joint',{joint:j,deg:d});}

$('btnEnable').onclick=()=>{
  if(enabled && !confirm('DISABLE cuts torque to all joints — the arm will DROP under gravity.\n\nSupport it or lower it to a rest position first. Continue?'))return;
  post('/api/enable',{on:!enabled});
};
$('btnHome').onclick=()=>{post('/api/home');J.forEach(j=>{$('s'+j).value=0;$('n'+j).value=0;});};
$('btnEstop').onclick=()=>{if(confirm('Send EMERGENCY STOP?'))post('/api/estop');};
$('btnRecover').onclick=()=>{if(confirm('Recover from collision?\n\nThe arm briefly goes limp while its fault clears, then re-enables — no power-cycle needed. Keep the workspace clear.'))post('/api/recover');};

let mode='joint';
function updateModeUI(){
  // Mode now auto-switches server-side per key (Cartesian for translate/gimbal,
  // Joint for wrist). The keyboard drives both, so it stays visible; the badge
  // just reflects which mode the arm is currently armed in.
  $('btnMode').textContent='ARM MODE · '+(mode==='joint'?'JOINT':'CARTESIAN');
  $('btnMode').className=mode==='cartesian'?'on':'ghost';
  $('cartHelp').style.display='block';
}
$('btnMode').onclick=()=>{mode=(mode==='joint')?'cartesian':'joint';post('/api/mode',{mode});updateModeUI();};

// Key map. kind 'cart' -> Cartesian leashed jog (/api/jog): axes 0-2 translate
// (X/Y/Z), 3-5 rotate (RX/RY/RZ, the tip-fixed gimbal). kind 'joint' -> wrist jog
// (/api/jointjog): moves that joint directly, J1-3 stay put. rot/wrist tag colours.
const KEYMAP={
  // translation — arrows do X/Y, W/S do Z (no longer duplicated)
  arrowup:   {kind:'cart',axis:0,dir:+1,glyph:'↑',fn:'forward',ax:'X+'},
  arrowdown: {kind:'cart',axis:0,dir:-1,glyph:'↓',fn:'back',ax:'X−'},
  arrowleft: {kind:'cart',axis:1,dir:+1,glyph:'←',fn:'left',ax:'Y+'},
  arrowright:{kind:'cart',axis:1,dir:-1,glyph:'→',fn:'right',ax:'Y−'},
  w:{kind:'cart',axis:2,dir:+1,glyph:'W',fn:'up',ax:'Z+'},
  s:{kind:'cart',axis:2,dir:-1,glyph:'S',fn:'down',ax:'Z−'},
  // gimbal — rotate the tool about a FIXED tip (Cartesian orientation)
  u:{kind:'cart',axis:3,dir:+1,glyph:'U',fn:'roll ↺',ax:'RX+',rot:1},
  o:{kind:'cart',axis:3,dir:-1,glyph:'O',fn:'roll ↻',ax:'RX−',rot:1},
  i:{kind:'cart',axis:4,dir:+1,glyph:'I',fn:'pitch ↑',ax:'RY+',rot:1},
  k:{kind:'cart',axis:4,dir:-1,glyph:'K',fn:'pitch ↓',ax:'RY−',rot:1},
  j:{kind:'cart',axis:5,dir:+1,glyph:'J',fn:'yaw ←',ax:'RZ+',rot:1},
  l:{kind:'cart',axis:5,dir:-1,glyph:'L',fn:'yaw →',ax:'RZ−',rot:1},
  // wrist joints — J1-3 fixed, tip NOT fixed (moves in an arc)
  a:{kind:'joint',joint:3,dir:+1,glyph:'A',fn:'J4 +',ax:'joint4',wrist:1},
  d:{kind:'joint',joint:3,dir:-1,glyph:'D',fn:'J4 −',ax:'joint4',wrist:1},
  q:{kind:'joint',joint:4,dir:+1,glyph:'Q',fn:'J5 +',ax:'joint5',wrist:1},
  e:{kind:'joint',joint:4,dir:-1,glyph:'E',fn:'J5 −',ax:'joint5',wrist:1},
  z:{kind:'joint',joint:5,dir:+1,glyph:'Z',fn:'J6 +',ax:'joint6',wrist:1},
  c:{kind:'joint',joint:5,dir:-1,glyph:'C',fn:'J6 −',ax:'joint6',wrist:1},
};
const held=new Set();
const capId=k=>'kc_'+k.replace('arrow','arr_');
function kcapHTML(k){
  if(k==='') return '<div class="kcap blank"></div>';
  const m=KEYMAP[k];
  return `<div class="kcap${m.rot?' rot':''}${m.wrist?' wrist':''}" id="${capId(k)}" data-k="${k}">`+
    `<div class="glyph">${m.glyph}</div><div class="fn">${m.fn}</div><div class="ax">${m.ax}</div></div>`;
}
function buildKeyboard(){
  const transXY=[['','arrowup',''],['arrowleft','arrowdown','arrowright']];
  const transZ=[['w'],['s']];
  const gimbal=[['u','i','o'],['j','k','l']];
  const wrist=[['a','q','z'],['d','e','c']];
  const cluster=(name,rows)=>`<div class="kcluster"><div class="kcname">${name}</div>`+
    rows.map(r=>`<div class="krow">${r.map(kcapHTML).join('')}</div>`).join('')+`</div>`;
  $('kbd').innerHTML=cluster('Translate · X/Y',transXY)+cluster('· Z',transZ)+
    cluster('Gimbal · tip fixed',gimbal)+cluster('Wrist joints · base fixed',wrist);
  $('kbd').querySelectorAll('.kcap[data-k]').forEach(el=>{
    const k=el.dataset.k;
    const dn=e=>{e.preventDefault();startJog(k);};
    const up=e=>{e.preventDefault();stopJog(k);};
    el.addEventListener('mousedown',dn); el.addEventListener('mouseup',up);
    el.addEventListener('mouseleave',up);
    el.addEventListener('touchstart',dn,{passive:false});
    el.addEventListener('touchend',up,{passive:false});
  });
}
function setDown(k,on){const el=$(capId(k));if(el)el.classList.toggle('down',on);}
function startJog(k){
  if(!enabled||held.has(k))return;
  const m=KEYMAP[k];held.add(k);setDown(k,true);
  if(m.kind==='joint') post('/api/jointjog',{joint:m.joint,dir:m.dir,on:true});
  else post('/api/jog',{axis:m.axis,dir:m.dir,on:true});
}
function stopJog(k){
  if(!held.has(k))return;
  const m=KEYMAP[k];held.delete(k);setDown(k,false);
  if(m.kind==='joint') post('/api/jointjog',{joint:m.joint,dir:m.dir,on:false});
  else post('/api/jog',{axis:m.axis,dir:m.dir,on:false});
}
function releaseAll(){
  if(!held.size)return;
  held.forEach(k=>setDown(k,false));held.clear();
  post('/api/jogstop');
}
document.addEventListener('keydown',ev=>{
  const k=ev.key.toLowerCase();
  if(!(k in KEYMAP))return;
  if(ev.target && /^(INPUT|TEXTAREA)$/.test(ev.target.tagName))return;
  if(!enabled)return;   // keys auto-switch mode server-side (cart vs wrist joint)
  ev.preventDefault(); startJog(k);
});
document.addEventListener('keyup',ev=>{
  const k=ev.key.toLowerCase();
  if(!(k in KEYMAP))return;
  if(held.has(k)){ev.preventDefault();stopJog(k);}   // always release a held key, even from an input
});
// safety: never leave a key stuck if the window loses focus or is hidden
window.addEventListener('blur',releaseAll);
document.addEventListener('visibilitychange',()=>{if(document.hidden)releaseAll();});

setInterval(()=>post('/api/heartbeat'),400);

async function poll(){
  try{
    const r=await fetch('/api/state'); const s=await r.json();
    if(s.error){$('hb').textContent='error: '+s.error;return;}
    LIM=s.limits; GR=s.gripper_range;
    if(!built) build();
    enabled=s.enabled;
    if(!enabled) releaseAll();   // disabled/e-stopped -> drop any held jog keys
    if(s.mode && s.mode!==mode){mode=s.mode;updateModeUI();}
    if(s.pose_current){const p=s.pose_current;
      let st=s.arm_status||'';
      let warn=/EXCEEDS_LIMIT|COLLISION|ERR|ABNORMAL/i.test(st)?'  ⚠ '+st+' — jog to a normal pose in JOINT mode first':(st?'  ['+st+']':'');
      $('posenow').textContent='TCP  X '+p[0].toFixed(0)+'  Y '+p[1].toFixed(0)+'  Z '+p[2].toFixed(0)+' mm'+warn;}
    const st=$('status');
    st.textContent=s.estopped?'E-STOPPED':(s.enabled?'ENABLED':'DISABLED');
    st.className='badge '+(s.enabled?'live':'halt');
    $('btnEnable').textContent=s.enabled?'DISABLE MOTORS':'ENABLE MOTORS';
    $('btnEnable').className=s.enabled?'on':'off';
    $('hb').textContent='streaming @20Hz'+(s.enabled?'':' (idle)');
    J.forEach(j=>{
      const c=s.current[j-1],t=s.targets[j-1];
      $('v'+j).innerHTML=`<span class="cur">${c.toFixed(1)}°</span> → ${t.toFixed(1)}°`;
      if(document.activeElement!==$('n'+j))$('n'+j).value=t.toFixed(1);
    });
    if(document.activeElement!==$('ss')&&document.activeElement!==$('ns')){
      $('ss').value=s.speed; $('ns').value=s.speed;
      $('vs').innerHTML=`<span class="cur">${s.speed}%</span>`;
    }
    $('vg').innerHTML=`<span class="cur">${s.gripper_mm.toFixed(0)}mm</span>`;
    $('log').innerHTML = s.log.length ? s.log.map(e=>
      `<div><span class="sub">${e.t}</span> <span class="fn">${e.fn}</span>(${
        Object.entries(e.params).map(([k,v])=>`${k}=${v}`).join(', ')})</div>`).join('')
      : '<div class="empty">No calls yet.</div>';
  }catch(e){$('hb').textContent='disconnected — arm will stop';}
}
setInterval(poll,300); poll();
</script></body></html>
"""


def main():
    global ARM
    ap = argparse.ArgumentParser(description="PiPER web control panel")
    ap.add_argument("--port", type=int, default=8770)
    ap.add_argument("--iface", default="can0")
    ap.add_argument("--speed", type=int, default=15)
    ap.add_argument("--bind", default="127.0.0.1",
                    help="address to listen on (default loopback). 'auto' = the direct-link "
                         "IP if that Ethernet cable is up, else loopback.")
    args = ap.parse_args()

    if args.bind == "auto":
        args.bind = pick_bind_address()
    with state_lock:
        STATE["speed"] = args.speed
    ARM = Arm(args.iface, args.speed)
    srv = ThreadingHTTPServer((args.bind, args.port), Handler)
    print(f"\nPiPER web control listening on http://{args.bind}:{args.port}")
    if args.bind == DIRECT_LINK_IP:
        print(f"  Direct Ethernet link is UP.")
        print(f"  Open from the linked laptop: http://{DIRECT_LINK_IP}:{args.port}")
        print(f"  Open on the board:  http://localhost:{args.port}")
    else:
        print(f"  Bound to {args.bind}.")
        print(f"  On the board's own screen:  http://localhost:{args.port}")
        print(f"  Or tunnel from another machine: "
              f"ssh -L {args.port}:localhost:{args.port} <this-host>")
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
