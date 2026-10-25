package com.nervus.handteleop.control

import com.nervus.handteleop.camera.Caller
import com.nervus.handteleop.math.Box
import com.nervus.sdk.component.ControlLease
import com.nervus.sdk.ipc.ControlLeaseException
import com.nervus.sdk.ipc.NervusCallException
import com.nervus.sdk.operation.OperationOutcome
import io.github.nervusos.iface.manipulator.v1.ArmReadiness
import io.github.nervusos.iface.manipulator.v1.ArmState
import io.github.nervusos.iface.manipulator.v1.MoveJointTrajectoryPoint
import io.github.nervusos.iface.manipulator.v1.MoveJointTrajectoryRequest
import io.github.nervusos.iface.manipulator.v1.GetArmStateRequest
import io.github.nervusos.iface.manipulator.v1.ManipulatorMethod
import io.github.nervusos.iface.manipulator.v1.Pose
import io.github.nervusos.iface.manipulator.v1.ServoToPoseRequest
import io.github.nervusos.iface.manipulator.v1.SetGripperRequest
import io.github.nervusos.iface.manipulator.v1.StopRequest

/**
 * The arm as the control loop sees it, in Manipulator@1's terms (metres, quaternions, base_link).
 * Two ways in: through NervusOS ([ArmLink]), or straight to the PiPER driver when the app runs as a
 * plain desktop app ([DriverArm]).
 */
interface Arm {
    /** Where this arm is reached, for the UI. */
    val label: String
    /** The arm's state in words (ready, motors off, teach mode...), as of the last [state] call. */
    val words: String
    fun state(): ArmState
    fun servo(target: Pose, velScale: Double)
    fun gripper(opening: Double, effortNm: Double)
    fun stop()
    /** Takes control of the arm; throws [ArmRefusedException] when it can't be had. */
    fun acquire(): ArmControl

    /** Why slow joint-space moves (the start pose) can't be done on this path; null when they can. */
    val jointMoveUnavailable: String?
    /** Each move asks the user first (NervusOS's confirmation), so it may take a while to begin. */
    val confirmsMoves: Boolean
    /** Starts a slow joint-space move (no IK) to [jointsDeg], degrees; returns at once. Needs control. */
    fun startJointMove(jointsDeg: List<Double>, nowNanos: Long)
    /** Keeps a move going; call every tick while it runs. Throws [ArmRefusedException] if it failed. */
    fun keepJointMove(nowNanos: Long)
    /** The move is over (arrived, stopped or failed): put back whatever [startJointMove] changed. */
    fun endJointMove()

    /** Why the last servo target won't be reached as asked (the driver's planner: partial, unreachable...), or null. */
    val servoNote: String? get() = null
    /** The box this arm clamps servo targets to, if it says; null when unknown. */
    val workspace: Box? get() = null
}

/** NervusApp.callOperation, handed in by the app (it's protected there). */
fun interface OperationCaller {
    fun call(interfaceId: String, methodId: Int, payload: ByteArray, timeoutSeconds: Long): OperationOutcome
}

/** Control of the arm while engaged: a NervusOS ControlLease, or the driver's one-client-at-a-time ownership. */
interface ArmControl {
    /** For the log. */
    val description: String
    /** Changes when control was lost and taken again: commands from before don't count any more. */
    val epoch: Long
    /** Keeps control alive; false if it had lapsed and was taken afresh (a new epoch). */
    fun renew(): Boolean
    fun release()
}

/** The arm refused: someone else has it, it isn't ready, or a guard is holding it. The message says which. */
class ArmRefusedException(message: String) : Exception(message)

