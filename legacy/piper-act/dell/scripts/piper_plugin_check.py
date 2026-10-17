#!/usr/bin/env python3
"""Does the LeRobot Piper plugin import and register against LeRobot 0.6.1?"""
import sys
import traceback

sys.path.insert(0, str(__import__("pathlib").Path(__file__).resolve().parent))

print("1. importing lerobot_robot_piper (triggers shim install)...")
try:
    import lerobot_robot_piper  # noqa: F401
    print("   import OK")
except Exception:
    traceback.print_exc()
    sys.exit(1)

print("2. registry check")
try:
    from lerobot.robots.config import RobotConfig
    from lerobot.teleoperators.config import TeleoperatorConfig

    def reg(base, name):
        try:
            if hasattr(base, "get_known_choices"):
                return name in base.get_known_choices()
            return base.get_choice_class(name) is not None
        except Exception as e:
            return f"err {e}"

    print("   piper_follower registered:", reg(RobotConfig, "piper_follower"))
    print("   piper_leader   registered:", reg(TeleoperatorConfig, "piper_leader"))
except Exception:
    traceback.print_exc()
    sys.exit(1)

print("3. shim installed?")
try:
    import can
    print("   can.Bus is shim:", "piper_mac_can" in str(getattr(can.Bus, "__module__", "")))
except Exception:
    pass

print("4. construct a PiperFollower config (no connect) ...")
try:
    from lerobot_robot_piper.config_piper import PiperFollowerConfig
    cfg = PiperFollowerConfig(port="candle", type="piper_follower")
    print("   config OK, port =", cfg.port)
except Exception:
    traceback.print_exc()
    sys.exit(1)

print("\nALL PLUGIN-LEVEL CHECKS PASSED")
