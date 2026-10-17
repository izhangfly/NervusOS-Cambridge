import sys, time
sys.path.insert(0, r"D:\Piper-CAN-Teleop\scripts")
import piper_mac_can; piper_mac_can.install()
from piper_sdk import C_PiperInterface_V2, C_PiperForwardKinematics
p = C_PiperInterface_V2("candle", judge_flag=False)
p.ConnectPort(piper_init=False)          # NOTE: no EnableFkCal()
time.sleep(1.5)
fk = C_PiperForwardKinematics(0x01)
k = 1000.0 * fk.RADIAN
worst = 0.0
for i in range(10):
    j = p.GetArmJointMsgs().joint_state
    e = p.GetArmEndPoseMsgs().end_pose
    links = fk.CalFK([j.joint_1/k, j.joint_2/k, j.joint_3/k, j.joint_4/k, j.joint_5/k, j.joint_6/k])
    tip = links[-1]
    arm_xyz = (e.X_axis/1000, e.Y_axis/1000, e.Z_axis/1000)
    err = max(abs(tip[0]-arm_xyz[0]), abs(tip[1]-arm_xyz[1]), abs(tip[2]-arm_xyz[2]))
    worst = max(worst, err)
    if i < 3:
        print(f"  my FK tip xyz = {[round(v,1) for v in tip[:3]]}   arm onboard end pose = {[round(v,1) for v in arm_xyz]}   diff={err:.2f} mm")
    time.sleep(0.2)
print(f"  links computed per call: {len(links)}   worst tip error over 10 samples: {worst:.2f} mm")
print("  FK CORRECT" if worst < 2.0 and any(abs(v) > 1 for v in tip[:3]) else "  FK WRONG OR ZERO")
p.DisconnectPort()