/** Manipulator@1 through nervud. Every motion call is checked against this connection's lease. */
class ArmLink(
    private val caller: Caller,
    private val leaseFactory: () -> ControlLease,
    private val operations: OperationCaller? = null,
) : Arm {
    override val label = "NervusOS · Manipulator@1"

    @Volatile
    override var words: String = "-"
        private set

    @Volatile
    private var moveFailure: String? = null

    override fun state(): ArmState {
        val st = try {
            ArmState.parseFrom(caller.call(ARM, ManipulatorMethod.MANIPULATOR_METHOD_GET_ARM_STATE_VALUE, GetArmStateRequest.getDefaultInstance().toByteArray(), 3))
        } catch (e: Exception) {
            words = "arm service not reachable"
            throw e
        }
        // Manipulator@1's readiness is coarser than the driver's view: NOT_READY covers motors off, teach mode
        // and another app holding the arm.
        words = when {
            !st.hasTcpPose() -> "not powered (no pose from the arm)"
            st.readiness == ArmReadiness.ARM_READINESS_READY -> "ready"
            st.readiness == ArmReadiness.ARM_READINESS_FAULT -> "fault: recover it in Robot Arm"
            st.readiness == ArmReadiness.ARM_READINESS_TRANSITIONING -> "moving"
            else -> "not ready (motors off, teach mode, or another app has it)"
        }
        return st
    }

    override val jointMoveUnavailable: String?
        get() = if (operations == null) "joint moves aren't available on this connection" else null

    override val confirmsMoves = true

    /**
     * MoveJointTrajectory to [jointsDeg] at about [MOVE_DEG_S], as one waypoint: piperd interpolates in
     * joint space and waits until the arm is there. It's a long task (an operation that asks the user to
     * confirm), so it runs on its own thread; the control loop watches the joints, and releasing the
     * lease (STOP) makes piperd abandon it.
     */
    override fun startJointMove(jointsDeg: List<Double>, nowNanos: Long) {
        val ops = operations ?: throw ArmRefusedException(jointMoveUnavailable!!)
        val cur = state().joints.positionRadList.map { Math.toDegrees(it) }
        if (cur.size != 6) throw ArmRefusedException("the arm's joints aren't known (is it powered?)")
        val span = cur.indices.maxOf { kotlin.math.abs(jointsDeg[it] - cur[it]) }
        val seconds = maxOf(3.0, span / MOVE_DEG_S)
        val req = MoveJointTrajectoryRequest.newBuilder().addPoints(
            MoveJointTrajectoryPoint.newBuilder()
                .addAllPositionRad(jointsDeg.map { Math.toRadians(it) })
                .setTimeFromStartNanos((seconds * 1e9).toLong()),
        ).build()
        moveFailure = null
        Thread({
            try {
                val out = ops.call(ARM, ManipulatorMethod.MANIPULATOR_METHOD_MOVE_JOINT_TRAJECTORY_VALUE, req.toByteArray(), 180)
                if (!out.isSuccess) moveFailure = "the move ended ${out.state.name.removePrefix("OPERATION_STATE_").lowercase()}"
            } catch (e: Exception) {
                moveFailure = e.message ?: "the move failed"
            }
        }, "start-pose-move").apply { isDaemon = true; start() }
    }

    override fun keepJointMove(nowNanos: Long) {
        moveFailure?.let { throw ArmRefusedException(it) }
    }

    override fun endJointMove() {
        moveFailure = null
    }

    override fun servo(target: Pose, velScale: Double) {
        refused {
            caller.call(ARM, ManipulatorMethod.MANIPULATOR_METHOD_SERVO_TO_POSE_VALUE,
                ServoToPoseRequest.newBuilder().setTarget(target).setMaxVelScale(velScale).build().toByteArray(), 2)
        }
    }

    override fun gripper(opening: Double, effortNm: Double) {
        refused {
            caller.call(ARM, ManipulatorMethod.MANIPULATOR_METHOD_SET_GRIPPER_VALUE,
                SetGripperRequest.newBuilder().setOpening(opening).setMaxEffort(effortNm).build().toByteArray(), 2)
        }
    }

    override fun stop() {
        caller.call(ARM, ManipulatorMethod.MANIPULATOR_METHOD_STOP_VALUE, StopRequest.getDefaultInstance().toByteArray(), 2)
    }

    override fun acquire(): ArmControl {
        val lease = try {
            leaseFactory()
        } catch (e: Exception) {
            val c = e.cause ?: e
            if (c is ControlLeaseException) throw ArmRefusedException(c.message ?: "control lease refused")
            throw e
        }
        return object : ArmControl {
            override val description get() = "control lease ${lease.leaseId} on ${lease.resourceHandle}"
            override val epoch get() = lease.motionEpoch
            override fun renew() = lease.renew()
            override fun release() = lease.release()
        }
    }

    // The kernel's typed refusal (no lease, lapsed epoch, not ready) becomes an ArmRefusedException.
    private inline fun refused(block: () -> Unit) {
        try {
            block()
        } catch (e: NervusCallException) {
            throw ArmRefusedException(e.code.name.removePrefix("STATUS_CODE_").lowercase())
        }
    }

    companion object {
        const val ARM = "nervus.interface.manipulator.arm"
        /** Joint speed for the start-pose move: slow enough to watch, about what the driver does at 12 %. */
        const val MOVE_DEG_S = 10.0
    }
}
