"""AgileX NERO 7-DOF adapter — implements the DimOS ManipulatorAdapter protocol.

DimOS ships adapters for a1z, a750, openarm, openyam, piper and xarm, but not
NERO. This fills that gap by wrapping AgileX's unified `pyAgxArm` SDK.

Units: pyAgxArm's high-level API is already SI (rad, rad/s, m), which is what
the DimOS protocol requires, so no scaling happens here. Do not confuse this
with raw `piper_sdk`, whose CAN layer speaks millidegrees.

Address format mirrors scripts/arm_connect.py:
    Linux   "can0"                  -> socketcan
    macOS   "/dev/tty.usbmodem123"  -> slcan
    Windows "0"                     -> agx_cando
"""

from __future__ import annotations

import math

from pyAgxArm import AgxArmFactory, ArmModel, create_agx_arm_config

from dimos.hardware.manipulators.spec import (
    ControlMode,
    JointLimits,
    ManipulatorInfo,
)

DOF = 7

# Conservative joint limits, in degrees, taken from the NERO web UI defaults
# (manual §6.8.1) rather than the wider envelope in the spec table (§1.2).
# The arm is asked for its real limits at connect(); these are the fallback.
_LIMITS_DEG: tuple[tuple[float, float, float], ...] = (
    # (min, max, max_speed)
    (-155.0, 155.0, 179.9),  # J1
    (-100.0, 100.0, 179.9),  # J2
    (-158.0, 158.0, 179.9),  # J3
    (-58.0, 123.0, 179.9),  # J4
    (-158.0, 158.0, 224.6),  # J5
    (-42.0, 55.0, 224.6),  # J6
    (-90.0, 90.0, 224.6),  # J7
)


def _slcan_or_socketcan(address: str) -> str:
    """Infer the python-can interface from the address shape."""
    if address.startswith("/dev/"):
        return "slcan"
    if address.isdigit():
        return "agx_cando"
    return "socketcan"


