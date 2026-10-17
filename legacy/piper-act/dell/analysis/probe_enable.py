"""READ-ONLY probe to find the 'NoneType is not callable' in the unified Arm enable/send path.
Connects like the portal (no torque, no commands), then inspects every callable in the send path."""
import sys
sys.path.insert(0, r"D:\Piper-CAN-Teleop\scripts")
import piper_act_portal as portal

arm = portal.Arm("candle", mock=False)
ok = arm.connect()
print("connect:", ok, "| robot:", type(arm.robot).__name__)
print("bus  :", type(arm.bus).__name__ if arm.bus else None)
print("piper:", type(arm.piper).__name__ if arm.piper else None)
vals, age = arm.read()
print("vals :", "ok" if vals else "None", "age %.3f" % age)
if vals:
    for name in ("_unnormalize", "enable_torque", "get_action", "calibration"):
        print(f"  bus.{name} =", type(getattr(arm.bus, name, None)).__name__ if hasattr(arm.bus, name) else "MISSING")
    for name in ("ModeCtrl", "JointCtrl", "GripperCtrl", "GetArmStatus", "EnablePiper"):
        print(f"  piper.{name} =", type(getattr(arm.piper, name, None)).__name__ if hasattr(arm.piper, name) else "MISSING")
    print("  calibration type:", type(arm.bus.calibration).__name__)
    try:
        raw = arm.bus._unnormalize(vals)
        print("  _unnormalize(vals) -> ok:", {k: raw[k] for k in list(raw)[:3]})
    except Exception as e:
        print("  _unnormalize FAILED:", type(e).__name__, e)
arm.close()
