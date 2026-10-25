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
    /**
     * Hands-free engage: a fist held 1.5 s engages, and disengages when engaged; while engaged a fist is
     * a clutch (the arm holds, and the hand is anchored afresh when it opens).
     */
    @Volatile var fistGesture: Boolean = true
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
    /** The arm's state in words: ready, motors off, teach mode, controlled by another app... */
    val armWords: String = "-",
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
    /** The start-pose move's progress, 0..1, while [Phase.MOVING]. */
    val moveProgress: Double? = null,
    /** How far through a held fist, 0..1 (0 when none), and whether it will engage (else disengage). */
    val gestureProgress: Double = 0.0,
    val gestureEngages: Boolean = true,
) {
    /**
     * IDLE: not in control. OUTSIDE: engage was asked for, but the arm is outside the workspace; the UI
     * offers the start pose. MOVING: going to the start pose. ENGAGED / HOLD: in control, following or
     * holding. ERROR: in control (or trying), but the last command was refused.
     */
    enum class Phase { IDLE, OUTSIDE, MOVING, ENGAGED, HOLD, ERROR }
}

/**
 * Maps the operator's hand to the arm. SPEC.md next to this app is the definition of what it does.
 *
 * Engage is a clutch: at the moment you engage (and again whenever a lost hand comes back) the
 * current hand position is anchored to the arm's current pose, and from then on the arm follows the
 * hand's movement relative to that anchor. The hand is placed in metres in front of the camera: its
 * distance comes from the palm's bones (pixels against the model's metric world landmarks), so tilting
 * the palm doesn't read as moving toward or away. Moving the hand left/right moves the arm sideways,
 * up/down moves it up/down, and toward the camera reaches forward. Rolling the wrist turns the tool
 * about its own axis; its other two angles stay as they were at the anchor. The gripper follows the
 * pinch. At the edge of the workspace the anchor slips along, so the arm comes back as soon as the
 * hand does. A fist held 1.5 s engages or disengages; a shorter fist is a clutch.
 *
 * Teleop can only start inside the workspace. Outside it (the PiPER at rest is folded below it), the
 * start pose is a slow joint-space move there, offered instead of a refusal.
 *
 * Safety: control (a HUMAN ControlLease, or the driver's ownership) is held only while engaged or
 * moving, every command feeds a deadman, and the arm holds still the moment the hand is lost. Frames
 * where tracking jumps are dropped; three in a row count as a lost hand.
 */