class NeroAdapter:
    """AgileX NERO hardware adapter.

    Implements ManipulatorAdapter by duck typing — no inheritance required.
    """

    def __init__(self, address: str, dof: int = DOF, firmware: str = "default") -> None:
        if not address:
            raise ValueError("address is required for NeroAdapter (e.g. 'can0')")
        self._address = address
        self._dof = dof
        self._firmware = firmware
        self._arm = None
        self._control_mode = ControlMode.POSITION
        self._limits: JointLimits | None = None
        self._last_positions = [0.0] * dof

    # --- lifecycle ---------------------------------------------------------

    def connect(self) -> bool:
        config = create_agx_arm_config(
            robot=ArmModel.NERO,
            firmeware_version=self._firmware,  # SDK spelling
            channel=self._address,
            interface=_slcan_or_socketcan(self._address),
        )
        self._arm = AgxArmFactory.create_arm(config)
        self._arm.connect()
        self._limits = self._read_limits_from_arm()
        return self.is_connected()

    def disconnect(self) -> None:
        if self._arm is not None:
            self._arm.disconnect()
            self._arm = None

    def is_connected(self) -> bool:
        return self._arm is not None and not self._arm.has_comm_error()

    def activate(self) -> bool:
        return self.write_enable(True)

    def deactivate(self) -> bool:
        return self.write_enable(False)

    # --- description -------------------------------------------------------

    def get_info(self) -> ManipulatorInfo:
        firmware = None
        if self._arm is not None:
            try:
                firmware = str(self._arm.get_firmware())
            except Exception:  # noqa: BLE001 - informational only
                firmware = None
        return ManipulatorInfo(vendor="AgileX", model="NERO", dof=self._dof, firmware_version=firmware)

    def get_dof(self) -> int:
        # joint_nums is what absorbs the 7-vs-6 difference across AgileX arms.
        if self._arm is not None:
            reported = getattr(self._arm, "joint_nums", None)
            if isinstance(reported, int) and reported > 0:
                return reported
        return self._dof

    def get_limits(self) -> JointLimits:
        if self._limits is None:
            self._limits = self._fallback_limits()
        return self._limits

    def _fallback_limits(self) -> JointLimits:
        lower = [math.radians(low) for low, _, _ in _LIMITS_DEG]
        upper = [math.radians(high) for _, high, _ in _LIMITS_DEG]
        speed = [math.radians(spd) for _, _, spd in _LIMITS_DEG]
        return JointLimits(position_lower=lower, position_upper=upper, velocity_max=speed)

    def _read_limits_from_arm(self) -> JointLimits:
        """Prefer the arm's own limits; fall back to the documented table."""
        reader = getattr(self._arm, "get_joint_angle_vel_limits", None)
        if reader is None:
            return self._fallback_limits()
        try:
            lower: list[float] = []
            upper: list[float] = []
            speed: list[float] = []
            for joint in range(1, self.get_dof() + 1):
                entry = reader(joint)
                lower.append(float(entry.min_angle_limit))
                upper.append(float(entry.max_angle_limit))
                speed.append(float(entry.max_joint_spd))
            if len(lower) == self.get_dof():
                return JointLimits(position_lower=lower, position_upper=upper, velocity_max=speed)
        except Exception:  # noqa: BLE001 - hardware may not answer; use the table
            pass
        return self._fallback_limits()

    # --- control mode ------------------------------------------------------

    def set_control_mode(self, mode: ControlMode) -> bool:
        if mode not in (ControlMode.POSITION, ControlMode.SERVO_POSITION, ControlMode.VELOCITY):
            return False
        self._control_mode = mode
        return True

    def get_control_mode(self) -> ControlMode:
        return self._control_mode

    # --- reads -------------------------------------------------------------

    def read_joint_positions(self) -> list[float]:
        if self._arm is None:
            return [0.0] * self.get_dof()
        # get_joint_angles() returns None until the first CAN frame lands, so hold
        # the last good reading rather than raising inside the 100 Hz control loop.
        reading = self._arm.get_joint_angles()
        if reading is None:
            return list(self._last_positions)
        self._last_positions = [float(angle) for angle in reading.msg]
        return list(self._last_positions)

    def read_joint_velocities(self) -> list[float]:
        if self._arm is None:
            return [0.0] * self.get_dof()
        # Feedback is per joint: get_motor_states(i).msg.velocity, in rad/s.
        velocities: list[float] = []
        for joint in range(1, self.get_dof() + 1):
            try:
                state = self._arm.get_motor_states(joint)
                velocities.append(0.0 if state is None else float(state.msg.velocity))
            except Exception:  # noqa: BLE001 - velocity feedback is optional
                velocities.append(0.0)
        return velocities

    def read_joint_efforts(self) -> list[float]:
        # NERO exposes no torque feedback through the documented API.
        return [0.0] * self.get_dof()

    def read_state(self) -> dict[str, int]:
        if self._arm is None:
            return {"connected": 0}
        return {"connected": int(self.is_connected()), "enabled": int(self.read_enabled())}

    def read_error(self) -> tuple[int, str]:
        if self._arm is None:
            return (1, "not connected")
        if self._arm.has_comm_error():
            return (1, str(self._arm.get_comm_error()))
        return (0, "")

    def read_enabled(self) -> bool:
        if self._arm is None:
            return False
        try:
            return all(bool(flag) for flag in self._arm.get_joints_enable_status_list())
        except Exception:  # noqa: BLE001
            return False

    # --- writes ------------------------------------------------------------

    def write_joint_positions(self, positions: list[float], velocity: float = 1.0) -> bool:
        """Command joint positions in radians, clamped to limits."""
        if self._arm is None:
            return False
        limits = self.get_limits()
        clamped = [
            min(max(angle, limits.position_lower[i]), limits.position_upper[i])
            for i, angle in enumerate(positions[: self.get_dof()])
        ]
        self._arm.set_speed_percent(int(max(1, min(100, velocity * 100))))
        self._arm.move_j(clamped)
        return True

    def write_joint_velocities(self, velocities: list[float]) -> bool:
        if self._arm is None:
            return False
        for index, vel in enumerate(velocities[: self.get_dof()], start=1):
            self._arm.move_cpv_vel(index, float(vel))
        return True

    def write_stop(self) -> bool:
        """Controlled stop. For a true e-stop the arm exposes a dedicated call."""
        if self._arm is None:
            return False
        stop = getattr(self._arm, "electronic_emergency_stop", None)
        if stop is not None:
            stop()
            return True
        self._arm.reset()
        return True

    def write_enable(self, enable: bool) -> bool:
        if self._arm is None:
            return False
        # enable()/disable() already carry a 1.5 s timeout and report success, so
        # report it onward instead of assuming the arm complied.
        return bool(self._arm.enable() if enable else self._arm.disable())

    def write_clear_errors(self) -> bool:
        if self._arm is None:
            return False
        self._arm.clear_joint_error()
        return True

    # --- optional capabilities ---------------------------------------------
    # The protocol says: return None / False when unsupported.

    _POSE_KEYS = ("x", "y", "z", "roll", "pitch", "yaw")

    def read_cartesian_position(self) -> dict[str, float] | None:
        """TCP pose in metres / radians (SDK returns [x, y, z, roll, pitch, yaw])."""
        if self._arm is None:
            return None
        try:
            pose = self._arm.get_tcp_pose()
            values = pose.msg if hasattr(pose, "msg") else pose
            return dict(zip(self._POSE_KEYS, (float(v) for v in values), strict=True))
        except Exception:  # noqa: BLE001 - pose feedback is optional
            return None

    def write_cartesian_position(self, pose: dict[str, float], velocity: float = 1.0) -> bool:
        """Command a TCP pose. NERO plans this itself via move_p."""
        if self._arm is None:
            return False
        try:
            target = [float(pose[key]) for key in self._POSE_KEYS]
        except KeyError:
            return False
        self._arm.set_speed_percent(int(max(1, min(100, velocity * 100))))
        self._arm.move_p(target)
        return True

    def read_gripper_position(self) -> float | None:
        """Not wired yet — the AgxGripper is a separate effector object.

        NERO's optional two-finger gripper opens 0–100 mm. Wire it through
        init_effector() before relying on this.
        """
        return None

    def write_gripper_position(self, position: float) -> bool:
        return False

    def read_force_torque(self) -> list[float] | None:
        """NERO has no force/torque sensor."""
        return None
