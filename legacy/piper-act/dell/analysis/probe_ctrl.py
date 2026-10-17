import sys
sys.path.insert(0, r"D:\Piper-CAN-Teleop\scripts")
import piper_act_portal as portal

arm = portal.Arm("candle", mock=False)
print("connect:", arm.connect())
print("ctrl_mode is method:", callable(arm.ctrl_mode))
print("ctrl_mode():", arm.ctrl_mode())
print("ctrl_mode_raw():", arm.ctrl_mode_raw())
print("status_raw():", arm.status_raw())
print("fault():", arm.fault())
arm.close()
print("ok")
