#!/usr/bin/env python3
"""
Two-GO2 leader–follower keyboard teleop, over WebRTC.

You drive the LEADER live with the keyboard. The FOLLOWER replays the leader's
exact command history `--delay` seconds later — same velocities, same tricks,
in the same order — so it traces the same route a few seconds behind. Line the
follower up behind the leader (same heading) and you get a convoy.

Why command-replay and not spatial path-tracking: two GO2 Air units on Wi-Fi
share no localization frame. GO2 `Move(vx,vy,vyaw)` is dead-reckoned by each
robot's own controller, so replaying an identical (vx,vy,vyaw) time series
reproduces the same body-frame trajectory. From an aligned start that is the
same ground path. Drift accumulates with no global reference — fine for a demo,
not for a warehouse.

This is the WebRTC equivalent of "DimOS keyboard teleop" for this hardware.
DimOS's own unitree_go2 adapter is DDS/unitree_sdk2py (wired 192.168.123.x /
EDU) and cannot drive two Air dogs on Wi-Fi; this can.

    CONTROLS
      ↑ ↓            forward / backward         (Shift = fast)
      ← →            turn left / right          (Shift = fast)
      space          stop moving
      g              DAMP both dogs now (safety — immediate, not delayed)
      1 sit   2 stand-up   3 lie-down   4 balance   5 stretch
      6 hello 7 dance1     8 dance2     9 pose      0 scrape   - heart
      ?              reprint this help
      q / Esc        quit (stops + damps both, disconnects)

Safety: speeds are capped (NORMAL/FAST below); movement auto-stops ~0.4 s after
you release a key (terminals autorepeat while held); quitting damps both dogs.
Keep clear, keep the space open, keep a power button reachable.

⚠ Both dogs must be on the same LAN and a TUN VPN/proxy must be OFF (see
  `go2_connect.py --discover`). Stand them where they each have room to move.
"""

from __future__ import annotations

import argparse
import asyncio
import os
import sys
import termios
import time
import tty
from collections import deque

from unitree_webrtc_connect.webrtc_driver import (
    UnitreeWebRTCConnection,
    WebRTCConnectionMethod,
)

# Shared, already-validated primitives.
from go2_connect import API, TRICKS, sport

# --- this rig's dogs (override with --leader / --follower) -----------------
DEFAULT_LEADER = "B42D2000PROXXXXX"    # the second dog you drive
DEFAULT_FOLLOWER = "B42D1000AIRXXXXX"  # your original dog, the one that follows

# --- capped speed envelope -------------------------------------------------
NORMAL_SPEED = 0.3   # m/s
FAST_SPEED = 0.6     # m/s (Shift)
NORMAL_YAW = 0.5     # rad/s
FAST_YAW = 1.0       # rad/s (Shift)

HZ = 10              # command stream rate for both dogs
HOLD_TIMEOUT = 0.4   # s since last movement key before the leader auto-stops
DEFAULT_DELAY = 1.5  # s the follower trails the leader

# number key -> trick name in TRICKS
KEY_TRICKS = {
    "1": "sit", "2": "recovery", "3": "stand_down", "4": "balance",
    "5": "stretch", "6": "hello", "7": "dance1", "8": "dance2",
    "9": "pose", "0": "scrape", "-": "heart",
}

HELP = __doc__[__doc__.index("    CONTROLS"):__doc__.index("\nSafety")]


# --- keyboard token parsing ------------------------------------------------
# Arrow keys arrive as CSI escape sequences; Shift+arrow adds a ";2" modifier.
_ARROWS = {b"A": "up", b"B": "down", b"C": "right", b"D": "left"}


