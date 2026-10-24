package com.nervus.handteleop.control

import com.nervus.handteleop.math.Box
import com.nervus.handteleop.math.HandMath
import com.nervus.handteleop.math.Mat3
import com.nervus.handteleop.math.OutlierGate
import com.nervus.handteleop.math.Quat
import com.nervus.handteleop.math.RollShaper
import com.nervus.handteleop.math.SlipAnchor
import com.nervus.handteleop.math.Vec3
import com.nervus.handteleop.vision.HandLandmarks
import com.nervus.handteleop.vision.HandMetrics
import com.nervus.handteleop.vision.Observation
import com.nervus.handteleop.vision.OneEuro
import com.nervus.handteleop.vision.OneEuro3
import io.github.nervusos.iface.manipulator.v1.ArmState
import io.github.nervusos.iface.manipulator.v1.Pose
import java.util.logging.Logger

enum class PinchMode(val label: String) {
    THUMB_INDEX("Thumb + index"),
    THUMB_FINGERS("Thumb + four fingers"),
}

/**
 * Whether the tool turns about its own axis with the wrist. ON maps a right-handed turn of the hand
 * about its fingers to a right-handed turn of the tool about its own z (the approach axis), which
 * matches when your fingers point the way the gripper does; INVERTED is for setups where it doesn't.
 */
enum class RollMode(val label: String, val sign: Double) {
    OFF("Off", 0.0),
    ON("On", 1.0),
    INVERTED("Inverted", -1.0),
}

/** Everything the operator can tune. Read by the control loop every tick. */
class TeleopSettings {
    @Volatile var hand: HandLandmarks.Side = HandLandmarks.Side.RIGHT
    @Volatile var pinchMode: PinchMode = PinchMode.THUMB_INDEX
    /**
     * Metres of arm travel per metre of hand travel (dimensionless). The hand is measured in metres in
     * front of the camera: sideways, up and down, and toward or away from it (toward reaches forward).
     */
    @Volatile var gainSide: Double = 1.2
    @Volatile var gainUp: Double = 1.2
    @Volatile var gainReach: Double = 1.0
    @Volatile var speed: Double = 0.6
    @Volatile var invertSide: Boolean = false
    /**
     * Show the video mirrored, like a selfie view. Off by default: the owner prefers the picture as the camera sees it.
     * Display only: tracking always works on the frame as the camera saw it, so the body pose keeps
     * your real left and right.
     */
    @Volatile var mirrorImage: Boolean = false
    /**
     * The camera's focal length over the frame width (0.7 is a laptop webcam about 70 degrees wide). It
     * sets the scale of everything metric: the hand's distance, and how far a hand movement is.
     */
    @Volatile var focalFraction: Double = 0.7
    /** Wrist roll turns the tool about its own axis; the other two wrist angles stay as engaged. */
    @Volatile var rollMode: RollMode = RollMode.ON
    @Volatile var gripperEffortNm: Double = 1.0
    /**
     * Pinch ratio at which the gripper is fully closed / fully open. The ratio is a 3-D distance over the
     * wrist-to-middle-knuckle length (about 9 cm on an adult hand). Fingertips touching read about
     * 0.1 to 0.2 (the landmarks sit inside the finger pads), a relaxed open pinch (8 to 9 cm) about 1.0,
     * a fully spread thumb 1.5 or more. So 0.25 closes fully with margin for landmark noise, and 1.0
     * opens fully without straining the hand.
     */
    @Volatile var closedRatio: Double = 0.25
    @Volatile var openRatio: Double = 1.0
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
    /** Estimated distance from the camera to the operator's hand, metres. */
    val handDistance: Double? = null,
    /** How far the wrist has rolled since the anchor (null when not engaged), and the roll sent to the tool; degrees. */
    val handRollDeg: Double? = null,
    val toolRollDeg: Double = 0.0,
    val rollMode: RollMode = RollMode.ON,
) {
    enum class Phase { IDLE, ENGAGED, HOLD, ERROR }
}