class Teleop(
    private val arm: Arm,
    private val settings: TeleopSettings,
    private val log: Logger,
    val startPose: StartPoseStore = StartPoseStore(null, log),
) {
    @Volatile
    var status = TeleopStatus()
        private set

    @Volatile
    private var engageRequested = false
    @Volatile
    private var disengageRequested = false
    @Volatile
    private var moveRequested: Boolean? = null // engage after the move?
    @Volatile
    private var cancelRequested = false
    private var lease: ArmControl? = null
    private var anchor: Anchor? = null
    private var meas: Measurement? = null
    private var move: Move? = null
    private var lastObsAt = 0L
    private var lastTickAt = 0L
    private var lastRenew = 0L
    private var lastGripSent = -1.0
    private var lastGripAt = 0L
    private var lastHandAt = 0L
    private var holding = false
    private var clutched = false
    private var note: String? = null
    private var noteUntil = 0L
    private var noteIsError = false
    private var servoCount = 0
    private var servoWindow = System.nanoTime()
    @Volatile
    private var lastState: ArmState? = null
    private var lastStateAt = 0L
    private val fist = HoldTimer()
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

    /** A start-pose move in progress. */
    private class Move(val target: List<Double>, val startedAt: Long, val engageAfter: Boolean) {
        var initialError = Double.NaN
    }

    private enum class Frame { ACCEPTED, EDGE_ON, DROPPED, RESET, UNMEASURABLE }

    /** Why this path can't move to the start pose, for the UI; null when it can. */
    val jointMoveUnavailable: String? get() = arm.jointMoveUnavailable

    fun engage() { engageRequested = true }
    fun disengage() { disengageRequested = true }

    /** Moves the arm to the start pose in joint space, then engages if [engageAfter]. */
    fun goToStartPose(engageAfter: Boolean = false) { moveRequested = engageAfter }

    /** Dismisses the outside-the-workspace prompt. */
    fun cancelPrompt() { cancelRequested = true }

    /** Emergency: stop the arm now (a start-pose move too) and give up control. Callable from any thread. */
    fun stopNow() {
        disengageRequested = true
        runCatching { arm.stop() }
    }

    /** "Set start pose here": the arm's current joints, if its TCP is inside the workspace. Returns what happened. */
    fun setStartPoseHere(): String {
        val st = lastState ?: return "No arm state yet"
        val j = jointsDeg(st) ?: return "The arm's joints aren't known (is it powered?)"
        val tcp = if (st.hasTcpPose()) Vec3(st.tcpPose.x, st.tcpPose.y, st.tcpPose.z) else PiperKinematics.tcp(j)
        if (tcp !in WORKSPACE) return "Not saved: the arm is outside the workspace here (${StartPoseStore.mm(tcp)})"
        return startPose.save(j)?.let { "Not saved: $it" } ?: "Start pose saved"
    }

    /** "Reset start pose": back to the driver's READY pose. Returns what happened. */
    fun resetStartPose(): String = startPose.reset()?.let { "Start pose reset, but $it" } ?: "Start pose reset to READY"

    /** One control tick (call at ~20 Hz with the newest observation). */
    fun tick(obs: Observation?, nowNanos: Long) {
        val dt = if (lastTickAt == 0L) 0.05 else ((nowNanos - lastTickAt) / 1e9).coerceIn(0.0, 0.2)
        lastTickAt = nowNanos
        refreshState(nowNanos)
        if (disengageRequested) {
            disengageRequested = false
            engageRequested = false
            moveRequested = null
            if (move != null) endMove("Move to the start pose stopped") else release("Disengaged")
            return
        }
        if (cancelRequested) {
            cancelRequested = false
            if (status.phase == TeleopStatus.Phase.OUTSIDE) status = status.copy(phase = TeleopStatus.Phase.IDLE, message = "Press ENGAGE (Space) to take control")
        }
        if (move != null) {
            stepMove(nowNanos)
            return
        }
        moveRequested?.let { engageAfter ->
            moveRequested = null
            startMove(engageAfter, nowNanos)
            return
        }

        val hand = obs?.hands?.firstOrNull { it.side == settings.hand }
        val metrics = hand?.let { HandMetrics.of(it) }
        val frameW = obs?.frame?.width ?: 640
        val frameH = obs?.frame?.height ?: 480
        val focal = settings.focalFraction * frameW

        // The fist gesture: held 1.5 s it engages (or disengages); while engaged, any fist is a clutch.
        val isFist = settings.fistGesture && hand != null && HandGesture.isFist(hand.world)
        if (fist.update(isFist, nowNanos)) {
            if (lease == null) {
                engageRequested = true
                clutched = true // anchor once the fist opens, not on the fist itself
            } else {
                release("Disengaged (fist)")
                return
            }
        }
        status = status.copy(gestureProgress = fist.progress(nowNanos), gestureEngages = lease == null)

        if (engageRequested && lease == null) {
            engageRequested = false
            if (metrics == null) {
                status = status.copy(phase = TeleopStatus.Phase.IDLE, message = "Show your ${settings.hand.name.lowercase()} hand to the camera first")
                return
            }
            if (outsideWorkspace()) {
                // Don't take control just to give it back: offer the start pose instead.
                prompt()
                return
            }
            if (!takeControl(nowNanos)) return
            anchor = null
            meas = null
            lastObsAt = 0
            resetFilters()
        }
        val l = lease
        if (l == null) {
            clutched = false
            val idle = status.copy(
                pinch = metrics?.let { pinchOf(it) } ?: 0.0,
                handDistance = metrics?.depth(focal),
                handRollDeg = null,
                toolRollDeg = 0.0,
                rollMode = settings.rollMode,
                moveProgress = null,
            )
            // The outside-the-workspace prompt stays up until it's answered.
            val n = noteNow(nowNanos)
            status = if (status.phase == TeleopStatus.Phase.OUTSIDE) idle else idle.copy(
                phase = if (n != null && noteIsError) TeleopStatus.Phase.ERROR else TeleopStatus.Phase.IDLE,
                message = n
                    ?: if (metrics == null) "Raise your ${settings.hand.name.lowercase()} hand into view"
                    else if (settings.fistGesture) "Press ENGAGE (Space), or hold a fist for 1.5 s, to take control"
                    else "Press ENGAGE (Space) to take control",
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
                release(leaseText(e), error = true)
                return
            }
        }

        // A fist while engaged is a clutch: the arm and gripper hold, and the hand is anchored afresh when
        // it opens, so it can be repositioned without moving the arm (the AgileX demo's open-hand rule).
        if (fist.held && settings.fistGesture) {
            if (!clutched) {
                clutched = true
                runCatching { arm.stop() }
            }
            anchor = null
            lastObsAt = obs.atNanos // the fist's frames don't count as hand positions
            status = status.copy(
                phase = TeleopStatus.Phase.HOLD,
                message = if (fist.spent) "Engaged: open your hand to start moving the arm"
                else "Fist: arm holding. Open your hand to move on, or keep the fist to disengage",
            )
            return
        }
        if (clutched) {
            clutched = false
            meas = null
            resetFilters()
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
                // from there makes the arm look for an IK solution it often can't find. Offer the start pose.
                release("The arm is outside the workspace")
                prompt()
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
        val raw = a.base.base + Vec3(reach, side, up)
        val p = a.base.target(Vec3(reach, side, up))
        val atEdge = (raw - p).norm() > 1e-6

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
            err = "Arm holding, servo refused: ${e.message}"
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
            message = err ?: noteNow(nowNanos)
                ?: if (atEdge) "At the edge of the workspace: the arm stops there and comes back as soon as your hand does"
                else "Engaged: the arm follows your ${settings.hand.name.lowercase()} hand",
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

    // ---------------------------------------------------------------- start pose

    private fun outsideWorkspace(): Boolean {
        val st = lastState ?: return false
        if (!st.hasTcpPose()) return false
        return Vec3(st.tcpPose.x, st.tcpPose.y, st.tcpPose.z) !in WORKSPACE
    }

    private fun prompt() {
        val canMove = arm.jointMoveUnavailable == null
        status = status.copy(
            phase = TeleopStatus.Phase.OUTSIDE,
            message = if (canMove) "The arm is outside the workspace: move it to the start pose?"
            else "The arm is outside the workspace, and this connection can't move it to the start pose (${arm.jointMoveUnavailable}). Move it into the workspace first",
            target = null,
        )
    }

    private fun takeControl(nowNanos: Long): Boolean {
        if (lease != null) return true
        return try {
            val c = arm.acquire()
            lease = c
            lastRenew = nowNanos
            log.info("${c.description}, epoch ${c.epoch}")
            true
        } catch (e: Exception) {
            flag(leaseText(e), nowNanos, NOTE_NS, error = true)
            status = status.copy(phase = TeleopStatus.Phase.ERROR, message = leaseText(e), lastError = e.message)
            false
        }
    }

    private fun startMove(engageAfter: Boolean, nowNanos: Long) {
        arm.jointMoveUnavailable?.let {
            flag("Can't move to the start pose: $it", nowNanos, NOTE_NS, error = true)
            status = status.copy(phase = TeleopStatus.Phase.ERROR, message = "Can't move to the start pose: $it")
            return
        }
        fist.reset()
        if (!takeControl(nowNanos)) return
        anchor = null
        meas = null
        val target = startPose.joints
        try {
            arm.startJointMove(target, nowNanos)
        } catch (e: Exception) {
            release("Can't move to the start pose: ${e.message}", error = true)
            return
        }
        log.info("moving to the start pose $target")
        move = Move(target, nowNanos, engageAfter).also { mv -> lastState?.let { jointsDeg(it) }?.let { mv.initialError = maxError(it, target).second } }
        lastStateAt = 0 // read the joints again right away
        status = status.copy(
            phase = TeleopStatus.Phase.MOVING,
            moveProgress = 0.0,
            gestureProgress = 0.0,
            target = null,
            message = if (arm.confirmsMoves) "Moving to the start pose (confirm it on screen)" else "Moving to the start pose",
        )
    }

    /** One tick of the start-pose move: keep it alive, watch the joints, finish or give up. */
    private fun stepMove(nowNanos: Long) {
        val mv = move ?: return
        try {
            lease?.let { l ->
                // A NervusOS HUMAN lease has a 300 ms deadman, and a move sends no commands of its own.
                if (nowNanos - lastRenew > 200_000_000L) {
                    l.renew()
                    lastRenew = nowNanos
                }
            }
            arm.keepJointMove(nowNanos)
        } catch (e: Exception) {
            endMove("Move to the start pose stopped: ${e.message}", error = true)
            return
        }
        val cur = lastState?.let { jointsDeg(it) }
        if (cur != null) {
            val (worst, err) = maxError(cur, mv.target)
            if (mv.initialError.isNaN()) mv.initialError = err
            if (err <= ARRIVED_DEG) {
                finishMove(mv, nowNanos)
                return
            }
            val limit = MOVE_TIMEOUT_NS + if (arm.confirmsMoves) CONFIRM_GRACE_NS else 0L
            if (nowNanos - mv.startedAt > limit) {
                endMove("The start pose wasn't reached in ${MOVE_TIMEOUT_NS / 1_000_000_000L} s (J${worst + 1} is ${"%.0f".format(err)} deg short): arm stopped", error = true)
                return
            }
            val p = if (mv.initialError > ARRIVED_DEG) (1 - (err - ARRIVED_DEG) / (mv.initialError - ARRIVED_DEG)).coerceIn(0.0, 1.0) else 0.0
            status = status.copy(
                phase = TeleopStatus.Phase.MOVING,
                moveProgress = p,
                message = "Moving to the start pose: ${"%.0f".format(err)} deg to go (J${worst + 1}). STOP or Esc aborts",
            )
        } else if (nowNanos - mv.startedAt > MOVE_TIMEOUT_NS) {
            endMove("The start pose move stopped: no joint feedback from the arm", error = true)
        }
    }

    private fun finishMove(mv: Move, nowNanos: Long) {
        move = null
        runCatching { arm.endJointMove() }
        log.info("at the start pose")
        if (mv.engageAfter && lease != null) {
            anchor = null
            meas = null
            lastObsAt = 0
            holding = false
            resetFilters()
            flag("At the start pose: engaged", nowNanos)
            status = status.copy(phase = TeleopStatus.Phase.HOLD, moveProgress = null, message = "At the start pose: engaged. Show your hand")
        } else {
            release("At the start pose. Press ENGAGE (Space) to take control")
        }
    }

    /** The move is over without arriving (STOP, refused, timed out): hold the arm where it is and give up control. */
    private fun endMove(message: String, error: Boolean = false) {
        move = null
        runCatching { arm.endJointMove() }
        log.info(message)
        release(message, error) // stops the arm, so it holds where it got to
    }

    // ---------------------------------------------------------------- measuring

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

    /**
     * Shows [text] in place of the usual message for a while (a second by default), so that a one-frame
     * event, or why control was given up, can be read before the next tick's message replaces it.
     */
    private fun flag(text: String, nowNanos: Long, forNanos: Long = 1_000_000_000L, error: Boolean = false) {
        note = text
        noteUntil = nowNanos + forNanos
        noteIsError = error
    }

    private fun noteNow(nowNanos: Long): String? = note?.takeIf { nowNanos < noteUntil }

    private fun refreshState(nowNanos: Long) {
        // Five times a second, ten while a move is watching the joints.
        val period = if (move != null) 100_000_000L else 200_000_000L
        if (nowNanos - lastStateAt < period) return
        lastStateAt = nowNanos
        try {
            val st = arm.state()
            lastState = st
            status = status.copy(
                armReadiness = st.readiness.name.removePrefix("ARM_READINESS_"),
                armWords = arm.words,
                tcp = if (st.hasTcpPose()) st.tcpPose else null,
            )
        } catch (e: Exception) {
            status = status.copy(armReadiness = "UNREACHABLE", armWords = arm.words)
        }
    }

    /** Gives up control, stopping the arm where it is. [message] stays on screen for a few seconds. */
    private fun release(message: String, error: Boolean = false) {
        val l = lease
        lease = null
        anchor = null
        meas = null
        holding = false
        clutched = false
        flag(message, lastTickAt, NOTE_NS, error)
        resetFilters()
        if (l != null) {
            runCatching { arm.stop() }
            runCatching { l.release() }
        }
        lastGripSent = -1.0
        status = status.copy(
            phase = if (error) TeleopStatus.Phase.ERROR else TeleopStatus.Phase.IDLE,
            message = message, target = null, handRollDeg = null, toolRollDeg = 0.0, moveProgress = null,
        )
    }

    private fun leaseText(e: Throwable): String = when (e) {
        is ArmRefusedException -> "Arm not available: ${e.message}"
        else -> "Could not take control of the arm: ${(e.cause ?: e).message}"
    }

    fun shutdown() {
        if (move != null) endMove("Closed") else release("Closed")
    }

    companion object {
        // Metres in base_link, a little inside the driver's own box.
        val X_RANGE = 0.08..0.46
        val Y_RANGE = -0.30..0.30
        val Z_RANGE = 0.10..0.55
        val WORKSPACE = Box(X_RANGE, Y_RANGE, Z_RANGE)

        /** A frame whose palm bones look shorter than this fraction of the anchor's is taken as edge-on. */
        const val EDGE_ON = 0.4

        /** How long a reason for giving up control stays on screen. */
        const val NOTE_NS = 5_000_000_000L

        /** The start pose is reached when every joint is this close (degrees). */
        const val ARRIVED_DEG = 1.0
        const val MOVE_TIMEOUT_NS = 25_000_000_000L
        /** Extra time for a move that waits for the user's confirmation first (NervusOS). */
        const val CONFIRM_GRACE_NS = 60_000_000_000L

        /** The joints of [st] in degrees, or null when the arm doesn't report six. */
        fun jointsDeg(st: ArmState): List<Double>? =
            st.joints.positionRadList.takeIf { st.hasJoints() && it.size == 6 }?.map { Math.toDegrees(it) }

        /** The joint furthest from its target, and by how much (degrees). */
        fun maxError(cur: List<Double>, target: List<Double>): Pair<Int, Double> =
            cur.indices.map { it to kotlin.math.abs(cur[it] - target[it]) }.maxBy { it.second }
    }
}
