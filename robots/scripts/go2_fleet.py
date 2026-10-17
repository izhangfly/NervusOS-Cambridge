#!/usr/bin/env python3
"""
Launch one DimOS instance per Go2, each in its own isolated process group.

WHY THIS EXISTS
---------------
DimOS tags every descendant of a `dimos run` with an env var (default
DIMOS_RUN_ID) and sweeps by it. From
dimos/core/coordination/process_lifecycle.py:

    "Every descendant of a `dimos run` invocation inherits a tag env var
     (default `DIMOS_RUN_ID`) in its environment. kill_run_processes scans
     the system via psutil and terminates any process whose environment
     matches a given run id."

plus `spawn_watchdog`, a sidecar that runs that sweep when the main PID dies.

So if you start two dogs without distinct run ids, stopping (or crashing) one
reaps the other's workers. This launcher gives every robot its own run id
derived from its SERIAL, so a process always belongs to exactly one dog and an
app can target that dog deterministically instead of "whatever answered last".

Serial is the stable identity. IP is not: the Air was recorded at 10.72.13.201
in the handoff and came back as 10.72.10.40 on the next DHCP lease.

USAGE
    # see who is on the network, with role assignment
    ./go2_fleet.py --discover

    # leader/follower as two isolated instances (recommended)
    ./go2_fleet.py --leader B42D2000PROXXXXX --follower B42D1000AIRXXXXX

    # single coordinated fleet process (DimOS coordinates internally)
    ./go2_fleet.py --leader <SN> --follower <SN> --mode fleet

    # record the leader's path so the follower can replay it
    ./go2_fleet.py --leader <SN> --follower <SN> --record-path

    ./go2_fleet.py --stop            # sweep every dog instance
"""

import argparse
import json
import os
import re
import shutil
import subprocess
import sys
import time
from pathlib import Path

DIMOS_DIR = Path.home() / "NervusOS/robots/vendor/dimos"
DIMOS_BIN = DIMOS_DIR / ".venv/bin/dimos"
STATE_DIR = Path.home() / ".cache/go2_fleet"
AES_CACHE = STATE_DIR / "aes_keys.json"   # from go2_fetch_aes_keys.py
DISCOVER_SECS = 20

# Known robots, from RDK_HANDOFF.md. Serial -> friendly name.
KNOWN = {
    "B42D1000AIRXXXXX": "air",       # lidar works
    "B42D2000PROXXXXX": "pro",       # lidar broken
    "B42D2000NEWXXXXX": "go2_new",
}

# Blueprints, verified present via `dimos list` on 2026-07-24.
BP_SINGLE = "unitree-go2"
BP_FLEET = "unitree-go2-fleet"
BP_NAV3D = "unitree-go2-nav-3d"      # honours DIMOS_NAV_RECORD=1


def load_aes_cache() -> dict:
    if not AES_CACHE.exists():
        return {}
    try:
        return json.loads(AES_CACHE.read_text())
    except Exception:
        return {}


def aes_key_for(token: str, claimed: set | None = None):
    """Return (key, serial_it_belongs_to) for a robot addressed by serial OR IP.

    Newer-firmware dogs report no serial over discovery, so they must be
    addressed by IP — but keys are issued per SERIAL. Looking up an IP in a
    serial-keyed cache silently yields no key, and the robot then dies with
    AesKeyRequiredError. So: exact serial hit first; otherwise, if this is an
    IP and exactly one cached key is still unclaimed, that key must be this
    robot's. Ambiguity is reported rather than guessed.
    """
    cache = load_aes_cache()
    if not cache:
        return None, None
    if token in cache:
        return cache[token]["key"], token
    if is_ip(token):
        free = [sn for sn in cache if sn not in (claimed or set())]
        if len(free) == 1:
            return cache[free[0]]["key"], free[0]
        if len(free) > 1:
            print(f"  ! {token}: {len(free)} unclaimed keys ({', '.join(free)}) — "
                  f"cannot infer which. Pass the serial instead of the IP.")
    return None, None


def run_id_for(serial: str, role: str) -> str:
    """Stable, unique per robot. This is the whole point of the launcher."""
    return f"go2-{role}-{serial}"


GO2_PORT = 9991           # Unitree signalling port; open on every Go2
IP_RE = re.compile(r"^\d{1,3}(?:\.\d{1,3}){3}$")


def is_ip(s: str) -> bool:
    return bool(IP_RE.match(s or ""))