/**
 * Maps the operator's hand to the arm.
 *
 * Engage is a clutch: at the moment you engage (and again whenever a lost hand comes back) the
 * current hand position is anchored to the arm's current pose, and from then on the arm follows the
 * hand's movement relative to that anchor. The hand is placed in metres in front of the camera: its
 * distance comes from the palm's bones (pixels against the model's metric world landmarks), so tilting
 * the palm doesn't read as moving toward or away. Moving the hand left/right moves the arm sideways,
 * up/down moves it up/down, and toward the camera reaches forward. Rolling the wrist turns the tool
 * about its own axis; its other two angles stay as they were at the anchor. The gripper follows the
 * pinch. At the edge of the workspace the anchor slips along, so the arm comes back as soon as the
 * hand does.
 *
 * Safety: a HUMAN ControlLease is held only while engaged, the kernel refuses commands without it,
 * every command feeds the lease's 300 ms deadman, and the arm holds still the moment the hand is lost.
 * Frames where tracking jumps are dropped; three in a row count as a lost hand.
 */
class Teleop(
    private val arm: Arm,
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
    private var lease: ArmControl? = null
    private var anchor: Anchor? = null
    private var meas: Measurement? = null
    private var lastObsAt = 0L
    private var lastTickAt = 0L
    private var lastRenew = 0L
    private var lastGripSent = -1.0
    private var lastGripAt = 0L
    private var lastHandAt = 0L
    private var holding = false
    private var note: String? = null
    private var noteUntil = 0L
    private var servoCount = 0
    private var servoWindow = System.nanoTime()
    private var lastState: ArmState? = null
    private var lastStateAt = 0L
    private val fx = OneEuro(1.0, 0.015)
    private val fy = OneEuro(1.0, 0.015)
    // Depth is a ratio of small lengths and the noisiest of the three, so it is smoothed harder.
    private val fz = OneEuro(0.5, 3.0)
    private val fp = OneEuro(2.0, 0.05)
    // The five palm points the roll is built from: world landmarks jitter by millimetres, which over a
    // palm 8 cm wide is degrees.
    private val fw = Array(HandMath.PALM_POINTS.size) { OneEuro3(0.4, 0.1) }
    private val gate = OutlierGate()
    private val roll = RollShaper()

    /** One accepted vision frame, filtered: the palm centre in camera metres, the palm frame, the pinch. */
    private class Measurement(val pos: Vec3, val palm: Mat3?, val pinch: Double, val bonesPx: Double)

    /** The hand and the arm as they were at the anchor. */
    private class Anchor(val hand: Measurement, val tcp: Pose, val base: SlipAnchor) {
        /** The arm's orientation, which roll turns from; null if the Provider reports none. */
        val q0: Quat? = Quat(tcp.qx, tcp.qy, tcp.qz, tcp.qw).unit()
    }

    private enum class Frame { ACCEPTED, EDGE_ON, DROPPED, RESET, UNMEASURABLE }

    fun engage() { engageRequested = true }
    fun disengage() { disengageRequested = true }

    /** Emergency: stop the arm now and give up control. Callable from any thread. */
    fun stopNow() {
        disengageRequested = true
        runCatching { arm.stop() }
    }

    /** One control tick (call at ~20 Hz with the newest observation). */
    fun tick(obs: Observation?, nowNanos: Long) {
        val dt = if (lastTickAt == 0L) 0.05 else ((nowNanos - lastTickAt) / 1e9).coerceIn(0.0, 0.2)
        lastTickAt = nowNanos
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
        val focal = settings.focalFraction * frameW

        if (engageRequested && lease == null) {
            engageRequested = false
            if (metrics == null) {
                status = status.copy(phase = TeleopStatus.Phase.IDLE, message = "Show your ${settings.hand.name.lowercase()} hand to the camera first")
                return
            }
            try {
                val c = arm.acquire()
                lease = c
                lastRenew = nowNanos
                log.info("${c.description}, epoch ${c.epoch}")
            } catch (e: Exception) {
                status = status.copy(phase = TeleopStatus.Phase.ERROR, message = leaseText(e), lastError = e.message)
                return
            }
            anchor = null
            meas = null
            lastObsAt = 0
            resetFilters()
        }
        val l = lease
        if (l == null) {
            status = status.copy(
                phase = TeleopStatus.Phase.IDLE,
                pinch = metrics?.let { pinchOf(it) } ?: 0.0,
                handDistance = metrics?.depth(focal),
                handRollDeg = null,
                toolRollDeg = 0.0,
                rollMode = settings.rollMode,
                message = if (metrics == null) "Raise your ${settings.hand.name.lowercase()} hand into view" else "Press ENGAGE (Space) to take control",
            )
            return
        }

        // Hand lost: hold still, keep the lease only briefly.
        if (obs == null || hand == null || metrics == null) {
            if (!holding && nowNanos - lastHandAt > 300_000_000L) hold()
            status = status.copy(phase = TeleopStatus.Phase.HOLD, message = "Hand lost: arm holding. Show your hand to continue")
            return
        }
        lastHandAt = nowNanos

        // Keep the lease alive while someone is actually steering.
        if (nowNanos - lastRenew > 500_000_000L) {
            try {
                if (!l.renew()) {
                    // A new epoch: the arm may have stopped short of the last target, so start again from where it is.
                    log.info("control replaced (new epoch ${l.epoch}); re-anchoring")
                    anchor = null
                }
                lastRenew = nowNanos
            } catch (e: Exception) {
                release(leaseText(e))
                return
            }
        }

        // Each vision frame is measured once. The ticks in between send the same target again, which
        // keeps the stream (and the lease's deadman) going while the arm holds there.
        if (obs.atNanos != lastObsAt) {
            lastObsAt = obs.atNanos
            when (measure(hand, metrics, frameW, frameH, focal, obs.atNanos)) {
                Frame.ACCEPTED -> {}
                Frame.EDGE_ON -> flag("Palm edge-on to the camera: holding the last position", nowNanos)
                Frame.DROPPED -> flag("Tracking jumped: frame skipped", nowNanos)
                Frame.UNMEASURABLE -> flag("Hand too small to measure: holding the last position", nowNanos)
                Frame.RESET -> {
                    // Three jumps in a row: treat it as a lost hand that is back somewhere else.
                    hold()
                    flag("Tracking jumped: re-anchored", nowNanos)
                    status = status.copy(phase = TeleopStatus.Phase.HOLD, message = "Tracking jumped: arm holding, re-anchoring")
                    return
                }
            }
        }
        val m = meas
        if (m == null) {
            status = status.copy(phase = TeleopStatus.Phase.HOLD, message = "Hold your hand still to anchor")
            return
        }

        // (Re-)anchor on the arm's current pose so the arm never jumps.
        if (anchor == null) {
            val tcp = lastState?.takeIf { it.hasTcpPose() && it.tcpPose.frame.isNotEmpty() }?.tcpPose
            if (tcp == null) {
                status = status.copy(phase = TeleopStatus.Phase.HOLD, message = "Waiting for the arm's pose (is the PiPER powered?)")
                return
            }
            val at = Vec3(tcp.x, tcp.y, tcp.z)
            if (at !in WORKSPACE) {
                // Folded at rest, or somewhere teleop can't start from: dragging the target into the box
                // from there makes the arm look for an IK solution it often can't find.
                release("The arm is outside the teleop workspace (folded?). Open Robot Arm, press HOME (ready pose), then engage again")
                return
            }
            anchor = Anchor(m, tcp, SlipAnchor(at, WORKSPACE))
            holding = false
            roll.reset()
        }
        val a = anchor!!

        // Camera axes to base_link: the camera faces the operator, so the hand coming toward it (z
        // falling) reaches forward (+x), and image down (+y) is down (-z).
        val d = m.pos - a.hand.pos
        val side = settings.gainSide * d.x * (if (settings.invertSide) -1.0 else 1.0)
        val up = -settings.gainUp * d.y
        val reach = -settings.gainReach * d.z
        // Pushed past the edge of the workspace, the anchor slides along, so turning back moves the arm at once.
        val p = a.base.target(Vec3(reach, side, up))

        // Roll only (the paper's choice): the twist about the anchor's finger axis. Pitch and yaw of the
        // hand would need the arm's wrist to follow through poses IK often can't reach from a webcam's view.
        val handRoll = if (a.hand.palm != null && m.palm != null) HandMath.roll(a.hand.palm, m.palm) else null
        val toolRoll = if (handRoll == null) roll.value else roll.step(handRoll * settings.rollMode.sign, dt)
        val target = Pose.newBuilder(a.tcp).setX(p.x).setY(p.y).setZ(p.z).apply {
            a.q0?.let { q0 ->
                // q0 * Rz: multiplied on the right, the turn is about the tool's own z axis, whichever way the tool points.
                val q = q0 * Quat.about(Vec3.Z, toolRoll)
                setQx(q.x).setQy(q.y).setQz(q.z).setQw(q.w)
            }
        }.build()

        var err: String? = null
        try {
            arm.servo(target, settings.speed)
            servoCount++
        } catch (e: ArmRefusedException) {
            // The lease may have lapsed (deadman, pre-empted), or the driver is holding the arm (busy, IK
            // guard): show why, renew next tick, then re-anchor on wherever the arm really is.
            err = "servo refused: ${e.message}"
            lastRenew = 0
            anchor = null
        } catch (e: Exception) {
            err = e.message
        }

        val opening = ((m.pinch - settings.closedRatio) / (settings.openRatio - settings.closedRatio)).coerceIn(0.0, 1.0)
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
            message = err ?: note?.takeIf { nowNanos < noteUntil } ?: "Engaged: the arm follows your ${settings.hand.name.lowercase()} hand",
            target = target,
            gripper = opening,
            pinch = m.pinch,
            leaseEpoch = l.epoch,
            lastError = err ?: status.lastError,
            handDistance = m.pos.z,
            handRollDeg = handRoll?.let { Math.toDegrees(it) },
            toolRollDeg = Math.toDegrees(toolRoll),
            rollMode = settings.rollMode,
        )
    }

    /** Gates one new vision frame and, if it passes, filters it into [meas]. */
    private fun measure(h: HandLandmarks, m: HandMetrics, frameW: Int, frameH: Int, focal: Double, t: Long): Frame {
        val z = m.depth(focal) ?: return Frame.UNMEASURABLE
        val a = anchor
        // Nearly edge-on (fingers toward the camera), both bone sums shrink toward nothing and their
        // ratio, the depth, is mostly noise.
        if (a != null && m.bonesPx < EDGE_ON * a.hand.bonesPx) return Frame.EDGE_ON
        val cx = frameW / 2.0
        val cy = frameH / 2.0
        val left = h.side == HandLandmarks.Side.LEFT
        // Raw values go to the gate before any filter sees them, so a glitch never gets into the filters.
        val raw = HandMath.backProject(m.palmX.toDouble(), m.palmY.toDouble(), z, focal, cx, cy)
        val rawPalm = if (settings.rollMode != RollMode.OFF) HandMath.palmFrame(h.world, left) else null
        when (gate.check(raw, rawPalm)) {
            OutlierGate.Verdict.ACCEPT -> {}
            OutlierGate.Verdict.DROP -> return Frame.DROPPED
            OutlierGate.Verdict.RESET -> return Frame.RESET
        }
        val u = fx.filter(m.palmX.toDouble(), t)
        val v = fy.filter(m.palmY.toDouble(), t)
        val zf = fz.filter(z, t)
        val pts = HandMath.PALM_POINTS.mapIndexed { k, i -> fw[k].filter(h.world[i], t) }
        val palm = HandMath.palmFrame(pts[0], pts[1], pts[2], pts[3], pts[4], left) ?: meas?.palm
        meas = Measurement(HandMath.backProject(u, v, zf, focal, cx, cy), palm, fp.filter(pinchOf(m), t), m.bonesPx)
        return Frame.ACCEPTED
    }

    private fun pinchOf(m: HandMetrics): Double =
        (if (settings.pinchMode == PinchMode.THUMB_INDEX) m.pinchIndex else m.pinchFour).toDouble()

    /** Hand lost, or tracking unreliable: stop where the arm is, and anchor afresh when the hand is back. */
    private fun hold() {
        holding = true
        anchor = null
        meas = null
        runCatching { arm.stop() }
        resetFilters()
    }

    private fun resetFilters() {
        fx.reset(); fy.reset(); fz.reset(); fp.reset()
        fw.forEach { it.reset() }
        gate.reset()
        roll.reset()
    }

    /** Shows [text] in place of the usual message for a second, so a one-frame event can be read. */
    private fun flag(text: String, nowNanos: Long) {
        note = text
        noteUntil = nowNanos + 1_000_000_000L
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
        meas = null
        holding = false
        note = null
        resetFilters()
        if (l != null) {
            runCatching { arm.stop() }
            runCatching { l.release() }
        }
        lastGripSent = -1.0
        status = status.copy(phase = TeleopStatus.Phase.IDLE, message = message, target = null, handRollDeg = null, toolRollDeg = 0.0)
    }

    private fun leaseText(e: Throwable): String = when (e) {
        is ArmRefusedException -> "Arm not available: ${e.message}"
        else -> "Could not take control of the arm: ${(e.cause ?: e).message}"
    }

    fun shutdown() = release("Closed")

    companion object {
        // Metres in base_link, a little inside the driver's own box.
        val X_RANGE = 0.08..0.46
        val Y_RANGE = -0.30..0.30
        val Z_RANGE = 0.10..0.55
        val WORKSPACE = Box(X_RANGE, Y_RANGE, Z_RANGE)

        /** A frame whose palm bones look shorter than this fraction of the anchor's is taken as edge-on. */
        const val EDGE_ON = 0.4
    }
}
