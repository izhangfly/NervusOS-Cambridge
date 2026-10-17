"""
A small, safe action API for the AgileX PiPER — the surface a policy drives.

Everything here encodes hard-won facts about this arm; see
`C:\\Users\\zhy10\\Shared\\Piper Arm\\README.md` for the full story. The ones
that shape this file:

* The SDK hardcodes socketcan, so `piper_mac_can.install()` must run BEFORE the
  interface is constructed. It routes CAN onto the candleLight USB stick.
* Joint-zero is a kinematic singularity (wrist Euler angles land at RY~85 deg,
  one degree off gimbal lock). Only Z is joggable there. READY_JOINTS is an
  elbow-bent pose measured to give all six Cartesian axes.
* `EndPoseCtrl` is point-to-point MOVE P and every setpoint restarts the
  planner's ramp, so streamed motion wants a target about one control period
  ahead — further ahead is measurably SLOWER (0.35 s lookahead crawled at
  2 mm/s where 0.05 s gave 88 mm/s).
* Statuses 0x02/0x03/0x04 mean "that target was unreachable", not "the arm is
  broken". They clear on their own in ~1.3 s once you stop pushing.
"""

import os
import pathlib
import sys
import time

_SCRIPTS = pathlib.Path(__file__).resolve().parent.parent / "scripts"
sys.path.insert(0, str(_SCRIPTS))

import piper_mac_can  # noqa: E402

if os.name == "nt" or sys.platform == "darwin":
    piper_mac_can.install()

from piper_sdk import C_PiperInterface_V2  # noqa: E402

MMM = MDEG = 1000.0                 # arm speaks 0.001 mm / 0.001 deg
GRIP_UNIT = 1000.0
GRIPPER_MM = (0.0, 70.0)

READY_JOINTS_DEG = (0.0, 45.0, -60.0, 0.0, 45.0, 0.0)

# Measured ceilings; see the speed sweep in the setup doc.
JOG_MAX_VEL_MM_S = 350.0
JOG_MAX_VEL_DEG_S = 240.0
STREAM_HZ = 20
LOOKAHEAD_S = 1.0 / STREAM_HZ

END_MAX_LINEAR_VEL = 900            # 0.001 m/s
END_MAX_ANGULAR_VEL = 1800          # 0.001 rad/s
END_MAX_LINEAR_ACC = 1200           # 0.001 m/s^2
END_MAX_ANGULAR_ACC = 2000          # 0.001 rad/s^2

SOFT_FAULTS = {0x02, 0x03, 0x04}    # unreachable target, not a broken arm

# Conservative box for autonomous control. A policy — or a language model — will
# happily ask for a pose through the table, so every target is clamped here
# before it reaches the arm. Widen deliberately, not by accident.
WORKSPACE = {
    "x": (60.0, 480.0),
    "y": (-320.0, 320.0),
    "z": (80.0, 600.0),
}
MAX_STEP_MM = 60.0                  # per single move_delta call
MAX_STEP_DEG = 30.0


class ArmError(RuntimeError):
    pass


