#!/usr/bin/env python3
"""
Follow bridge — makes one Go2 follow another ACROSS two isolated DimOS buses.

The fleet launcher puts each dog on its own LCM bus (7667 leader, 7668 follower)
so their Coordinators don't collide and each dog is independently addressable.
That isolation means the follower cannot see the leader's odometry directly, so
this bridge carries it across:

    leader bus  --(subscribe /odom)-->  [breadcrumb trail]  --(publish goal)-->  follower bus

The follower's ReplanningAStarPlanner consumes the goal and A*-navigates to it.
Feeding the leader's PAST positions (a breadcrumb one or two behind the leader's
current spot) makes the follower walk the leader's actual route — this is both
"follow" and "remember path": the follower retraces where the leader went rather
than cutting toward wherever the leader is now.

MODES
    # 1. Diagnose: list channels + data rates on a bus
    ./go2_follow_bridge.py --sniff --bus 7667

    # 2. Live follow (leader on 7667, follower on 7668)
    ./go2_follow_bridge.py --leader-bus 7667 --follower-bus 7668

    # 3. Record the leader's path to a file while following
    ./go2_follow_bridge.py --record ~/leader_path.json

    # 4. Replay a recorded path as follower goals (no leader needed)
    ./go2_follow_bridge.py --replay ~/leader_path.json --follower-bus 7668

Run with the dimos venv:
    ~/NervusOS/robots/vendor/dimos/.venv/bin/python scripts/go2_follow_bridge.py ...
"""

import argparse
import collections
import json
import math
import sys
import time

MCAST = "239.255.76.67"

# DimOS LCM channels are "<topic>#<msg_name>", verified by sniffing a live bus.
ODOM_TOPIC = "/odom"
GOAL_TOPIC = "/goal"

try:
    import lcm
except ImportError:
    sys.exit("python-lcm not importable — use the dimos venv "
             "(~/NervusOS/robots/vendor/dimos/.venv/bin/python)")


def bus_url(port: int) -> str:
    return f"udpm://{MCAST}:{port}?ttl=0"


def _load_types():
    """dimos_lcm message classes use .lcm_encode()/.lcm_decode(), not the
    stdlib lcm .encode()/.decode()."""
    from dimos_lcm.geometry_msgs import PoseStamped, PointStamped
    return PoseStamped, PointStamped


def channel_for(topic: str, msg_name: str) -> str:
    return f"{topic}#{msg_name}"


# --------------------------------------------------------------------- sniff
def sniff(port: int, secs: float) -> int:
    lc = lcm.LCM(bus_url(port))
    counts = collections.Counter()
    sizes = {}

    def on_any(channel, data):
        counts[channel] += 1
        sizes[channel] = len(data)

    lc.subscribe(".*", on_any)
    print(f"sniffing {bus_url(port)} for {secs:.0f}s ...")
    end = time.time() + secs
    while time.time() < end:
        lc.handle_timeout(200)

    if not counts:
        print("  (no traffic — is an instance running on this bus?)")
        return 1
    print(f"\n  {'msgs':>6}  {'bytes':>7}  channel")
    for ch, n in sorted(counts.items(), key=lambda x: -x[1]):
        flag = "  <== odom" if "odom" in ch.lower() else ""
        print(f"  {n:6d}  {sizes.get(ch, 0):7d}  {ch}{flag}")
    return 0


# ------------------------------------------------------------------ helpers
def pose_xy(pose_stamped):
    """Extract (x, y) from a decoded PoseStamped."""
    p = pose_stamped.pose.position
    return float(p.x), float(p.y)


def make_goal(PointStamped, x: float, y: float, frame: str = "map"):
    g = PointStamped()
    try:
        g.header.frame_id = frame
        g.header.stamp.sec = int(time.time())
    except Exception:
        pass
    g.point.x = float(x)
    g.point.y = float(y)
    g.point.z = 0.0
    return g


# ------------------------------------------------------------------- follow
def follow(leader_port, follower_port, spacing, lag, rate, record_path, replay_path):
    PoseStamped, PointStamped = _load_types()
    goal_ch = channel_for(GOAL_TOPIC, PointStamped().msg_name if hasattr(PointStamped(), "msg_name")
                          else "geometry_msgs.PointStamped")
    follower = lcm.LCM(bus_url(follower_port))

    # -------- replay mode: no leader, just play a saved trail --------
    if replay_path:
        trail = json.load(open(replay_path))
        print(f"replaying {len(trail)} waypoints -> follower bus {follower_port}")
        for i, (x, y) in enumerate(trail):
            follower.publish(goal_ch, make_goal(PointStamped, x, y).lcm_encode())
            print(f"  goal {i+1}/{len(trail)}: ({x:.2f}, {y:.2f})")
            time.sleep(1.0 / rate)
        print("replay done.")
        return 0

    # -------- live follow: leader odom -> breadcrumb -> follower goal --------
    odom_ch = channel_for(ODOM_TOPIC, PoseStamped().msg_name if hasattr(PoseStamped(), "msg_name")
                         else "geometry_msgs.PoseStamped")
    leader = lcm.LCM(bus_url(leader_port))
    trail = collections.deque()          # breadcrumbs the follower still owes
    recorded = []
    last = {"xy": None}

    def on_odom(channel, data):
        ps = PoseStamped.lcm_decode(data)
        x, y = pose_xy(ps)
        lx = last["xy"]
        if lx is None or math.hypot(x - lx[0], y - lx[1]) >= spacing:
            trail.append((x, y))
            recorded.append((x, y))
            last["xy"] = (x, y)

    leader.subscribe(odom_ch, on_odom)
    print(f"following: leader {leader_port} {odom_ch}")
    print(f"       ->  follower {follower_port} {goal_ch}")
    print(f"       spacing={spacing}m  lag={lag} breadcrumbs  rate={rate}Hz")
    print("Ctrl-C to stop.\n")

    next_goal = time.time()
    try:
        while True:
            leader.handle_timeout(50)
            now = time.time()
            # Publish a goal only once we have more than `lag` breadcrumbs, so the
            # follower trails the leader rather than chasing its exact position.
            if now >= next_goal and len(trail) > lag:
                x, y = trail.popleft()
                follower.publish(goal_ch, make_goal(PointStamped, x, y).lcm_encode())
                print(f"  goal ({x:+.2f}, {y:+.2f})   pending={len(trail)}")
                next_goal = now + 1.0 / rate
    except KeyboardInterrupt:
        print(f"\nstopped. {len(recorded)} breadcrumbs collected.")
        if record_path:
            json.dump(recorded, open(record_path, "w"))
            print(f"saved trail -> {record_path}")
    return 0


