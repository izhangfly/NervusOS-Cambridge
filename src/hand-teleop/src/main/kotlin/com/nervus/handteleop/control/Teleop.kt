package com.nervus.handteleop.control

import com.nervus.handteleop.math.ArmCalibrator
import com.nervus.handteleop.math.BodyMath
import com.nervus.handteleop.math.Box
import com.nervus.handteleop.math.HandMath
import com.nervus.handteleop.math.Mat3
import com.nervus.handteleop.math.OutlierGate
import com.nervus.handteleop.math.Quat
import com.nervus.handteleop.math.RollShaper
import com.nervus.handteleop.math.SlipAnchor
import com.nervus.handteleop.math.Vec3
import com.nervus.handteleop.vision.BodyPose
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

/**
 * How the hand drives the arm. MIRROR: absolute, shoulder-anchored; the arm puts its TCP where your hand
 * is relative to your shoulder (scaled), with the robot's shoulder standing in for yours. RELATIVE: the
 * clutch; the arm follows the hand's movement since you engaged.
 */
enum class ControlMode(val label: String) {
    MIRROR("Mirror my arm"),
    RELATIVE("Relative (clutch)"),
}

/** Everything the operator can tune. Read by the control loop every tick. */
class TeleopSettings {
    @Volatile var controlMode: ControlMode = ControlMode.MIRROR
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
    /** Mirror mode: the operator's shoulder width, metres. It sets the torso's distance from the camera; "Calibrate arm" measures it. */
    @Volatile var shoulderWidthM: Double = DEFAULT_SHOULDER_WIDTH_M
    /**
     * Mirror mode: robot metres per metre of the operator's reach, the robot's reach over the arm's
     * length (shoulder to palm): 0.50 / 0.62 by default; "Calibrate arm" measures the arm.
     */
    @Volatile var armScale: Double = ROBOT_REACH_M / DEFAULT_ARM_LENGTH_M
    /**
     * Mirror mode: raises every target, metres. The robot's shoulder (J2) is only 0.123 m above the table, so
     * without a lift a relaxed hand at chest height maps onto the table; 0.15 puts it in mid-air.
     */
    @Volatile var mirrorLiftM: Double = 0.15
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