def port_scan_go2(subnet_hint: str = "10.72") -> dict:
    """Find Go2s by SERVICE, not by serial or MAC OUI.

    `go2tool discover` reads identity over HTTP, so a dog on newer firmware —
    which serves only 9991 and closes 22/80/8080 — is invisible to it. MAC OUI
    is no better: the two dogs here use 94:ba:06 and 78:22:88 respectively.
    Port 9991 is the one thing every Go2 answers on.

    Returns {ip: {"ip": ip, "mac": mac, "serial": None}}.
    """
    found = {}
    try:
        out = subprocess.run(["ip", "neigh"], capture_output=True, text=True, timeout=20).stdout
    except Exception:
        return found
    hosts = []
    for line in out.splitlines():
        if "INCOMPLETE" in line.upper() or "FAILED" in line.upper():
            continue
        m = re.match(rf"^({re.escape(subnet_hint)}\.\d+\.\d+)\s+.*lladdr\s+(\S+)", line)
        if m:
            hosts.append(m.groups())
    for ip, mac in hosts:
        try:
            import socket
            s = socket.socket(); s.settimeout(1.5)
            if s.connect_ex((ip, GO2_PORT)) == 0:
                found[ip] = {"ip": ip, "mac": mac, "serial": None}
            s.close()
        except Exception:
            pass
    return found


def discover(timeout: int = DISCOVER_SECS) -> dict:
    """Return {serial: {ip, mac}} seen on the LAN."""
    if not DIMOS_BIN.exists():
        sys.exit(f"dimos not found at {DIMOS_BIN}")
    print(f"discovering for {timeout}s (LAN) ...")
    try:
        out = subprocess.run(
            [str(DIMOS_BIN), "go2tool", "discover", "--lan", "-t", str(timeout)],
            cwd=DIMOS_DIR, capture_output=True, text=True, timeout=timeout + 40,
        ).stdout
    except subprocess.TimeoutExpired:
        sys.exit("discovery timed out")

    found = {}
    for line in out.splitlines():
        # LAN    -    10.72.10.40    xx:xx:xx:xx:xx:xx    B42D1000AIRXXXXX
        m = re.search(r"(\d+\.\d+\.\d+\.\d+)\s+([0-9A-Fa-f:]{17})\s+(\S+)\s*$", line)
        if m:
            ip, mac, serial = m.groups()
            found[serial] = {"ip": ip, "mac": mac}
    return found


def show(found: dict, leader: str = None, follower: str = None) -> None:
    if not found:
        print("\nNo robots found. Power them on and confirm they joined the Wi-Fi.")
        return
    print(f"\n{'SERIAL':<20} {'NAME':<11} {'IP':<15} {'ROLE'}")
    for sn, d in found.items():
        role = "leader" if sn == leader else "follower" if sn == follower else "-"
        print(f"{sn:<20} {KNOWN.get(sn, '?'):<11} {d['ip']:<15} {role}")


LCM_BASE_PORT = 7667                 # DimOS default multicast port
LCM_MCAST = "239.255.76.67"
_ROLE_LCM_OFFSET = {"leader": 0, "follower": 1}


def lcm_url_for(role: str) -> str:
    """A distinct LCM multicast bus per role, so each dog gets its own
    Coordinator. Matches DimOS's own conftest.py isolation scheme."""
    off = _ROLE_LCM_OFFSET.get(role, abs(hash(role)) % 20 + 2)
    return f"udpm://{LCM_MCAST}:{LCM_BASE_PORT + off}?ttl=0"