def parse_bytes(data: bytes) -> list[tuple]:
    """Turn a raw stdin read into a list of control tokens."""
    tokens: list[tuple] = []
    i = 0
    while i < len(data):
        b = data[i]
        if b == 0x1B:  # ESC
            # Shift+arrow: ESC [ 1 ; 2 <A-D>   (6 bytes)
            if data[i:i + 4] == b"\x1b[1;" and i + 6 <= len(data) and data[i + 5:i + 6] in _ARROWS:
                tokens.append(("arrow", _ARROWS[data[i + 5:i + 6]], True))
                i += 6
                continue
            # plain arrow: ESC [ <A-D>        (3 bytes)
            if data[i:i + 2] == b"\x1b[" and i + 3 <= len(data) and data[i + 2:i + 3] in _ARROWS:
                tokens.append(("arrow", _ARROWS[data[i + 2:i + 3]], False))
                i += 3
                continue
            # lone ESC = quit
            tokens.append(("quit",))
            i += 1
            continue
        ch = chr(b)
        if ch in (" ",):
            tokens.append(("stop",))
        elif ch in ("q", "\x03"):  # q or Ctrl-C
            tokens.append(("quit",))
        elif ch == "g":
            tokens.append(("damp",))
        elif ch == "?":
            tokens.append(("help",))
        elif ch in KEY_TRICKS:
            tokens.append(("trick", KEY_TRICKS[ch]))
        i += 1
    return tokens


def _arrow_to_twist(direction: str, fast: bool) -> tuple[float, float, float]:
    v = FAST_SPEED if fast else NORMAL_SPEED
    w = FAST_YAW if fast else NORMAL_YAW
    return {
        "up": (v, 0.0, 0.0),
        "down": (-v, 0.0, 0.0),
        "left": (0.0, 0.0, w),
        "right": (0.0, 0.0, -w),
    }[direction]


def _looks_like_ip(s: str) -> bool:
    parts = s.split(".")
    return len(parts) == 4 and all(p.isdigit() for p in parts)


def make_conn(locator: str) -> UnitreeWebRTCConnection:
    if _looks_like_ip(locator):
        return UnitreeWebRTCConnection(WebRTCConnectionMethod.LocalSTA, ip=locator)
    return UnitreeWebRTCConnection(WebRTCConnectionMethod.LocalSTA, serialNumber=locator)


class Teleop:
    """Leader driven live; follower replays the leader's history after `delay`."""

    def __init__(self, leader, follower, delay: float) -> None:
        self.leader = leader
        self.follower = follower
        self.delay = delay
        self.running = True

        # leader command state
        self.twist = [0.0, 0.0, 0.0]  # vx, vy, vyaw target
        self.last_move_ts = 0.0
        self._leader_moving = False
        self._last_appended: tuple | None = None

        # timeline the follower consumes (ts, kind, payload)
        self.pending: deque[tuple] = deque()
        self.follower_twist = [0.0, 0.0, 0.0]
        self._follower_moving = False

    def _append(self, kind: str, payload) -> None:
        self.pending.append((time.monotonic(), kind, payload))

    # --- keyboard handling (awaits allowed; runs in the consumer task) -----
    async def handle(self, token: tuple) -> None:
        kind = token[0]
        if kind == "arrow":
            self.twist = list(_arrow_to_twist(token[1], token[2]))
            self.last_move_ts = time.monotonic()
        elif kind == "stop":
            self.twist = [0.0, 0.0, 0.0]
            self.last_move_ts = time.monotonic()
        elif kind == "trick":
            tid = TRICKS[token[1]]
            print(f"  leader: {token[1]}")
            await sport(self.leader, tid)
            self._append("trick", tid)  # follower does it too, delayed
        elif kind == "damp":
            print("  DAMP both (safety)")
            self.twist = [0.0, 0.0, 0.0]
            await sport(self.leader, API["damp"])
            await sport(self.follower, API["damp"])
        elif kind == "help":
            print(HELP)
        elif kind == "quit":
            self.running = False

    # --- 10 Hz control ticks ----------------------------------------------
    async def _drive(self, conn, twist, moving_flag_attr: str) -> None:
        vx, vy, vyaw = twist
        if vx == 0.0 and vy == 0.0 and vyaw == 0.0:
            if getattr(self, moving_flag_attr):
                await sport(conn, API["stop_move"])
                setattr(self, moving_flag_attr, False)
        else:
            await sport(conn, API["move"], {"x": vx, "y": vy, "z": vyaw})
            setattr(self, moving_flag_attr, True)

    async def leader_tick(self) -> None:
        now = time.monotonic()
        if now - self.last_move_ts > HOLD_TIMEOUT and any(self.twist):
            self.twist = [0.0, 0.0, 0.0]
        eff = tuple(self.twist)
        if eff != self._last_appended:  # record every change for the follower
            self._append("move", eff)
            self._last_appended = eff
        await self._drive(self.leader, eff, "_leader_moving")

    async def follower_tick(self) -> None:
        target = time.monotonic() - self.delay
        while self.pending and self.pending[0][0] <= target:
            _, kind, payload = self.pending.popleft()
            if kind == "move":
                self.follower_twist = list(payload)
            elif kind == "trick":
                await sport(self.follower, payload)
        await self._drive(self.follower, tuple(self.follower_twist), "_follower_moving")

    async def control_loop(self) -> None:
        while self.running:
            await self.leader_tick()
            await self.follower_tick()
            await asyncio.sleep(1 / HZ)


