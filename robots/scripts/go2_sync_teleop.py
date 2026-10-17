#!/usr/bin/env python3
"""
Synchronized two-dog teleop: WASD drives BOTH dogs identically at once, plus a
one-shot wave (Unitree "Hello", sport API 1016 — confirmed in
vendor/unitree_sdk2_python/.../sport_client.py and constants.py, and already
used by scripts/go2_connect.py's TRICKS["hello"]).

This is deliberately NOT leader/follower (see go2_follow_bridge.py for that).
Both dogs get the exact same /cmd_vel Twist on their own LCM bus every tick —
mirrored motion, not staggered.

Movement is CONFIRMED end-to-end: /cmd_vel is the same channel the fleet
launcher's MovementManager already consumes (verified in this session's dog
logs). Fire-and-forget property applies — same as PiPER JointCtrl and the
original go2_connect.py Move: stop streaming and the dog stops. Hold a key,
it moves; release, it stops.

The WAVE call goes through DimOS's own GO2Connection RPC (not a second WebRTC
connection — DimOS already owns that channel per dog). The exact registered
method name is discovered defensively at runtime rather than hardcoded, since
it has not yet been confirmed against the live GO2Connection source; run
--list-skills once to see it, then --wave uses whichever of
hello/wave/greet/trick it finds.

USAGE (run with the DIMOS venv):
    ~/NervusOS/robots/vendor/dimos/.venv/bin/python scripts/go2_sync_teleop.py \\
        --bus-a 7667 --bus-b 7668

    Controls: W/A/S/D = forward/left/back/right (held = moving)
              Q/E      = turn left/right
              SPACE    = wave (both dogs, one-shot)
              ESC      = quit (stops both)

    --list-skills       just print each bus's available RPC skills and exit
    --wave-once         send the wave and exit, no interactive loop
"""

import argparse
import sys
import time

MCAST = "239.255.76.67"
SPEED = 0.3         # m/s, deliberately conservative — this is a demo, not a race
YAW_SPEED = 0.5     # rad/s
STREAM_HZ = 10       # matches the fire-and-forget safety property used elsewhere

try:
    import lcm
except ImportError:
    sys.exit("python-lcm not importable — use the dimos venv:\n"
             "  ~/NervusOS/robots/vendor/dimos/.venv/bin/python scripts/go2_sync_teleop.py")


def bus_url(port: int) -> str:
    return f"udpm://{MCAST}:{port}?ttl=0"


def _twist_type():
    from dimos.msgs.geometry_msgs.Twist import Twist
    return Twist


def make_twist(vx: float, vy: float, wz: float):
    Twist = _twist_type()
    t = Twist()
    t.linear.x, t.linear.y, t.linear.z = vx, vy, 0.0
    t.angular.x, t.angular.y, t.angular.z = 0.0, 0.0, wz
    return t


class DogBus:
    """One dog's raw LCM handle: publish /cmd_vel, and (best-effort) find and
    call its wave/hello skill through DimOS's own connection."""

    CMD_VEL_CH = "/cmd_vel#geometry_msgs.Twist"
    WAVE_NAMES = ("hello", "wave", "greet", "sport_hello", "trick")

    def __init__(self, port: int, label: str):
        self.port = port
        self.label = label
        self.lc = lcm.LCM(bus_url(port))

    def drive(self, vx: float, vy: float, wz: float) -> None:
        self.lc.publish(self.CMD_VEL_CH, make_twist(vx, vy, wz).lcm_encode())

    def stop(self) -> None:
        self.drive(0.0, 0.0, 0.0)

    def list_skills(self) -> list[str]:
        try:
            from dimos.porcelain.dimos import Dimos
        except ImportError as e:
            print(f"  [{self.label}] porcelain not importable: {e}")
            return []
        try:
            import os
            os.environ["LCM_DEFAULT_URL"] = bus_url(self.port)
            d = Dimos.connect(timeout=5.0)
            names = sorted(dir(d.skills)) if hasattr(d, "skills") else sorted(dir(d))
            return [n for n in names if not n.startswith("_")]
        except Exception as e:
            print(f"  [{self.label}] connect failed: {type(e).__name__}: {e}")
            return []

    def wave(self) -> bool:
        try:
            from dimos.porcelain.dimos import Dimos
            import os
            os.environ["LCM_DEFAULT_URL"] = bus_url(self.port)
            d = Dimos.connect(timeout=5.0)
            target = d.skills if hasattr(d, "skills") else d
            for name in self.WAVE_NAMES:
                fn = getattr(target, name, None)
                if callable(fn):
                    print(f"  [{self.label}] calling skills.{name}() ...")
                    fn()
                    return True
            print(f"  [{self.label}] no matching skill in {self.WAVE_NAMES}; "
                  f"run --list-skills to see what's actually registered.")
        except Exception as e:
            print(f"  [{self.label}] wave failed: {type(e).__name__}: {e}")
        return False


def read_key() -> str:
    """Single keypress, no Enter required (POSIX raw mode)."""
    import termios, tty
    fd = sys.stdin.fileno()
    old = termios.tcgetattr(fd)
    try:
        tty.setraw(fd)
        ch = sys.stdin.read(1)
    finally:
        termios.tcsetattr(fd, termios.TCSADRAIN, old)
    return ch


def interactive(a: DogBus, b: DogBus) -> None:
    print(f"\nsynchronized teleop: bus {a.port} + bus {b.port}")
    print("W/A/S/D move, Q/E turn, SPACE wave, ESC quit. Ctrl-C also stops.\n")
    keymap = {
        "w": (SPEED, 0.0, 0.0), "s": (-SPEED, 0.0, 0.0),
        "a": (0.0, SPEED, 0.0), "d": (0.0, -SPEED, 0.0),
        "q": (0.0, 0.0, YAW_SPEED), "e": (0.0, 0.0, -YAW_SPEED),
    }
    try:
        while True:
            ch = read_key().lower()
            if ch == "\x1b":  # ESC
                break
            if ch == " ":
                print("waving both dogs...")
                a.wave(); b.wave()
                continue
            if ch in keymap:
                vx, vy, wz = keymap[ch]
                print(f"  {ch} -> vx={vx:+.2f} vy={vy:+.2f} wz={wz:+.2f}  "
                      f"(streaming {STREAM_HZ}Hz for 0.3s)")
                end = time.time() + 0.3   # one key press = one short pulse
                while time.time() < end:
                    a.drive(vx, vy, wz)
                    b.drive(vx, vy, wz)
                    time.sleep(1.0 / STREAM_HZ)
                a.stop(); b.stop()
    except KeyboardInterrupt:
        pass
    finally:
        a.stop(); b.stop()
        print("\nstopped both dogs.")


def main():
    ap = argparse.ArgumentParser(description="Synchronized two-dog teleop + wave")
    ap.add_argument("--bus-a", type=int, default=7667)
    ap.add_argument("--bus-b", type=int, default=7668)
    ap.add_argument("--list-skills", action="store_true")
    ap.add_argument("--wave-once", action="store_true")
    args = ap.parse_args()

    a = DogBus(args.bus_a, "A")
    b = DogBus(args.bus_b, "B")

    if args.list_skills:
        for dog in (a, b):
            print(f"bus {dog.port} skills:", dog.list_skills())
        return 0

    if args.wave_once:
        a.wave(); b.wave()
        return 0

    interactive(a, b)
    return 0


if __name__ == "__main__":
    sys.exit(main())