def launch(serial: str, ip: str, role: str, blueprint: str, record: bool,
           claimed: set | None = None, viz: bool = True) -> subprocess.Popen:
    """Start one dog's DimOS instance in its own process group + run id.

    Only ONE instance may run the visualization: the rerun web server (3030)
    and websocket_vis (7779) bind FIXED localhost ports, so a second viewer
    dies with 'address already in use (errno 98)' and takes its instance down.
    Process isolation (DIMOS_RUN_ID) is necessary but not sufficient; the
    viewer must be single-owner too. Pydantic BaseSettings reads bare field
    names (no prefix), so RERUN_WEB / VIEWER are the right env knobs.
    """
    env = os.environ.copy()
    env["DIMOS_RUN_ID"] = run_id_for(serial, role)   # process-lifecycle isolation
    env["LCM_DEFAULT_URL"] = lcm_url_for(role)       # separate coordination bus
    # Fixed nav-viz TCP port also collides between instances; offset per role.
    env["DIMOS_WEBSOCKET_VIS_PORT"] = str(7779 + _ROLE_LCM_OFFSET.get(role, 2))
    env["ROBOT_IP"] = ip
    if not viz:
        env["VIEWER"] = "none"
        env["RERUN_WEB"] = "false"
    key, key_sn = aes_key_for(serial, claimed)
    if key:
        # Per-robot secret; scoped to this instance's env, never global.
        env["UNITREE_AES_128_KEY"] = key
    if record:
        env["DIMOS_NAV_RECORD"] = "1"

    STATE_DIR.mkdir(parents=True, exist_ok=True)
    log = STATE_DIR / f"{role}-{serial}.log"
    meta = STATE_DIR / f"{role}-{serial}.json"

    keynote = f"aes-key({key_sn})" if key else "no-key"
    lcm_port = env["LCM_DEFAULT_URL"].split(":")[-1].split("?")[0]
    print(f"  {role:<8} {serial}  ip={ip:<15} {keynote:<8} "
          f"run_id={env['DIMOS_RUN_ID']}  lcm_port={lcm_port}")
    proc = subprocess.Popen(
        [str(DIMOS_BIN), "run", blueprint],
        cwd=DIMOS_DIR, env=env,
        stdout=open(log, "w"), stderr=subprocess.STDOUT,
        start_new_session=True,          # own process group; Ctrl-C won't cross-kill
    )
    meta.write_text(json.dumps({
        "serial": serial, "ip": ip, "role": role, "pid": proc.pid,
        "run_id": env["DIMOS_RUN_ID"], "blueprint": blueprint,
        "record": record, "started": time.time(),
    }, indent=2))
    return proc


def stop_all() -> int:
    """Terminate every instance this launcher started, one run id at a time."""
    if not STATE_DIR.exists():
        print("nothing to stop")
        return 0
    n = 0
    for meta in sorted(STATE_DIR.glob("*.json")):
        d = json.loads(meta.read_text())
        print(f"  stopping {d['role']} {d['serial']} (run_id={d['run_id']}, pid={d['pid']})")
        try:
            os.killpg(os.getpgid(d["pid"]), 15)
        except (ProcessLookupError, PermissionError):
            pass
        # Sweep by run id too, in case workers outlived the group.
        subprocess.run(["pkill", "-f", d["run_id"]], capture_output=True)
        meta.unlink()
        n += 1
    # Safety net: sweep any stray `dimos run` a crashed launch may have orphaned
    # (a headless follower can keep running after the leader dies).
    stray = subprocess.run(["pkill", "-f", "dimos run"], capture_output=True)
    if stray.returncode == 0:
        print("  swept stray 'dimos run' process(es)")
    print(f"stopped {n} instance(s)")
    return 0


