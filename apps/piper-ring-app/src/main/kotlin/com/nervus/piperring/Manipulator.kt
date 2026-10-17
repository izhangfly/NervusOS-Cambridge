package com.nervus.piperring

import com.nervus.sdk.annotations.Interface
import com.nervus.sdk.annotations.Method
import io.github.nervusos.iface.manipulator.v1.ArmState
import io.github.nervusos.iface.manipulator.v1.GetArmStateRequest
import io.github.nervusos.iface.manipulator.v1.GoHomeRequest
import io.github.nervusos.iface.manipulator.v1.ManipulatorMoveResult
import io.github.nervusos.iface.manipulator.v1.MoveJointTrajectoryRequest
import io.github.nervusos.iface.manipulator.v1.MoveToPoseRequest
import io.github.nervusos.iface.manipulator.v1.SetGripperRequest
import io.github.nervusos.iface.manipulator.v1.StopRequest

/**
 * Kotlin binding for the standard NervusOS interface **Manipulator@1**
 * (interface_id = `nervus.interface.manipulator.arm`). Each `@Method(id=…)` matches
 * `ManipulatorMethod` in manipulator.proto; the SDK's `InterfaceProxy` marshals the
 * single protobuf request and parses the protobuf response over Port A → nervud.
 *
 * Units (from the proto): Cartesian pos = m, quaternion dimensionless, joints = rad.
 * MoveToPose / MoveJointTrajectory / GoHome are long-running *operations*; SetGripper
 * / Stop are short commands.
 *
 * !! VERIFY on real nervud:
 *   - InterfaceProxy's exact arg/return marshalling (proto `.toByteArray()` in, parse out).
 *   - SetGripper's real response_type (used ManipulatorMoveResult as a placeholder ack).
 */
@Interface(id = "nervus.interface.manipulator.arm")
interface Manipulator {
    @Method(id = 1) suspend fun getArmState(req: GetArmStateRequest): ArmState
    @Method(id = 3) suspend fun moveToPose(req: MoveToPoseRequest): ManipulatorMoveResult
    @Method(id = 4) suspend fun moveJointTrajectory(req: MoveJointTrajectoryRequest): ManipulatorMoveResult
    @Method(id = 5) suspend fun goHome(req: GoHomeRequest): ManipulatorMoveResult
    @Method(id = 6) suspend fun setGripper(req: SetGripperRequest): ManipulatorMoveResult
    @Method(id = 7) suspend fun stop(req: StopRequest): ManipulatorMoveResult
}
