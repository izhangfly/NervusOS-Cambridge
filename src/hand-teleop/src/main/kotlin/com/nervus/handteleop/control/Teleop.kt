package com.nervus.handteleop.control

import com.nervus.handteleop.camera.Caller
import com.nervus.handteleop.vision.HandLandmarks
import com.nervus.handteleop.vision.HandMetrics
import com.nervus.handteleop.vision.Observation
import com.nervus.handteleop.vision.OneEuro
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
import java.util.logging.Logger

/** Manipulator@1 through nervud. Every motion call is checked against this connection's lease. */
class ArmLink(private val caller: Caller) {
    fun state(): ArmState =
        ArmState.parseFrom(caller.call(ARM, ManipulatorMethod.MANIPULATOR_METHOD_GET_ARM_STATE_VALUE, GetArmStateRequest.getDefaultInstance().toByteArray(), 3))

    fun servo(target: Pose, velScale: Double) {
        caller.call(ARM, ManipulatorMethod.MANIPULATOR_METHOD_SERVO_TO_POSE_VALUE,
            ServoToPoseRequest.newBuilder().setTarget(target).setMaxVelScale(velScale).build().toByteArray(), 2)
    }

    fun gripper(opening: Double, effortNm: Double) {
        caller.call(ARM, ManipulatorMethod.MANIPULATOR_METHOD_SET_GRIPPER_VALUE,
            SetGripperRequest.newBuilder().setOpening(opening).setMaxEffort(effortNm).build().toByteArray(), 2)
    }

    fun stop() {
        caller.call(ARM, ManipulatorMethod.MANIPULATOR_METHOD_STOP_VALUE, StopRequest.getDefaultInstance().toByteArray(), 2)
    }

    companion object {
        const val ARM = "nervus.interface.manipulator.arm"
    }
}

enum class PinchMode(val label: String) {
    THUMB_INDEX("Thumb + index"),
    THUMB_FINGERS("Thumb + four fingers"),
}

/** Everything the operator can tune. Read by the control loop every tick. */
class TeleopSettings {
    @Volatile var hand: HandLandmarks.Side = HandLandmarks.Side.RIGHT
    @Volatile var pinchMode: PinchMode = PinchMode.THUMB_INDEX
    /** Metres of arm travel for a hand movement across the whole frame width / height. */
    @Volatile var gainSide: Double = 0.45
    @Volatile var gainUp: Double = 0.40
    /** Metres of reach per +100 % change in apparent hand size (moving toward the camera). */
    @Volatile var gainReach: Double = 0.30
    @Volatile var speed: Double = 0.6
    @Volatile var invertSide: Boolean = false
    @Volatile var gripperEffortNm: Double = 1.0
    /** Pinch ratio (distance / palm size) at which the gripper is fully closed / fully open. */
    @Volatile var closedRatio: Double = 0.30
    @Volatile var openRatio: Double = 1.10
}

/** What the UI shows about the control loop. */
data class TeleopStatus(
    val phase: Phase = Phase.IDLE,
    val message: String = "Raise your hand and press ENGAGE",
    val armReadiness: String = "-",
    val tcp: Pose? = null,
    val target: Pose? = null,
    val gripper: Double = 0.0,
    val pinch: Double = 0.0,
    val leaseEpoch: Long = 0,
    val servoHz: Float = 0f,
    val lastError: String? = null,
) {
    enum class Phase { IDLE, ENGAGED, HOLD, ERROR }
}

/**
 * Maps the operator's hand to the arm.
 *
 * Engage is a clutch: at the moment you engage (and again whenever a lost hand comes back) the
 * current hand position is anchored to the arm's current pose, and from then on the arm follows the
 * hand's movement relative to that anchor. Moving the hand left/right moves the arm sideways, up/down
 * moves it up/down, and bringing the hand toward the camera (it looks bigger) reaches forward. The
 * gripper follows the pinch. Orientation stays as it was when you engaged.
 *
 * Safety: a HUMAN ControlLease is held only while engaged, the kernel refuses commands without it,
 * every command feeds the lease's 300 ms deadman, and the arm holds still the moment the hand is lost.
 */