def main() -> int:
    ap = argparse.ArgumentParser(description="Per-robot isolated DimOS launcher for Go2 fleet")
    ap.add_argument("--discover", action="store_true", help="list robots and exit")
    ap.add_argument("--scan", action="store_true",
                    help="find dogs by port 9991 (catches newer firmware that reports no serial)")
    ap.add_argument("--leader", help="serial of the dog that walks in front")
    ap.add_argument("--follower", help="serial of the dog that follows")
    ap.add_argument("--solo", metavar="SERIAL_OR_IP",
                    help="launch ONE dog on bus 7668 with a viewer, and exit. "
                         "For single-dog record/replay when the other dog is down.")
    ap.add_argument("--mode", choices=["separate", "fleet"], default="separate",
                    help="separate = one isolated process per dog (default); "
                         "fleet = one coordinated process for both")
    ap.add_argument("--dual-viz", action="store_true",
                    help="both instances run a viewer (offset rerun ports). "
                         "Default: only the leader visualizes, to avoid the "
                         "fixed-port (3030/9877) collision.")
    ap.add_argument("--record-path", action="store_true",
                    help="set DIMOS_NAV_RECORD=1 on the leader to capture its trajectory")
    ap.add_argument("--stop", action="store_true", help="stop all launched instances")
    ap.add_argument("--timeout", type=int, default=DISCOVER_SECS)
    args = ap.parse_args()

    if args.stop:
        return stop_all()

    if args.solo:
        found = discover(8)
        ip = args.solo if is_ip(args.solo) else (found.get(args.solo, {}) or {}).get("ip")
        if not ip:
            print(f"could not resolve {args.solo} — pass an IP, or a serial discovery can see.")
            return 1
        serial = args.solo if not is_ip(args.solo) else next(
            (s for s, d in found.items() if d["ip"] == ip), args.solo)
        # Reuse the proven launch() path (start_new_session, per-instance env,
        # meta + log) — the single dog goes on the follower bus 7668 so the
        # follow bridge's record/replay commands work unchanged.
        proc = launch(serial, ip, "follower", BP_SINGLE, False, viz=True)
        print(f"\nsolo dog up (pid {proc.pid}). Log: {STATE_DIR}/follower-{serial}.log")
        print("Give it ~40s to connect, then:")
        print(f"  ./vendor/dimos/.venv/bin/python scripts/go2_follow_bridge.py --sniff --bus 7668")
        return 0

    if args.scan:
        print("scanning ARP neighbours for port 9991 ...")
        hits = port_scan_go2()
        if not hits:
            print("  none found — run a ping sweep first so ARP is populated")
            return 1
        print(f"\n{'IP':<16} {'MAC':<20} NOTE")
        for ip, d in hits.items():
            note = "serial via discovery" if any(v['ip'] == ip for v in discover(8).values()) \
                   else "no serial (newer firmware) - address by IP"
            print(f"{ip:<16} {d['mac']:<20} {note}")
        return 0

    found = discover(args.timeout)

    if args.discover or not (args.leader and args.follower):
        show(found, args.leader, args.follower)
        if not args.discover:
            print("\nPass --leader <SERIAL> --follower <SERIAL> to launch.")
            print("Known:", ", ".join(f"{v}={k}" for k, v in KNOWN.items()))
        return 0

    # Resolve each role to an IP. Accept a serial (looked up via discovery) OR a
    # raw IP — newer-firmware dogs never report a serial, so serial-only would
    # make them unaddressable.
    def resolve(token: str) -> str | None:
        if is_ip(token):
            return token
        if token in found:
            return found[token]["ip"]
        return None

    leader_ip, follower_ip = resolve(args.leader), resolve(args.follower)
    missing = [t for t, ip in ((args.leader, leader_ip), (args.follower, follower_ip)) if ip is None]
    if missing:
        show(found, args.leader, args.follower)
        print(f"\nCould not resolve: {', '.join(missing)}")
        print("Pass a serial that discovery can see, or the robot's IP directly.")
        print("Tip: ./go2_fleet.py --scan   finds dogs by port 9991, including")
        print("     newer-firmware ones that report no serial.")
        return 1

    if leader_ip == follower_ip:
        print(f"\nleader and follower resolve to the same robot ({leader_ip}) — refusing.")
        return 1

    show(found, args.leader, args.follower)
    print(f"\n  leader   -> {leader_ip}")
    print(f"  follower -> {follower_ip}")
    found.setdefault(args.leader, {"ip": leader_ip, "mac": "?"})
    found.setdefault(args.follower, {"ip": follower_ip, "mac": "?"})

    if args.mode == "fleet":
        # One process, both robots. DimOS coordinates internally; leader first.
        ips = f"{leader_ip},{follower_ip}"
        env = os.environ.copy()
        env["ROBOT_IPS"] = ips
        env["DIMOS_RUN_ID"] = f"go2-fleet-{args.leader}-{args.follower}"
        print(f"\nfleet mode: ROBOT_IPS={ips}")
        print(f"            DIMOS_RUN_ID={env['DIMOS_RUN_ID']}")
        os.execve(str(DIMOS_BIN), [str(DIMOS_BIN), "run", BP_FLEET], env)

    print("\nlaunching isolated instances:")
    bp_leader = BP_NAV3D if args.record_path else BP_SINGLE
    procs = [
        launch(args.leader, leader_ip, "leader", bp_leader, args.record_path,
               claimed={args.follower} if not is_ip(args.follower) else set(),
               viz=True),
        launch(args.follower, follower_ip, "follower", BP_SINGLE, False,
               claimed={args.leader} if not is_ip(args.leader) else set(),
               viz=False),
    ]
    print(f"\nlogs: {STATE_DIR}/")
    print("stop with:  ./go2_fleet.py --stop")
    if args.record_path:
        print(f"\nleader is recording its path (DIMOS_NAV_RECORD=1, blueprint {BP_NAV3D}).")

    try:
        while all(p.poll() is None for p in procs):
            time.sleep(2)
        print("\nan instance exited; check the logs.")
    except KeyboardInterrupt:
        print("\ninterrupted — stopping both")
        stop_all()
    return 0


if __name__ == "__main__":
    sys.exit(main())