    companion object {
        /** How far the PiPER reaches for a fully stretched operator arm in mirror mode, metres. */
        const val ROBOT_REACH_M = 0.50
        const val DEFAULT_ARM_LENGTH_M = 0.62
        const val DEFAULT_SHOULDER_WIDTH_M = 0.37
    }
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
    val mode: ControlMode = ControlMode.MIRROR,
    /**
     * Diagnostics. [reach]: mirror mode, the operator's shoulder-to-hand vector (forward, side, up; m);
     * clutch mode, the hand's movement since the anchor in the same axes. [goalRaw] / [goal]: the TCP
     * target before and after the workspace clamp (base_link, m); [clamped]: they differ; [slipping]:
     * clutch mode, the anchor is sliding along the edge.
     */
    val reach: Vec3? = null,
    val goalRaw: Vec3? = null,
    val goal: Vec3? = null,
    val clamped: Boolean = false,
    val slipping: Boolean = false,
    /** Why the arm won't reach the target as asked (the driver's planner), or null. */
    val servoNote: String? = null,
    /** "Calibrate arm" progress, 0..1, while [Phase.CALIBRATING]. */
    val calibration: Double? = null,
) {
    /**
     * IDLE: not in control. OUTSIDE: engage was asked for (clutch mode), but the arm is outside the
     * workspace; the UI offers the start pose. MOVING: going to the start pose. CALIBRATING: measuring the
     * operator's arm. APPROACH: mirror mode, engaged and on the way to where the hand says. ENGAGED /
     * HOLD: in control, following or holding. ERROR: in control (or trying), but the last command was refused.
     */
    enum class Phase { IDLE, OUTSIDE, MOVING, CALIBRATING, APPROACH, ENGAGED, HOLD, ERROR }
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
 * That is RELATIVE (clutch) mode. In MIRROR mode (the default) the mapping is absolute instead: the
 * vector from the operator's shoulder (YOLO pose, at the torso's depth from the shoulder width) to the
 * palm (at the palm's own depth) is scaled onto the robot with the robot's shoulder (J2) as the origin.
 * Engaging there means the arm first travels to where the hand says (APPROACH), slowly, and then follows.
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
    private val body: BodySizeStore = BodySizeStore(SettingsFile(null)),
    private val tickLog: TickLog? = null,
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
    // Mirror mode: the shoulder's pixel position and the shoulder width (the torso's distance) are smoothed
    // like the hand; the width hardest, since the torso hardly moves toward or away while working.
    private val fsu = OneEuro(1.0, 0.015)
    private val fsv = OneEuro(1.0, 0.015)
    private val fsw = OneEuro(0.3, 0.005)
    // A whole-arm estimate can legitimately move much faster than a hand nudge: 15 cm per frame.
    private val mirrorGate = OutlierGate(maxStep = MIRROR_JUMP_M)
    private var approaching = false
    private var approachGoal: Vec3? = null
    private var approachStart = 0L
    private var mirrorRef: MirrorRef? = null
    private val tcpTrail = ArrayDeque<Pair<Long, Vec3>>()
    @Volatile
    private var calibrateRequested = false
    private var calibrating = false
    private var calibrationStart = 0L
    private val calibrator = ArmCalibrator()

    init {
        // A saved calibration replaces the defaults.
        body.load()?.let {
            settings.shoulderWidthM = it.shoulderWidthM
            settings.armScale = TeleopSettings.ROBOT_REACH_M / it.armLengthM
        }
    }

    /**
     * One accepted vision frame, filtered: the palm centre in camera metres, the palm frame, the pinch;
     * in mirror mode also the shoulder-to-hand vector in the operator's axes (forward, side, up).
     */
    private class Measurement(val pos: Vec3, val palm: Mat3?, val pinch: Double, val bonesPx: Double, val reach: Vec3? = null)

    /** Mirror mode: the tool orientation to keep and the palm to measure roll from, taken when engaging. */
    private class MirrorRef(val q0: Quat?, val palm: Mat3?, val bonesPx: Double)

    /** The hand and the arm as they were at the anchor. */
    private class Anchor(val hand: Measurement, val tcp: Pose, val base: SlipAnchor) {
        /** The arm's orientation, which roll turns from; null if the Provider reports none. */
        val q0: Quat? = Quat(tcp.qx, tcp.qy, tcp.qz, tcp.qw).unit()
    }

    /** A start-pose move in progress. */
    private class Move(val target: List<Double>, val startedAt: Long, val engageAfter: Boolean) {
        var initialError = Double.NaN
    }

    private enum class Frame { ACCEPTED, EDGE_ON, DROPPED, RESET, UNMEASURABLE, NO_SHOULDERS }

    /** Why this path can't move to the start pose, for the UI; null when it can. */
    val jointMoveUnavailable: String? get() = arm.jointMoveUnavailable

    fun engage() { engageRequested = true }
    fun disengage() { disengageRequested = true }

    /** Moves the arm to the start pose in joint space, then engages if [engageAfter]. */
    fun goToStartPose(engageAfter: Boolean = false) { moveRequested = engageAfter }

    /** Dismisses the outside-the-workspace prompt. */
    fun cancelPrompt() { cancelRequested = true }

    /** "Calibrate arm": measure the operator's arm and shoulders (arm held straight out sideways for 2 s). */
    fun calibrate() { calibrateRequested = true }

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
        tickInner(obs, nowNanos)
        val s = status
        if (tickLog != null && (lease != null || move != null || s.reach != null || calibrating)) {
            val tcp = s.tcp?.let { Vec3(it.x, it.y, it.z) }
            tickLog.write(nowNanos, s.phase.name, s.mode.name, s.reach, s.goalRaw, s.goal, tcp,
                listOfNotNull(if (s.clamped) "clamped" else null, if (s.slipping) "slip" else null, s.servoNote, s.message).joinToString(" | "))
        }
    }

    private fun tickInner(obs: Observation?, nowNanos: Long) {
        val dt = if (lastTickAt == 0L) 0.05 else ((nowNanos - lastTickAt) / 1e9).coerceIn(0.0, 0.2)
        lastTickAt = nowNanos
        refreshState(nowNanos)
        if (disengageRequested) {
            disengageRequested = false
            engageRequested = false
            moveRequested = null
            calibrateRequested = false
            when {
                move != null -> endMove("Move to the start pose stopped")
                calibrating -> endCalibration("Calibration stopped", error = false)
                else -> release("Disengaged")
            }
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

        if (calibrateRequested) {
            calibrateRequested = false
            if (lease != null) {
                flag("Disengage before calibrating", nowNanos, NOTE_NS, error = true)
            } else {
                calibrating = true
                calibrationStart = nowNanos
                calibrator.reset()
            }
        }
        if (calibrating) {
            stepCalibration(obs, hand, metrics, focal, nowNanos)
            return
        }

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
            if (settings.controlMode == ControlMode.RELATIVE && outsideWorkspace()) {
                // Don't take control just to give it back: offer the start pose instead. (Mirror mode doesn't
                // start from the arm's pose, so it needs no start pose: the driver's planner gets it there.)
                prompt()
                return
            }
            if (!takeControl(nowNanos)) return
            anchor = null
            meas = null
            lastObsAt = 0
            resetFilters()
            startApproach(nowNanos)
            mirrorRef = null
        }
        val l = lease
        if (l == null) {
            clutched = false
            val pv = preview(obs, metrics, focal)
            val idle = status.copy(
                mode = settings.controlMode,
                reach = pv?.first,
                goalRaw = pv?.second,
                goal = pv?.third,
                clamped = pv != null && (pv.second - pv.third).norm() > 1e-6,
                slipping = false,
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
            startApproach(nowNanos) // mirror mode: the hand may be somewhere else now
        }

        // Each vision frame is measured once. The ticks in between send the same target again, which
        // keeps the stream (and the lease's deadman) going while the arm holds there.
        if (obs.atNanos != lastObsAt) {
            lastObsAt = obs.atNanos
            when (measure(hand, metrics, obs.body, frameW, frameH, focal, obs.atNanos)) {
                Frame.ACCEPTED -> {}
                Frame.EDGE_ON -> flag("Palm edge-on to the camera: holding the last position", nowNanos)
                Frame.DROPPED -> flag("Tracking jumped: frame skipped", nowNanos)
                Frame.UNMEASURABLE -> flag("Hand too small to measure: holding the last position", nowNanos)
                Frame.NO_SHOULDERS -> flag("Shoulders not in view: step back so the camera sees both, arm holding", nowNanos)
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
            status = status.copy(
                phase = TeleopStatus.Phase.HOLD,
                message = noteNow(nowNanos) ?: if (settings.controlMode == ControlMode.MIRROR) "Show your hand and both shoulders to the camera" else "Hold your hand still to anchor",
            )
            return
        }
        if (settings.controlMode == ControlMode.MIRROR) {
            mirrorStep(m, l, nowNanos, dt)
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

        var err = servo(target, settings.speed)
        val opening = gripper(m, nowNanos)?.let { e -> if (err == null) err = e; null } ?: status.gripper
        countServo(nowNanos)
        status = status.copy(
            mode = ControlMode.RELATIVE,
            reach = BodyMath.toUser(d, settings.invertSide),
            goalRaw = raw,
            goal = p,
            clamped = atEdge,
            slipping = atEdge,
            servoNote = arm.servoNote,
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

    /** Sends [target]; null when accepted, else why not (and the anchor is dropped, so the next tick starts from the arm's real pose). */
    private fun servo(target: Pose, speed: Double): String? = try {
        arm.servo(target, speed)
        servoCount++
        null
    } catch (e: ArmRefusedException) {
        // The lease may have lapsed (deadman, pre-empted), or the driver is holding the arm (busy, IK
        // guard): show why, renew next tick, then re-anchor on wherever the arm really is.
        lastRenew = 0
        anchor = null
        "Arm holding, servo refused: ${e.message}"
    } catch (e: Exception) {
        e.message ?: "servo failed"
    }

    /**
     * The gripper follows the pinch (continuous opening), sent only when it changes enough, at most 10
     * times a second. Updates the status's opening; returns an error, or null.
     */
    private fun gripper(m: Measurement, nowNanos: Long): String? {
        val opening = ((m.pinch - settings.closedRatio) / (settings.openRatio - settings.closedRatio)).coerceIn(0.0, 1.0)
        status = status.copy(gripper = opening)
        val gripStep = kotlin.math.abs(opening - lastGripSent)
        if (gripStep > 0.04 || (gripStep > 0.005 && (opening == 0.0 || opening == 1.0)) || lastGripSent < 0) {
            if (nowNanos - lastGripAt > 100_000_000L) {
                return runCatching { arm.gripper(opening, settings.gripperEffortNm) }
                    .onSuccess { lastGripSent = opening; lastGripAt = nowNanos }
                    .exceptionOrNull()?.let { "gripper: ${it.message}" }
            }
        }
        return null
    }

    private fun countServo(nowNanos: Long) {
        if (nowNanos - servoWindow > 1_000_000_000L) {
            val hz = servoCount * 1e9f / (nowNanos - servoWindow)
            servoCount = 0
            servoWindow = nowNanos
            status = status.copy(servoHz = hz)
        }
    }

    // ---------------------------------------------------------------- mirror mode

    private fun startApproach(nowNanos: Long) {
        approaching = true
        approachGoal = null
        approachStart = nowNanos
    }

    /**
     * Mirror mode, one tick: the shoulder-to-hand vector scaled onto the robot's shoulder, clamped to the
     * reachable band and the driver's box. Engaging (and coming back after a lost hand or a fist) starts
     * with APPROACH: the arm travels to the target slowly, the goal re-sent unchanged until the hand has
     * moved a few cm (so the driver keeps one plan instead of replanning every tick), until the TCP is
     * within [ARRIVE_M] or stops getting closer. Then it follows every tick at the normal speed.
     */
    private fun mirrorStep(m: Measurement, l: ArmControl, nowNanos: Long, dt: Double) {
        val reach = m.reach ?: return
        val raw = BodyMath.mirrorTarget(reach, settings.armScale, settings.mirrorLiftM)
        val goal = (arm.workspace ?: MIRROR_WORKSPACE).clamp(BodyMath.clampReach(raw))
        val clamped = (goal - raw).norm() > 1e-6
        val st = lastState
        val tcp = st?.takeIf { it.hasTcpPose() && it.tcpPose.frame.isNotEmpty() }?.tcpPose
        if (tcp == null) {
            status = status.copy(phase = TeleopStatus.Phase.HOLD, message = "Waiting for the arm's pose (is the PiPER powered?)", reach = reach, goalRaw = raw, goal = goal, clamped = clamped)
            return
        }
        val here = Vec3(tcp.x, tcp.y, tcp.z)
        val ref = mirrorRef ?: MirrorRef(
            // Keep the tool's orientation from engaging; from a folded arm, the READY pose's (pointing
            // forward and down), which works across the workspace, instead of the folded one.
            q0 = if (here in WORKSPACE) Quat(tcp.qx, tcp.qy, tcp.qz, tcp.qw).unit() else READY_Q,
            palm = m.palm, bonesPx = m.bonesPx,
        ).also { mirrorRef = it; roll.reset() }

        val dist = (here - goal).norm()
        val sent: Vec3
        val speed: Double
        var arrivedShort = false
        if (approaching) {
            val g = approachGoal
            if (g == null || (goal - g).norm() > APPROACH_RESEND_M) approachGoal = goal
            sent = approachGoal!!
            speed = minOf(settings.speed, APPROACH_SPEED)
            if (dist < ARRIVE_M) {
                approaching = false
            } else if (nowNanos - approachStart > 2_000_000_000L && settled(nowNanos)) {
                // The planner got as close as it can (out of reach, partial plan): follow from here.
                approaching = false
                arrivedShort = true
            }
        } else {
            sent = goal
            speed = settings.speed
        }
        // Roll only once the arm is following: while approaching, the goal must stay the same goal.
        val handRoll = if (ref.palm != null && m.palm != null) HandMath.roll(ref.palm, m.palm) else null
        val toolRoll = when {
            approaching -> roll.value
            handRoll == null -> roll.value
            else -> roll.step(handRoll * settings.rollMode.sign, dt)
        }
        val target = Pose.newBuilder(tcp).setFrame(tcp.frame).setX(sent.x).setY(sent.y).setZ(sent.z).apply {
            ref.q0?.let { q0 ->
                val q = q0 * Quat.about(Vec3.Z, toolRoll)
                setQx(q.x).setQy(q.y).setQz(q.z).setQw(q.w)
            }
        }.build()
        var err = servo(target, speed)
        if (err != null) {
            startApproach(nowNanos) // after a refusal, go back to the hand slowly
            mirrorRef = null
        }
        val opening = gripper(m, nowNanos)?.let { e -> if (err == null) err = e; null } ?: status.gripper
        countServo(nowNanos)
        val note = arm.servoNote
        status = status.copy(
            phase = when {
                err != null -> TeleopStatus.Phase.ERROR
                approaching -> TeleopStatus.Phase.APPROACH
                else -> TeleopStatus.Phase.ENGAGED
            },
            message = err ?: noteNow(nowNanos) ?: when {
                approaching -> "Moving to your hand: %.0f cm to go".format(dist * 100)
                arrivedShort -> "The arm stopped %.0f cm short of where your hand says (out of reach?): following from here".format(dist * 100)
                note != null -> "Following, but $note"
                clamped -> "At the edge of what the arm can reach: following as close as it can"
                else -> "Mirroring your ${settings.hand.name.lowercase()} arm"
            },
            mode = ControlMode.MIRROR,
            target = target,
            reach = reach,
            goalRaw = raw,
            goal = sent,
            clamped = clamped,
            slipping = false,
            servoNote = note,
            gripper = opening,
            pinch = m.pinch,
            leaseEpoch = l.epoch,
            lastError = err ?: status.lastError,
            handDistance = m.pos.z,
            handRollDeg = handRoll?.let { Math.toDegrees(it) },
            toolRollDeg = Math.toDegrees(toolRoll),
            rollMode = settings.rollMode,
        )
        if (arrivedShort) flag(status.message, nowNanos, 3_000_000_000L)
    }

    /** The TCP has moved less than 5 mm over the last second (the approach got as far as it will). */
    private fun settled(nowNanos: Long): Boolean {
        val recent = tcpTrail.filter { nowNanos - it.first <= 1_000_000_000L }
        if (recent.size < 4 || nowNanos - recent.first().first < 800_000_000L) return false
        val p0 = recent.first().second
        return recent.all { (it.second - p0).norm() < 0.005 }
    }

    // ---------------------------------------------------------------- calibration

    /**
     * "Calibrate arm", one tick: the operator faces the camera with the arm straight out sideways. The
     * palm's metric depth stands in for the torso's, so the shoulder width and the shoulder-to-palm length
     * come out in metres; [ArmCalibrator] needs 2 s of good frames and takes the medians.
     */
    private fun stepCalibration(obs: Observation?, hand: HandLandmarks?, metrics: HandMetrics?, focal: Double, nowNanos: Long) {
        if (nowNanos - calibrationStart > CALIBRATION_TIMEOUT_NS) {
            endCalibration("Calibration timed out: hold your arm straight out to the side, level, for 2 s", error = true)
            return
        }
        val side = settings.hand.name.lowercase()
        val b = obs?.body
        val (sIdx, eIdx) = shoulderElbow()
        val other = if (sIdx == BodyPose.L_SHOULDER) BodyPose.R_SHOULDER else BodyPose.L_SHOULDER
        val z = metrics?.depth(focal)
        val why = when {
            b == null || hand == null || metrics == null || z == null -> "show your $side hand and your shoulders"
            b[sIdx].score < KP_SCORE || b[other].score < KP_SCORE || b[eIdx].score < KP_SCORE -> "the camera needs to see both shoulders and your elbow"
            else -> calibrator.check(b[sIdx].x.toDouble(), b[sIdx].y.toDouble(), b[eIdx].x.toDouble(), b[eIdx].y.toDouble(), metrics.palmX.toDouble(), metrics.palmY.toDouble())
        }
        val result = if (b != null && metrics != null && z != null) {
            val w = kotlin.math.hypot((b[sIdx].x - b[other].x).toDouble(), (b[sIdx].y - b[other].y).toDouble())
            val armPx = kotlin.math.hypot(metrics.palmX.toDouble() - b[sIdx].x, metrics.palmY.toDouble() - b[sIdx].y)
            calibrator.update(why, nowNanos, w, armPx, z, focal)
        } else {
            calibrator.update(why ?: "no hand", nowNanos, 0.0, 0.0, 1.0, focal)
        }
        if (result != null) {
            if (result.armLengthM !in ArmCalibrator.ARM_RANGE || result.shoulderWidthM !in ArmCalibrator.SHOULDER_RANGE) {
                endCalibration("Calibration didn't look right (arm %.2f m, shoulders %.2f m): try again, facing the camera".format(result.armLengthM, result.shoulderWidthM), error = true)
                return
            }
            settings.shoulderWidthM = result.shoulderWidthM
            settings.armScale = TeleopSettings.ROBOT_REACH_M / result.armLengthM
            val saved = body.save(result)
            endCalibration(
                "Calibrated: arm %.2f m, shoulders %.2f m, arm scale %.2f".format(result.armLengthM, result.shoulderWidthM, settings.armScale) +
                    (saved?.let { " (not saved: $it)" } ?: ""),
                error = false,
            )
            return
        }
        status = status.copy(
            phase = TeleopStatus.Phase.CALIBRATING,
            calibration = calibrator.progress(nowNanos),
            message = "Calibrating: face the camera and hold your $side arm straight out to the side, level, for 2 s" +
                (why?.let { " ($it)" } ?: ""),
        )
    }

    private fun endCalibration(message: String, error: Boolean) {
        calibrating = false
        calibrator.reset()
        flag(message, lastTickAt, NOTE_NS, error)
        status = status.copy(phase = if (error) TeleopStatus.Phase.ERROR else TeleopStatus.Phase.IDLE, calibration = null, message = message)
    }

    /** The operating side's shoulder and elbow keypoints (COCO). */
    private fun shoulderElbow(): Pair<Int, Int> =
        if (settings.hand == HandLandmarks.Side.LEFT) BodyPose.L_SHOULDER to BodyPose.L_ELBOW else BodyPose.R_SHOULDER to BodyPose.R_ELBOW

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
    private fun measure(h: HandLandmarks, m: HandMetrics, body: BodyPose?, frameW: Int, frameH: Int, focal: Double, t: Long): Frame {
        val z = m.depth(focal) ?: return Frame.UNMEASURABLE
        val mirror = settings.controlMode == ControlMode.MIRROR
        // Nearly edge-on (fingers toward the camera), both bone sums shrink toward nothing and their
        // ratio, the depth, is mostly noise.
        val refBones = if (mirror) mirrorRef?.bonesPx else anchor?.hand?.bonesPx
        if (refBones != null && m.bonesPx < EDGE_ON * refBones) return Frame.EDGE_ON
        val cx = frameW / 2.0
        val cy = frameH / 2.0
        val left = h.side == HandLandmarks.Side.LEFT
        // Raw values go to the gate before any filter sees them, so a glitch never gets into the filters.
        val raw = HandMath.backProject(m.palmX.toDouble(), m.palmY.toDouble(), z, focal, cx, cy)
        val rawPalm = if (settings.rollMode != RollMode.OFF) HandMath.palmFrame(h.world, left) else null
        var shoulderPx: Triple<Double, Double, Double>? = null
        val verdict = if (mirror) {
            val sh = shoulders(body) ?: return Frame.NO_SHOULDERS
            val zt = BodyMath.torsoDepth(focal, settings.shoulderWidthM, sh.third) ?: return Frame.NO_SHOULDERS
            shoulderPx = sh
            mirrorGate.check(BodyMath.shoulderToHand(sh.first, sh.second, zt, m.palmX.toDouble(), m.palmY.toDouble(), z, focal, cx, cy), rawPalm)
        } else {
            gate.check(raw, rawPalm)
        }
        when (verdict) {
            OutlierGate.Verdict.ACCEPT -> {}
            OutlierGate.Verdict.DROP -> return Frame.DROPPED
            OutlierGate.Verdict.RESET -> return Frame.RESET
        }
        val u = fx.filter(m.palmX.toDouble(), t)
        val v = fy.filter(m.palmY.toDouble(), t)
        val zf = fz.filter(z, t)
        val pts = HandMath.PALM_POINTS.mapIndexed { k, i -> fw[k].filter(h.world[i], t) }
        val palm = HandMath.palmFrame(pts[0], pts[1], pts[2], pts[3], pts[4], left) ?: meas?.palm
        val reach = shoulderPx?.let { sh ->
            val su = fsu.filter(sh.first, t)
            val sv = fsv.filter(sh.second, t)
            val sw = fsw.filter(sh.third, t)
            val zt = BodyMath.torsoDepth(focal, settings.shoulderWidthM, sw) ?: return Frame.NO_SHOULDERS
            BodyMath.toUser(BodyMath.shoulderToHand(su, sv, zt, u, v, zf, focal, cx, cy), settings.invertSide)
        }
        meas = Measurement(HandMath.backProject(u, v, zf, focal, cx, cy), palm, fp.filter(pinchOf(m), t), m.bonesPx, reach)
        return Frame.ACCEPTED
    }

    /** The operating side's shoulder (pixels) and the shoulder width (pixels), or null when the pose model doesn't see both shoulders. */
    private fun shoulders(body: BodyPose?): Triple<Double, Double, Double>? {
        val b = body ?: return null
        val (sIdx, _) = shoulderElbow()
        val other = if (sIdx == BodyPose.L_SHOULDER) BodyPose.R_SHOULDER else BodyPose.L_SHOULDER
        if (b[sIdx].score < KP_SCORE || b[other].score < KP_SCORE) return null
        val w = kotlin.math.hypot((b[sIdx].x - b[other].x).toDouble(), (b[sIdx].y - b[other].y).toDouble())
        return Triple(b[sIdx].x.toDouble(), b[sIdx].y.toDouble(), w)
    }

    /**
     * Mirror mode while not in control: the unfiltered shoulder-to-hand vector and where it would send
     * the arm, so the operator can see the mapping before engaging.
     */
    private fun preview(obs: Observation?, m: HandMetrics?, focal: Double): Triple<Vec3, Vec3, Vec3>? {
        if (settings.controlMode != ControlMode.MIRROR || obs == null || m == null) return null
        val sh = shoulders(obs.body) ?: return null
        val z = m.depth(focal) ?: return null
        val zt = BodyMath.torsoDepth(focal, settings.shoulderWidthM, sh.third) ?: return null
        val v = BodyMath.shoulderToHand(sh.first, sh.second, zt, m.palmX.toDouble(), m.palmY.toDouble(), z, focal, obs.frame.width / 2.0, obs.frame.height / 2.0)
        val reach = BodyMath.toUser(v, settings.invertSide)
        val raw = BodyMath.mirrorTarget(reach, settings.armScale, settings.mirrorLiftM)
        return Triple(reach, raw, (arm.workspace ?: MIRROR_WORKSPACE).clamp(BodyMath.clampReach(raw)))
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
        // Mirror mode: the hand may come back anywhere, so travel to it slowly again.
        startApproach(lastTickAt)
        mirrorRef = null
    }

    private fun resetFilters() {
        fx.reset(); fy.reset(); fz.reset(); fp.reset()
        fw.forEach { it.reset() }
        gate.reset()
        roll.reset()
        fsu.reset(); fsv.reset(); fsw.reset()
        mirrorGate.reset()
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
            if (st.hasTcpPose()) {
                tcpTrail.addLast(nowNanos to Vec3(st.tcpPose.x, st.tcpPose.y, st.tcpPose.z))
                while (tcpTrail.isNotEmpty() && nowNanos - tcpTrail.first().first > 2_000_000_000L) tcpTrail.removeFirst()
            }
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
        approaching = false
        mirrorRef = null
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
            goalRaw = null, goal = null, clamped = false, slipping = false, servoNote = null,
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

        /**
         * Mirror mode's box: the PiPER driver's own default (--workspace 60,480,-320,320,80,600 mm), to
         * which it clamps servo goals anyway; a driver that reports another box is followed instead. The
         * floor, 80 mm, keeps the gripper off the table.
         */
        val MIRROR_WORKSPACE = Box(0.06..0.48, -0.32..0.32, 0.08..0.60)
        /** Mirror mode: a shoulder-to-hand estimate moving more than this in one frame is a glitch. */
        const val MIRROR_JUMP_M = 0.15
        /** Mirror mode: engaged, the arm travels to the hand until it is this close... */
        const val ARRIVE_M = 0.02
        /** ...at this speed (fraction of the driver's), re-planning only when the target moves this far. */
        const val APPROACH_SPEED = 0.3
        const val APPROACH_RESEND_M = 0.03
        /** Pose keypoints below this confidence don't count. */
        const val KP_SCORE = 0.3f
        const val CALIBRATION_TIMEOUT_NS = 20_000_000_000L

        /** The READY pose's tool orientation, which mirror mode keeps when engaging from a folded arm. */
        val READY_Q: Quat = PiperKinematics.endPose(PiperKinematics.READY_POSE)
            .let { PiperPose.eulerToQuat(Math.toRadians(it[3]), Math.toRadians(it[4]), Math.toRadians(it[5])) }

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
