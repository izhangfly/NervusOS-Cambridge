#!/usr/bin/env python3
"""
Instrumented SDK connect. Separates the two failure modes:

  shim rx=0        -> the USB stick delivered nothing (USB/CAN layer)
  shim rx>0, SDK 0 -> frames arrived but the SDK parsed nothing (SDK layer)

Usage: piper_sdk_probe.py [--noinit]
"""
import sys
import time

sys.path.insert(0, str(__import__("pathlib").Path(__file__).resolve().parent))
import piper_mac_can
piper_mac_can.install()

from piper_sdk import C_PiperInterface_V2

noinit = "--noinit" in sys.argv

p = C_PiperInterface_V2("candle", judge_flag=False)
p.ConnectPort(piper_init=not noinit)

bus = p._C_PiperInterface_V2__arm_can.bus
time.sleep(2.0)

t1 = p.GetArmJointMsgs().time_stamp
time.sleep(0.5)
t2 = p.GetArmJointMsgs().time_stamp

print(f"piper_init={not noinit} | shim rx={bus.rx_count} tx={bus.tx_count} "
      f"echo={bus.echo_count} txfail={bus.tx_fail_count} drop={bus.tx_drop_count} "
      f"heal={bus.stall_recovery_count} "
      f"| io_alive={bus._io.is_alive()} io_dead={bus._dead} err={bus.last_error}")
print(f"  isOk={p.isOk()}  joint ts advancing={t1 != t2}  ({t1} -> {t2})")
pose = p.GetArmEndPoseMsgs().end_pose
print(f"  pose X={pose.X_axis} Y={pose.Y_axis} Z={pose.Z_axis}")
