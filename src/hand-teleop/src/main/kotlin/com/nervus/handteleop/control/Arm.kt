package com.nervus.handteleop.control

import com.nervus.handteleop.camera.Caller
import com.nervus.sdk.component.ControlLease
import com.nervus.sdk.ipc.ControlLeaseException
import com.nervus.sdk.ipc.NervusCallException
import io.github.nervusos.iface.manipulator.v1.ArmState
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
    fun state(): ArmState
    fun servo(target: Pose, velScale: Double)
    fun gripper(opening: Double, effortNm: Double)
    fun stop()
    /** Takes control of the arm; throws [ArmRefusedException] when it can't be had. */
    fun acquire(): ArmControl
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
class ArmLink(private val caller: Caller, private val leaseFactory: () -> ControlLease) : Arm {
    override val label = "NervusOS · Manipulator@1"

    override fun state(): ArmState =
        ArmState.parseFrom(caller.call(ARM, ManipulatorMethod.MANIPULATOR_METHOD_GET_ARM_STATE_VALUE, GetArmStateRequest.getDefaultInstance().toByteArray(), 3))

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
    }
}
