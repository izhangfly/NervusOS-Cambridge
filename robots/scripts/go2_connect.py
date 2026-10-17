#!/usr/bin/env python3
"""
Connect to a Unitree GO2 over WebRTC and probe it. Optionally drive it, slowly.

WebRTC is the path that works on a consumer GO2 (Air/Pro) with no jailbreak —
it is what DimOS uses. The DDS path in unitree_sdk2_python generally needs a
wired 192.168.123.x link and an EDU unit.

You do NOT need the robot's IP. Give it the serial and LocalSTA finds the IP by
multicast (group 231.1.1.1) — the dog just has to be on the same LAN. `--discover`
lists every dog it can see as SN -> IP, which is how you learn an unknown IP.

Usage:
    python scripts/go2_connect.py --discover                        # list SN -> IP
    python scripts/go2_connect.py --serial B42D...XX                # probe (no IP needed)
    python scripts/go2_connect.py --serial B42D...XX --move forward # 1 s nudge
    python scripts/go2_connect.py --serial B42D...XX --stand
    python scripts/go2_connect.py --serial B42D...XX --damp         # make safe

Safety: motion is opt-in, speed-capped (SPEED_CAP), time-boxed (MOVE_SECONDS),
and every run ends by sending StopMove. Move is fire-and-forget on the robot
side, so it must be streamed — stop streaming and the dog stops on its own.

⚠ Drop any VPN first. A default route over utun makes 10.88.15.x unreachable.
"""

from __future__ import annotations

import argparse
import asyncio
import contextlib
import io
import os
import sys

from unitree_webrtc_connect.multicast_scanner import discover_ip_sn
from unitree_webrtc_connect.webrtc_driver import (
    UnitreeWebRTCConnection,
    WebRTCConnectionMethod,
)

# --- safety envelope -------------------------------------------------------
SPEED_CAP = 0.3  # m/s   — deliberately gentle for indoor demos
YAW_CAP = 0.5  # rad/s
MOVE_SECONDS = 1.0  # how long a --move burst lasts
MOVE_HZ = 10  # command stream rate

# Sport API IDs (from unitree_sdk2py/go2/sport/sport_api.py)
API = {
    "damp": 1001,
    "balance_stand": 1002,
    "stop_move": 1003,
    "stand_up": 1004,
    "stand_down": 1005,
    "recovery_stand": 1006,
    "move": 1008,
    "hello": 1016,
}

# One-shot "tricks" — no parameters, fire and the robot plays the canned motion.
# Kept separate from motion streaming; each is a single request.
TRICKS = {
    "sit": 1009,
    "rise_sit": 1010,
    "stand_down": 1005,  # lie down
    "recovery": 1006,
    "balance": 1002,
    "stretch": 1017,
    "hello": 1016,
    "dance1": 1022,
    "dance2": 1023,
    "pose": 1028,
    "scrape": 1029,
    "heart": 1036,
}

MOVES = {
    "forward": (SPEED_CAP, 0.0, 0.0),
    "back": (-SPEED_CAP, 0.0, 0.0),
    "left": (0.0, SPEED_CAP, 0.0),
    "right": (0.0, -SPEED_CAP, 0.0),
    "turn_left": (0.0, 0.0, YAW_CAP),
    "turn_right": (0.0, 0.0, -YAW_CAP),
}


async def enable_lidar_probe(conn: UnitreeWebRTCConnection, seconds: float = 5.0) -> None:
    """Switch the L1 LIDAR on and prove point-cloud frames flow.

    The GO2 ships with the LIDAR stream off. Publishing "on" to rt/utlidar/switch
    starts it; "off" stops it. Only the Pro/EDU carry the L1 — the Air has none.
    """
    frames: list[dict] = []
    await conn.datachannel.disableTrafficSaving(True)  # else the map is throttled
    conn.datachannel.set_decoder(decoder_type="libvoxel")
    conn.datachannel.pub_sub.subscribe("rt/utlidar/voxel_map_compressed", frames.append)
    conn.datachannel.pub_sub.publish_without_callback("rt/utlidar/switch", "on")
    print(f"LIDAR switched on; listening {seconds:.0f}s ...")
    await asyncio.sleep(seconds)
    print(f"lidar frames received: {len(frames)}")
    if frames:
        data = frames[-1].get("data", {})
        inner = data.get("data", {}) if isinstance(data, dict) else {}
        pts = inner.get("point_count") if isinstance(inner, dict) else None
        print(f"  latest frame point_count: {pts}")
        print("  ✓ Pro LIDAR is alive. (publish 'off' to rt/utlidar/switch to stop it.)")
    else:
        print("  ✗ no frames — is this the Pro (Air has no L1)? try again, dog standing.")


async def sport(conn: UnitreeWebRTCConnection, api_id: int, parameter=None) -> None:
    """Publish one sport-mode request on the data channel."""
    payload = {"api_id": api_id}
    if parameter is not None:
        payload["parameter"] = parameter
    await conn.datachannel.pub_sub.publish_request_new("rt/api/sport/request", payload)


async def stream_move(conn: UnitreeWebRTCConnection, vx: float, vy: float, vyaw: float) -> None:
    """Move is fire-and-forget: stream it, then always stop."""
    print(f"moving vx={vx} vy={vy} vyaw={vyaw} for {MOVE_SECONDS}s ...")
    ticks = int(MOVE_SECONDS * MOVE_HZ)
    try:
        for _ in range(ticks):
            await sport(conn, API["move"], {"x": vx, "y": vy, "z": vyaw})
            await asyncio.sleep(1 / MOVE_HZ)
    finally:
        await sport(conn, API["stop_move"])
        print("StopMove sent.")


