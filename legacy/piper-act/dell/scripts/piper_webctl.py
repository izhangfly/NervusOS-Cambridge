#!/usr/bin/env python3
"""
PiPER web control panel — per-joint sliders, served from the RDK.

Runs on the board (where can0 lives) and is reached from the Mac over an SSH
local port-forward, so robot control is NEVER exposed to the public internet:

    ssh -L 8770:localhost:8770 rdk-persist
    open http://localhost:8770

Safety design:
  * Motors start DISABLED. Nothing moves until you press ENABLE.
  * The browser sends a heartbeat; if it stops for >1.5 s the control loop stops
    streaming JointCtrl and the arm halts. Closing the tab stops the arm.
  * E-STOP button sends EmergencyStop (CAN 0x150).
  * Targets are clamped to the manual's joint limits, server-side.

Units: degrees are authoritative (the arm speaks 0.001 deg). Percent is only a
convenience mapping over each joint's asymmetric range.

    python3 piper_webctl.py [--port 8770] [--iface can0] [--speed 15]
"""

import argparse
import json
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

try:
    from piper_sdk import C_PiperInterface_V2
except ImportError:
    raise SystemExit("piper_sdk not importable — use ~/NervusOS/robots/.venv/bin/python")

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

state_lock = threading.Lock()
STATE = {
    "targets": [0.0] * 6,
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
            STATE["targets"] = self.joints()

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
                tgt = list(STATE["targets"])
            if enabled and fresh:
                self.piper.JointCtrl(*[int(round(v * MDEG)) for v in tgt])
                streaming = True
            elif streaming:
                streaming = False
                if enabled and not fresh:
                    log_call("(deadman)", {"reason": "heartbeat lost", "action": "stopped streaming"})


ARM = None

# The direct Mac<->RDK Ethernet link (U45 / eth1). Binding here exposes the panel
# ONLY on that point-to-point cable — not on wlan0, and never on the internet.
DIRECT_LINK_IP = "192.168.127.10"


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
            except Exception as e:
                return self._send(500, json.dumps({"error": str(e)}))
            with state_lock:
                return self._send(200, json.dumps({
                    "current": cur, "targets": STATE["targets"],
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

        if self.path == "/api/home":
            with state_lock:
                STATE["targets"] = [0.0] * 6
            log_call("JointCtrl", {"all": 0.0, "note": "home to zero"})
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
    <span class="sub" id="hb">heartbeat —</span>
  </div>
  <div class="sub">Motors start disabled. Streaming stops if this tab closes (deadman).</div>
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
  built=true;
}

function send(j,d){const [lo,hi]=LIM[j];d=Math.max(lo,Math.min(hi,d));
  $('s'+j).value=d;$('n'+j).value=d.toFixed(1);post('/api/joint',{joint:j,deg:d});}

$('btnEnable').onclick=()=>post('/api/enable',{on:!enabled});
$('btnHome').onclick=()=>{post('/api/home');J.forEach(j=>{$('s'+j).value=0;$('n'+j).value=0;});};
$('btnEstop').onclick=()=>{if(confirm('Send EMERGENCY STOP?'))post('/api/estop');};

setInterval(()=>post('/api/heartbeat'),400);

async function poll(){
  try{
    const r=await fetch('/api/state'); const s=await r.json();
    if(s.error){$('hb').textContent='error: '+s.error;return;}
    LIM=s.limits; GR=s.gripper_range;
    if(!built) build();
    enabled=s.enabled;
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
    ap.add_argument("--bind", default="auto",
                    help="auto = the direct-link IP if the Ethernet cable is up, "
                         "else loopback. Explicit values also accepted.")
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
        print(f"  Open on your Mac:   http://{DIRECT_LINK_IP}:{args.port}")
        print(f"  Open on the board:  http://localhost:{args.port}")
    else:
        print(f"  Direct link not up — bound to loopback.")
        print(f"  On the board's own screen:  http://localhost:{args.port}")
        print(f"  Or tunnel from the Mac:     "
              f"ssh -L {args.port}:localhost:{args.port} rdk-persist")
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