class Teleop(
    private val arm: ArmLink,
    private val leaseFactory: () -> ControlLease,
    private val settings: TeleopSettings,
    private val log: Logger,
) {
    @Volatile
    var status = TeleopStatus()
        private set

    @Volatile
    private var engageRequested = false
    @Volatile
    private var disengageRequested = false
    private var lease: ControlLease? = null
    private var anchor: Anchor? = null
    private var lastRenew = 0L
    private var lastGripSent = -1.0
    private var lastGripAt = 0L
    private var lastHandAt = 0L
    private var holding = false
    private var servoCount = 0
    private var servoWindow = System.nanoTime()
    private var lastState: ArmState? = null
    private var lastStateAt = 0L
    private val fx = OneEuro(1.0, 0.015)
    private val fy = OneEuro(1.0, 0.015)
    private val fs = OneEuro(0.8, 0.01)
    private val fp = OneEuro(2.0, 0.05)

    private class Anchor(val palmX: Double, val palmY: Double, val size: Double, val tcp: Pose)

    fun engage() { engageRequested = true }
    fun disengage() { disengageRequested = true }

    /** Emergency: stop the arm now and give up control. Callable from any thread. */
    fun stopNow() {
        disengageRequested = true
        runCatching { arm.stop() }
    }

    /** One control tick (call at ~20 Hz with the newest observation). */
    fun tick(obs: Observation?, nowNanos: Long) {
        refreshState(nowNanos)
        if (disengageRequested) {
            disengageRequested = false
            engageRequested = false
            release("Disengaged")
            return
        }
        val hand = obs?.hands?.firstOrNull { it.side == settings.hand }
        val metrics = hand?.let { HandMetrics.of(it) }
        val frameW = obs?.frame?.width ?: 640
        val frameH = obs?.frame?.height ?: 480
        val pinchRaw = metrics?.let { if (settings.pinchMode == PinchMode.THUMB_INDEX) it.pinchIndex else it.pinchFour }?.toDouble()

        if (engageRequested && lease == null) {
            engageRequested = false
            if (metrics == null) {
                status = status.copy(phase = TeleopStatus.Phase.IDLE, message = "Show your ${settings.hand.name.lowercase()} hand to the camera first")
                return
            }
            try {
                lease = leaseFactory()
                lastRenew = nowNanos
                log.info("control lease ${lease!!.leaseId} on ${lease!!.resourceHandle}, epoch ${lease!!.motionEpoch}")
            } catch (e: Exception) {
                status = status.copy(phase = TeleopStatus.Phase.ERROR, message = leaseText(e), lastError = e.message)
                return
            }
            anchor = null
        }
        val l = lease
        if (l == null) {
            status = status.copy(
                phase = TeleopStatus.Phase.IDLE,
                pinch = pinchRaw ?: 0.0,
                message = if (metrics == null) "Raise your ${settings.hand.name.lowercase()} hand into view" else "Press ENGAGE (Space) to take control",
            )
            return
        }

        // Hand lost: hold still, keep the lease only briefly.
        if (metrics == null) {
            if (!holding && nowNanos - lastHandAt > 300_000_000L) {
                holding = true
                anchor = null
                runCatching { arm.stop() }
                fx.reset(); fy.reset(); fs.reset(); fp.reset()
            }
            status = status.copy(phase = TeleopStatus.Phase.HOLD, message = "Hand lost: arm holding. Show your hand to continue")
            return
        }
        lastHandAt = nowNanos

        // Keep the lease alive while someone is actually steering.
        if (nowNanos - lastRenew > 500_000_000L) {
            try {
                if (!l.renew()) log.info("lease replaced (new epoch ${l.motionEpoch}); re-anchoring")
                lastRenew = nowNanos
            } catch (e: Exception) {
                release(leaseText(e))
                return
            }
        }

        val px = fx.filter(metrics.palmX.toDouble(), nowNanos)
        val py = fy.filter(metrics.palmY.toDouble(), nowNanos)
        val ps = fs.filter(metrics.palmSize.toDouble(), nowNanos)
        val pinch = fp.filter(pinchRaw!!, nowNanos)

        // (Re-)anchor on the arm's current pose so the arm never jumps.
        if (anchor == null || holding) {
            val tcp = lastState?.takeIf { it.hasTcpPose() && it.tcpPose.frame.isNotEmpty() }?.tcpPose
            if (tcp == null) {
                status = status.copy(phase = TeleopStatus.Phase.HOLD, message = "Waiting for the arm's pose (is the PiPER powered?)")
                return
            }
            if (tcp.x !in X_RANGE || tcp.y !in Y_RANGE || tcp.z !in Z_RANGE) {
                // Folded at rest, or somewhere teleop can't start from: dragging the target into the box
                // from there makes the arm look for an IK solution it often can't find.
                release("The arm is outside the teleop workspace (folded?). Open Robot Arm, press HOME (ready pose), then engage again")
                return
            }
            anchor = Anchor(px, py, ps, tcp)
            holding = false
        }
        val a = anchor!!
        val side = (px - a.palmX) / frameW * settings.gainSide * (if (settings.invertSide) -1 else 1)
        val up = -(py - a.palmY) / frameH * settings.gainUp
        val reach = (ps / a.size - 1.0) * settings.gainReach
        val target = Pose.newBuilder(a.tcp)
            .setX((a.tcp.x + reach).coerceIn(X_RANGE))
            .setY((a.tcp.y + side).coerceIn(Y_RANGE))
            .setZ((a.tcp.z + up).coerceIn(Z_RANGE))
            .build()

        var err: String? = null
        try {
            arm.servo(target, settings.speed)
            servoCount++
        } catch (e: NervusCallException) {
            err = "servo refused: ${e.code.name.removePrefix("STATUS_CODE_").lowercase()}"
            // The lease may have lapsed (deadman, pre-empted): renew next tick, then re-anchor.
            lastRenew = 0
            anchor = null
        } catch (e: Exception) {
            err = e.message
        }

        val opening = ((pinch - settings.closedRatio) / (settings.openRatio - settings.closedRatio)).coerceIn(0.0, 1.0)
        val gripStep = kotlin.math.abs(opening - lastGripSent)
        if (gripStep > 0.04 || (gripStep > 0.005 && (opening == 0.0 || opening == 1.0)) || lastGripSent < 0) {
            if (nowNanos - lastGripAt > 100_000_000L) {
                runCatching { arm.gripper(opening, settings.gripperEffortNm) }
                    .onSuccess { lastGripSent = opening; lastGripAt = nowNanos }
                    .onFailure { if (err == null) err = "gripper: ${it.message}" }
            }
        }

        if (nowNanos - servoWindow > 1_000_000_000L) {
            val hz = servoCount * 1e9f / (nowNanos - servoWindow)
            servoCount = 0
            servoWindow = nowNanos
            status = status.copy(servoHz = hz)
        }
        status = status.copy(
            phase = if (err == null) TeleopStatus.Phase.ENGAGED else TeleopStatus.Phase.ERROR,
            message = err ?: "Engaged: the arm follows your ${settings.hand.name.lowercase()} hand",
            target = target,
            gripper = opening,
            pinch = pinch,
            leaseEpoch = l.motionEpoch,
            lastError = err ?: status.lastError,
        )
    }

    private fun refreshState(nowNanos: Long) {
        if (nowNanos - lastStateAt < 200_000_000L) return
        lastStateAt = nowNanos
        try {
            val st = arm.state()
            lastState = st
            status = status.copy(
                armReadiness = st.readiness.name.removePrefix("ARM_READINESS_"),
                tcp = if (st.hasTcpPose()) st.tcpPose else null,
            )
        } catch (e: Exception) {
            status = status.copy(armReadiness = "UNREACHABLE")
        }
    }

    private fun release(message: String) {
        val l = lease
        lease = null
        anchor = null
        holding = false
        if (l != null) {
            runCatching { arm.stop() }
            runCatching { l.release() }
        }
        lastGripSent = -1.0
        status = status.copy(phase = TeleopStatus.Phase.IDLE, message = message, target = null)
    }

    private fun leaseText(e: Throwable): String = when (val c = e.cause ?: e) {
        is ControlLeaseException -> "Arm not available: ${c.message}"
        else -> "Could not take control of the arm: ${c.message}"
    }

    fun shutdown() = release("Closed")

    companion object {
        // Metres in base_link, a little inside the driver's own box.
        val X_RANGE = 0.08..0.46
        val Y_RANGE = -0.30..0.30
        val Z_RANGE = 0.10..0.55
    }
}