def make_connection(args: argparse.Namespace) -> UnitreeWebRTCConnection:
    """Build a connection from whichever locator the user gave.

    Go2 firmware >= 1.1.15 uses a per-device AES-128 key for the LAN handshake
    (`con_notify` returns data2=3). Without it the connect fails outright; it is
    ignored on older firmware, so passing it always is safe. Fetch once with:
        unitree-fetch-aes-key --email YOU --password ... --sn <serial>
    """
    key = args.aes_key or os.environ.get("UNITREE_AES_128_KEY")
    if key:
        print("using per-device AES-128 key (data2=3 firmware)")
    if args.ap:
        print("connecting over WebRTC (LocalAP — join the dog's hotspot first) ...")
        return UnitreeWebRTCConnection(WebRTCConnectionMethod.LocalAP, aes_128_key=key)
    if args.serial:
        print(f"connecting to serial {args.serial} over WebRTC (LocalSTA) ...")
        return UnitreeWebRTCConnection(
            WebRTCConnectionMethod.LocalSTA, serialNumber=args.serial, aes_128_key=key
        )
    print(f"connecting to {args.ip} over WebRTC (LocalSTA) ...")
    return UnitreeWebRTCConnection(WebRTCConnectionMethod.LocalSTA, ip=args.ip, aes_128_key=key)


async def run(args: argparse.Namespace) -> None:
    conn = make_connection(args)
    await conn.connect()
    print("connected.\n")

    try:
        # Read-only: subscribe briefly to low state so we can prove data flows.
        seen: list[dict] = []

        def on_low_state(message):
            if len(seen) < 1:
                seen.append(message)

        conn.datachannel.pub_sub.subscribe("rt/lf/lowstate", on_low_state)
        await asyncio.sleep(2.0)
        print(f"lowstate messages received: {len(seen)}")
        if seen:
            data = seen[0].get("data", {})
            keys = list(data)[:12] if isinstance(data, dict) else type(data).__name__
            print(f"lowstate keys: {keys}")

        if args.lidar:
            await enable_lidar_probe(conn)
        if args.trick:
            await sport(conn, TRICKS[args.trick])
            print(f"{args.trick} sent.")
        if args.hello:
            await sport(conn, API["hello"])
            print("Hello sent.")
        if args.stand:
            await sport(conn, API["stand_up"])
            print("StandUp sent.")
        if args.move:
            await stream_move(conn, *MOVES[args.move])
        if args.damp:
            await sport(conn, API["damp"])
            print("Damp sent — motors compliant.")
    finally:
        # Always leave the dog not-moving, whatever happened above.
        try:
            await sport(conn, API["stop_move"])
        except Exception:  # noqa: BLE001 - best-effort on the way out
            pass
        print("\ndone.")


def main() -> None:
    parser = argparse.ArgumentParser(description="Probe / gently drive a Unitree GO2 over WebRTC.")
    locator = parser.add_mutually_exclusive_group()  # not required: --discover needs no locator
    locator.add_argument("--ip", help="robot IP on your LAN, e.g. 10.72.x.x")
    locator.add_argument("--serial", help="robot serial, e.g. B42D1000AIRXXXXX (LocalSTA by serial)")
    locator.add_argument("--ap", action="store_true", help="connect to the dog's own hotspot (LocalAP)")
    parser.add_argument("--discover", action="store_true", help="list every dog on the LAN as SN -> IP, then exit")
    parser.add_argument("--print-ip", action="store_true",
                        help="print ONLY the IP for --serial (for: export ROBOT_IP=$(... --print-ip))")
    parser.add_argument("--move", choices=sorted(MOVES), help=f"{MOVE_SECONDS}s capped motion burst")
    parser.add_argument("--trick", choices=sorted(TRICKS), help="one-shot canned motion (sit, dance1, ...)")
    parser.add_argument("--aes-key", help="per-device AES-128 key (32 hex) for Go2 fw >=1.1.15; "
                                          "defaults to $UNITREE_AES_128_KEY")
    parser.add_argument("--lidar", action="store_true", help="switch the L1 LIDAR on and probe frames (Pro only)")
    parser.add_argument("--stand", action="store_true", help="send StandUp")
    parser.add_argument("--damp", action="store_true", help="send Damp (safe/compliant)")
    parser.add_argument("--hello", action="store_true", help="send Hello (waves)")
    args = parser.parse_args()

    if args.print_ip:
        # Only the bare IP goes to stdout so it can be captured in a shell var.
        if not args.serial:
            parser.error("--print-ip requires --serial")
        with contextlib.redirect_stdout(io.StringIO()):
            found = discover_ip_sn(timeout=3)
        ip = found.get(args.serial)
        if not ip:
            print(f"serial {args.serial} not found on the LAN", file=sys.stderr)
            sys.exit(1)
        print(ip)
        return

    if args.discover:
        found = discover_ip_sn(timeout=3)
        if not found:
            sys.exit(
                "\nNo dogs answered. Checks: same Wi-Fi/LAN as the dog? VPN off?\n"
                "A brand-new dog may still be BLE-only — provision it onto Wi-Fi first\n"
                "(dimos go2tool connect-wifi --name Go2_XXXXX --ssid ... --password ...)."
            )
        print(f"\n{len(found)} dog(s):")
        for sn, ip in found.items():
            print(f"  {sn}  ->  {ip}")
        return

    if not (args.ip or args.serial or args.ap):
        parser.error("one of --ip / --serial / --ap is required (or use --discover)")

    try:
        asyncio.run(run(args))
    except KeyboardInterrupt:
        sys.exit("\ninterrupted.")


if __name__ == "__main__":
    main()
