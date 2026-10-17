#!/usr/bin/env python3
"""Real connect test: does the LeRobot Piper plugin drive the arm via the shim?

Constructs a PiperFollower, opens the CAN port through the shim, reads a joint
observation, then disconnects. Does NOT move the arm (no parking/calibrate).
"""
import sys
import time
import traceback

sys.path.insert(0, str(__import__("pathlib").Path(__file__).resolve().parent))

# The plugin's __init__ installs the shim on import; do it explicitly too so the
# ordering is obvious and this script works even if the plugin import is skipped.
import piper_mac_can
piper_mac_can.install()

from lerobot_robot_piper.config_piper import PiperFollowerConfig
from lerobot_robot_piper.piper_follower import PiperFollower


def main():
    print("constructing PiperFollower(port='candle')...")
    cfg = PiperFollowerConfig(port="candle")
    robot = PiperFollower(cfg)
    print("  constructed, bus =", type(robot.bus).__name__)
    print("  shim bus:", type(robot.bus.piper._C_PiperInterface_V2__arm_can.bus).__name__)

    print("connecting CAN (no parking)...")
    robot.bus.connect()
    time.sleep(1.0)
    print("  connected:", robot.bus.is_connected)

    print("reading joint observation...")
    obs = robot.bus.get_action()
    print("  joints:", {k: round(v, 2) for k, v in obs.items()})

    print("reading via robot.get_observation()...")
    try:
        o = robot.get_observation()
        print("  obs keys:", sorted(o.keys()))
        print("  joint1.pos:", round(o["joint1.pos"], 2))
    except Exception as e:
        print("  get_observation raised:", type(e).__name__, e)

    print("disconnecting...")
    robot.disconnect()
    print("DONE — plugin drives the arm through the shim.")
    return 0


if __name__ == "__main__":
    try:
        sys.exit(main())
    except Exception:
        traceback.print_exc()
        sys.exit(1)