class PiperArm:
    """Blocking, safety-clamped action API over the PiPER."""

    def __init__(self, iface="candle", speed=40, connect_timeout=8.0):
        self.piper = C_PiperInterface_V2(iface, judge_flag=False)
        self.piper.ConnectPort()
        t0 = time.time()
        while time.time() - t0 < connect_timeout:
            if self.piper.isOk():
                break
            time.sleep(0.1)
        if not self.piper.isOk():
            raise ArmError(
                "no CAN traffic from the arm. Is it powered, and is the CAN "
                "cable seated at both ends? Run scripts/piper_can_check.py.")
        time.sleep(0.5)
        self.speed = int(speed)
        self.piper.EndSpdAndAccParamSet(
            END_MAX_LINEAR_VEL, END_MAX_ANGULAR_VEL,
            END_MAX_LINEAR_ACC, END_MAX_ANGULAR_ACC)
        self.enabled = False

    # ---------------- state ----------------

    def pose(self):
        """[X mm, Y mm, Z mm, RX deg, RY deg, RZ deg]."""
        e = self.piper.GetArmEndPoseMsgs().end_pose
        return [e.X_axis / MMM, e.Y_axis / MMM, e.Z_axis / MMM,
                e.RX_axis / MDEG, e.RY_axis / MDEG, e.RZ_axis / MDEG]

    def joints(self):
        j = self.piper.GetArmJointMsgs().joint_state
        return [j.joint_1 / MDEG, j.joint_2 / MDEG, j.joint_3 / MDEG,
                j.joint_4 / MDEG, j.joint_5 / MDEG, j.joint_6 / MDEG]

    def status(self):
        """(name, value) of the arm status word."""
        st = self.piper.GetArmStatus().arm_status.arm_status
        return getattr(st, "name", "UNKNOWN"), getattr(st, "value", None)

    def is_soft_faulted(self):
        return self.status()[1] in SOFT_FAULTS

    # ---------------- control ----------------

    def enable(self):
        """Enable motors and take CAN control.

        Never send EmergencyStop(0x02) 'resume' first — it leaves the motors
        disabled and a following EnableArm silently fails to re-enable them.
        """
        self.piper.EnableArm(7, 0x02)
        time.sleep(1.2)
        self.set_speed(self.speed)
        self.enabled = True

    def disable(self):
        self.piper.DisableArm(7, 0x01)
        self.enabled = False

    def set_speed(self, speed):
        """move_mode 0x00 = MOVE P, the Cartesian mode EndPoseCtrl needs."""
        self.speed = int(max(1, min(100, speed)))
        self.piper.MotionCtrl_2(0x01, 0x00, self.speed, 0x00)

    def ready(self, timeout=6.0):
        """Joint-space move to READY. No IK, so it never faults; clears faults."""
        self.piper.ModeCtrl(0x01, 0x01, 70, 0x00)      # CAN + MOVE J
        time.sleep(0.3)
        self.piper.JointCtrl(*[int(round(d * MDEG)) for d in READY_JOINTS_DEG])
        deadline = time.time() + timeout
        while time.time() < deadline:
            time.sleep(0.1)
            if all(abs(a - b) < 2.0 for a, b in zip(self.joints(), READY_JOINTS_DEG)):
                break
        self.set_speed(self.speed)                     # back to MOVE P
        return self.pose()

    def _clamp(self, target):
        t = list(target)
        for i, ax in enumerate(("x", "y", "z")):
            lo, hi = WORKSPACE[ax]
            t[i] = max(lo, min(hi, t[i]))
        return t

    def move_to(self, target, settle=1.5, stream_s=0.6):
        """Stream a Cartesian target, then let it settle. Returns the new pose.

        The target is re-issued from the MEASURED pose each tick, so the arm is
        never commanded more than one lookahead beyond where it actually is —
        that leash is what keeps a bad target from becoming a fast bad move.
        """
        if not self.enabled:
            raise ArmError("arm is not enabled; call enable() first")
        target = self._clamp(target)
        lead_mm = max(1.0, JOG_MAX_VEL_MM_S * self.speed / 100.0 * LOOKAHEAD_S)
        lead_deg = max(1.0, JOG_MAX_VEL_DEG_S * self.speed / 100.0 * LOOKAHEAD_S)

        t0 = time.time()
        while time.time() - t0 < stream_s:
            cur = self.pose()
            step = list(cur)
            for i in range(6):
                d = target[i] - cur[i]
                cap = lead_mm if i < 3 else lead_deg
                step[i] = cur[i] + max(-cap, min(cap, d))
            self.piper.EndPoseCtrl(
                int(round(step[0] * MMM)), int(round(step[1] * MMM)),
                int(round(step[2] * MMM)), int(round(step[3] * MDEG)),
                int(round(step[4] * MDEG)), int(round(step[5] * MDEG)))
            time.sleep(1.0 / STREAM_HZ)

        # Cancel the outstanding lead so the arm stops where it is.
        cur = self.pose()
        self.piper.EndPoseCtrl(
            int(round(cur[0] * MMM)), int(round(cur[1] * MMM)),
            int(round(cur[2] * MMM)), int(round(cur[3] * MDEG)),
            int(round(cur[4] * MDEG)), int(round(cur[5] * MDEG)))
        time.sleep(settle)
        return self.pose()

    def move_delta(self, dx=0.0, dy=0.0, dz=0.0, drx=0.0, dry=0.0, drz=0.0,
                   **kw):
        """Relative Cartesian move, clamped to one sane step."""
        d = [dx, dy, dz, drx, dry, drz]
        for i in range(3):
            d[i] = max(-MAX_STEP_MM, min(MAX_STEP_MM, d[i]))
        for i in range(3, 6):
            d[i] = max(-MAX_STEP_DEG, min(MAX_STEP_DEG, d[i]))
        cur = self.pose()
        return self.move_to([cur[i] + d[i] for i in range(6)], **kw)

    def set_gripper(self, mm, settle=1.0):
        mm = max(GRIPPER_MM[0], min(GRIPPER_MM[1], float(mm)))
        self.piper.GripperCtrl(int(mm * GRIP_UNIT), 1000, 0x01, 0)
        time.sleep(settle)
        return mm

    def close(self):
        try:
            self.piper.DisconnectPort()
        except Exception:
            pass

    def __enter__(self):
        return self

    def __exit__(self, *_):
        self.close()


if __name__ == "__main__":
    with PiperArm() as a:
        print("status", a.status(), "pose", [round(v, 1) for v in a.pose()])
        a.enable()
        print("ready ->", [round(v, 1) for v in a.ready()])
        print("up 40mm ->", [round(v, 1) for v in a.move_delta(dz=40)])
        print("back    ->", [round(v, 1) for v in a.move_delta(dz=-40)])
        print("status", a.status())
