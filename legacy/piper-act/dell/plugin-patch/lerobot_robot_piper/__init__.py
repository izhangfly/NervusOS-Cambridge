# LeRobot plugin for Agilex Piper robotic arm.
# Importing this package registers configs with LeRobot's RobotConfig/TeleoperatorConfig registry.
#
# MODIFIED (Apache-2.0 section 4(b)): changed from WeGo-Robotics/lerobot_robot_piper@ed387ed9 by the piper-act
# project, 2026-09-10. Added the block below that installs the piper_mac_can CAN shim on non-Linux hosts.

import os
import platform
import sys

# Windows / macOS have no SocketCAN. Route the arm's CAN onto the candleLight USB
# stick by installing the project's shim (piper_mac_can) BEFORE any
# C_PiperInterface_V2 is constructed. The shim rewrites can.interface.Bus and
# neutralises the /sys/class/net existence check the SDK otherwise runs, so the
# plugin's C_PiperInterface_V2(port) call (judge_flag defaults True) works.
if platform.system() != "Linux":
    try:
        import piper_mac_can
    except ImportError:
        _shim_dir = os.environ.get("PIPER_SHIM_DIR", r"D:\Piper-CAN-Teleop\scripts")
        if _shim_dir not in sys.path:
            sys.path.insert(0, _shim_dir)
        import piper_mac_can
    piper_mac_can.install()

from .config_piper import PiperFollowerConfig
from .config_piper_leader import PiperLeaderConfig
from .piper_follower import PiperFollower
from .piper_leader import PiperLeader