async def run(args: argparse.Namespace) -> None:
    print(f"connecting leader={args.leader}  follower={args.follower} ...")
    leader = make_conn(args.leader)
    follower = make_conn(args.follower)
    await asyncio.gather(leader.connect(), follower.connect())
    print("both connected.\n")

    # Put both in a stable stand so they are ready to walk.
    await sport(leader, API["balance_stand"])
    await sport(follower, API["balance_stand"])
    await asyncio.sleep(0.5)

    teleop = Teleop(leader, follower, args.delay)
    print(HELP)
    print(f"\nfollower delay: {args.delay:.1f}s — line it up behind the leader.\n")

    # Raw, non-blocking stdin; feed bytes to the parser via the event loop.
    fd = sys.stdin.fileno()
    old = termios.tcgetattr(fd)
    tty.setcbreak(fd)  # cbreak keeps Ctrl-C working
    os.set_blocking(fd, False)
    queue: asyncio.Queue = asyncio.Queue()
    loop = asyncio.get_event_loop()

    def on_stdin() -> None:
        try:
            data = os.read(fd, 1024)
        except BlockingIOError:
            return
        for tok in parse_bytes(data):
            queue.put_nowait(tok)

    loop.add_reader(fd, on_stdin)

    async def consume_keys() -> None:
        while teleop.running:
            token = await queue.get()
            await teleop.handle(token)

    try:
        await asyncio.gather(teleop.control_loop(), consume_keys())
    finally:
        loop.remove_reader(fd)
        termios.tcsetattr(fd, termios.TCSADRAIN, old)
        print("\nstopping + damping both ...")
        for conn in (leader, follower):
            try:
                await sport(conn, API["stop_move"])
                await sport(conn, API["damp"])
            except Exception:  # noqa: BLE001 - best-effort shutdown
                pass
        await asyncio.gather(leader.disconnect(), follower.disconnect(), return_exceptions=True)
        print("done.")


def main() -> None:
    parser = argparse.ArgumentParser(description="Two-GO2 leader–follower keyboard teleop (WebRTC).")
    parser.add_argument("--leader", default=DEFAULT_LEADER, help="leader serial or IP (you drive this)")
    parser.add_argument("--follower", default=DEFAULT_FOLLOWER, help="follower serial or IP (this trails)")
    parser.add_argument("--delay", type=float, default=DEFAULT_DELAY, help="seconds the follower trails (0 = lockstep)")
    args = parser.parse_args()
    try:
        asyncio.run(run(args))
    except KeyboardInterrupt:
        pass


if __name__ == "__main__":
    main()