def record_only(port, spacing, out_path):
    """Subscribe to /odom on one bus and save the trail. Publishes NOTHING, so
    the dog is free to be driven by hand/app while we record where it goes.
    This is the 'remember' half of remember-path, provable with a single dog."""
    PoseStamped, _ = _load_types()
    odom_ch = channel_for(ODOM_TOPIC, PoseStamped().msg_name)
    lc = lcm.LCM(bus_url(port))
    trail, last = [], {"xy": None}

    def on_odom(channel, data):
        ps = PoseStamped.lcm_decode(data)
        x, y = pose_xy(ps)
        lx = last["xy"]
        if lx is None or math.hypot(x - lx[0], y - lx[1]) >= spacing:
            trail.append((x, y))
            last["xy"] = (x, y)
            print(f"  breadcrumb {len(trail):3d}: ({x:+.2f}, {y:+.2f})")

    lc.subscribe(odom_ch, on_odom)
    print(f"recording {odom_ch} on bus {port} (spacing {spacing}m). Drive the dog now.")
    print("Ctrl-C to stop and save.\n")
    try:
        while True:
            lc.handle_timeout(200)
    except KeyboardInterrupt:
        json.dump(trail, open(out_path, "w"))
        print(f"\nsaved {len(trail)} breadcrumbs -> {out_path}")
    return 0


def test_goal(port, dx, dy):
    """Publish ONE goal relative to the dog's current position. Small + safe way
    to confirm the dog obeys /goal before replaying a whole path."""
    PoseStamped, PointStamped = _load_types()
    lc = lcm.LCM(bus_url(port))
    cur = {"xy": None}
    lc.subscribe(channel_for(ODOM_TOPIC, PoseStamped().msg_name),
                 lambda ch, d: cur.__setitem__("xy", pose_xy(PoseStamped.lcm_decode(d))))
    t = time.time()
    while cur["xy"] is None and time.time() - t < 5:
        lc.handle_timeout(200)
    if cur["xy"] is None:
        print("no odom on this bus — is the dog instance running?")
        return 1
    x0, y0 = cur["xy"]
    gx, gy = x0 + dx, y0 + dy
    goal_ch = channel_for(GOAL_TOPIC, PointStamped().msg_name)
    print(f"current ({x0:+.2f},{y0:+.2f}) -> goal ({gx:+.2f},{gy:+.2f})  [{goal_ch}]")
    print("publishing goal in 3s — WATCH THE DOG, it will walk there. Ctrl-C to abort.")
    time.sleep(3)
    lc.publish(goal_ch, make_goal(PointStamped, gx, gy).lcm_encode())
    print("goal sent.")
    return 0


def main():
    ap = argparse.ArgumentParser(description="Go2 leader/follower bridge across DimOS buses")
    ap.add_argument("--sniff", action="store_true", help="list channels on a bus and exit")
    ap.add_argument("--record-only", metavar="FILE",
                    help="record /odom on --bus to FILE; publishes nothing (drive dog by hand)")
    ap.add_argument("--test-goal", nargs=2, type=float, metavar=("DX", "DY"),
                    help="publish ONE goal at current+(DX,DY) on --bus; the dog will move")
    ap.add_argument("--bus", type=int, default=7667, help="bus port for --sniff/--record-only/--test-goal")
    ap.add_argument("--secs", type=float, default=8.0, help="sniff duration")
    ap.add_argument("--leader-bus", type=int, default=7667)
    ap.add_argument("--follower-bus", type=int, default=7668)
    ap.add_argument("--spacing", type=float, default=0.30,
                    help="min metres between breadcrumbs")
    ap.add_argument("--lag", type=int, default=2,
                    help="breadcrumbs the follower stays behind the leader")
    ap.add_argument("--rate", type=float, default=2.0, help="goals per second")
    ap.add_argument("--record", metavar="FILE", help="save the leader's trail here")
    ap.add_argument("--replay", metavar="FILE", help="replay a saved trail (no leader)")
    args = ap.parse_args()

    if args.sniff:
        return sniff(args.bus, args.secs)
    if args.record_only:
        return record_only(args.bus, args.spacing, args.record_only)
    if args.test_goal:
        return test_goal(args.bus, args.test_goal[0], args.test_goal[1])
    return follow(args.leader_bus, args.follower_bus, args.spacing,
                  args.lag, args.rate, args.record, args.replay)


if __name__ == "__main__":
    try:
        sys.exit(main())
    except KeyboardInterrupt:
        sys.exit(130)
