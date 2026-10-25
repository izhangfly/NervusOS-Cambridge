package com.nervus.handteleop.math

import kotlin.math.abs
import kotlin.math.acos
import kotlin.math.atan2
import kotlin.math.hypot

// "Mirror my arm": the operator's hand relative to their own shoulder, mapped onto the PiPER with its
// shoulder (the J2 axis) as the origin. Pure geometry, no Compose or ONNX; see SPEC.md section 4.
//
// Camera axes as elsewhere: x right, y down, z away from the camera, metres. "User" axes are the
// operator's, as the robot should copy them: forward (toward the camera), side, up.

object BodyMath {
    /** The robot's shoulder: the J2 axis, d1 = 123 mm above the base (PiPER user manual, modified DH). */
    val ROBOT_SHOULDER = Vec3(0.0, 0.0, 0.123)

    /**
     * Mirror-mode targets stay this far from the robot's shoulder: farther than [MIN_REACH_M] (closer is
     * inside the robot's own links) and nearer than [MAX_REACH_M] (the PiPER's arm from J2 to the flange
     * is about 0.63 m fully stretched, where it is singular).
     */
    const val MIN_REACH_M = 0.15
    const val MAX_REACH_M = 0.58

    /**
     * Distance from the camera to the torso: the pinhole relation px = f m / Z applied to the shoulder
     * width. A torso turned away from the camera looks narrower and so reads too far; calibrate facing it.
     */
    fun torsoDepth(focalPx: Double, shoulderWidthM: Double, shoulderWidthPx: Double): Double? =
        if (shoulderWidthPx < 4.0 || shoulderWidthM <= 0) null else focalPx * shoulderWidthM / shoulderWidthPx

    /**
     * The shoulder-to-hand vector in camera metres: each point back-projected at its own depth (the
     * shoulder at the torso's, the hand at the palm's), then subtracted. Negative z: the hand is closer
     * to the camera than the shoulder.
     */
    fun shoulderToHand(
        shoulderU: Double, shoulderV: Double, torsoZ: Double,
        handU: Double, handV: Double, handZ: Double,
        focalPx: Double, cx: Double, cy: Double,
    ): Vec3 = HandMath.backProject(handU, handV, handZ, focalPx, cx, cy) - HandMath.backProject(shoulderU, shoulderV, torsoZ, focalPx, cx, cy)

    /**
     * Camera axes to the operator's: forward = -z (reaching toward the camera), up = -y, side = x, or
     * -x when [invertSide] (the MIRROR ME / FACE ME switch, as in clutch mode). Returned as (forward, side, up).
     */
    fun toUser(v: Vec3, invertSide: Boolean): Vec3 = Vec3(-v.z, if (invertSide) -v.x else v.x, -v.y)

    /** The robot target for an operator reach (forward, side, up): the robot's shoulder plus [scale] times it, raised by [lift]. */
    fun mirrorTarget(reach: Vec3, scale: Double, lift: Double = 0.0): Vec3 =
        ROBOT_SHOULDER + Vec3(scale * reach.x, scale * reach.y, scale * reach.z + lift)

    /**
     * Keeps [p] between [rMin] and [rMax] from [origin], along the same direction. A target that would
     * sit on the origin itself is put straight ahead of it.
     */
    // Adapted from jaimelopezruiz/so101-markerless-teleop bridge/bridge.py: its radial mapping keeps every
    // target inside the reachable annulus around the shoulder pivot (WS_RMIN..MAP_RMAX) so none needs IK luck.
    fun clampReach(p: Vec3, origin: Vec3 = ROBOT_SHOULDER, rMin: Double = MIN_REACH_M, rMax: Double = MAX_REACH_M): Vec3 {
        val d = p - origin
        val r = d.norm()
        if (r in rMin..rMax) return p
        val dir = d.unit() ?: Vec3(1.0, 0.0, 0.0)
        return origin + dir * r.coerceIn(rMin, rMax)
    }

    /** The angle at the elbow, degrees (180 = straight), from pixel positions. */
    fun elbowAngleDeg(sx: Double, sy: Double, ex: Double, ey: Double, wx: Double, wy: Double): Double {
        val ax = sx - ex; val ay = sy - ey
        val bx = wx - ex; val by = wy - ey
        val n = hypot(ax, ay) * hypot(bx, by)
        if (n < 1e-9) return 0.0
        return Math.toDegrees(acos(((ax * bx + ay * by) / n).coerceIn(-1.0, 1.0)))
    }

    /** How far a pixel segment is from horizontal, degrees (0..90). */
    fun fromHorizontalDeg(dx: Double, dy: Double): Double {
        val a = abs(Math.toDegrees(atan2(dy, dx)))
        return if (a > 90) 180 - a else a
    }
}

/** One calibration result: the operator's shoulder-to-palm length and shoulder width, metres. */
data class BodySize(val armLengthM: Double, val shoulderWidthM: Double)

/**
 * "Calibrate arm": the operator faces the camera and holds the arm straight out sideways. Then the hand
 * is at about the torso's depth, which the palm gives in metres (independent of any assumed body size),
 * so the shoulder width and the shoulder-to-palm length can both be measured: length = pixels x Z / f.
 * Frames count only while the arm is straight (elbow above [minElbowDeg]) and level (within
 * [maxTiltDeg] of horizontal); [holdNanos] of them make a result, the median of the samples.
 */
class ArmCalibrator(
    private val holdNanos: Long = 2_000_000_000L,
    private val minElbowDeg: Double = 155.0,
    private val maxTiltDeg: Double = 25.0,
    /** A break longer than this (bent arm, lost hand) starts the count again. */
    private val graceNanos: Long = 400_000_000L,
) {
    private val arm = ArrayList<Double>()
    private val shoulders = ArrayList<Double>()
    private var start = Long.MIN_VALUE
    private var lastGood = Long.MIN_VALUE

    /** 0..1 through the hold. */
    fun progress(nowNanos: Long): Double = if (start == Long.MIN_VALUE) 0.0 else ((nowNanos - start).toDouble() / holdNanos).coerceIn(0.0, 1.0)

    fun reset() {
        arm.clear(); shoulders.clear()
        start = Long.MIN_VALUE
        lastGood = Long.MIN_VALUE
    }

    /** Why this frame doesn't count (null when it does), from the pixel positions of the shoulder, elbow and palm centre. */
    fun check(sx: Double, sy: Double, ex: Double, ey: Double, hx: Double, hy: Double): String? = when {
        BodyMath.elbowAngleDeg(sx, sy, ex, ey, hx, hy) < minElbowDeg -> "straighten your arm"
        BodyMath.fromHorizontalDeg(hx - sx, hy - sy) > maxTiltDeg -> "hold your arm level, out to the side"
        else -> null
    }

    /**
     * Feeds one frame. Returns the result once the arm has been held for long enough, else null. A null
     * [why] means the frame is good; otherwise it's the reason it isn't (and too long a run of those resets).
     */
    fun update(
        why: String?, nowNanos: Long,
        shoulderWidthPx: Double, armPx: Double, handZ: Double, focalPx: Double,
    ): BodySize? {
        if (why != null) {
            if (lastGood != Long.MIN_VALUE && nowNanos - lastGood > graceNanos) reset()
            return null
        }
        if (start == Long.MIN_VALUE) start = nowNanos
        lastGood = nowNanos
        arm += armPx * handZ / focalPx
        shoulders += shoulderWidthPx * handZ / focalPx
        if (nowNanos - start < holdNanos || arm.size < 10) return null
        // Adapted from jaimelopezruiz/so101-markerless-teleop bridge/bridge.py (calibrate): the median of
        // the samples, robust to the odd bad landmark frame.
        val out = BodySize(median(arm), median(shoulders))
        reset()
        return out
    }

    private fun median(v: List<Double>): Double = v.sorted().let { s -> if (s.size % 2 == 1) s[s.size / 2] else (s[s.size / 2 - 1] + s[s.size / 2]) / 2 }

    companion object {
        /** Plausible adult sizes: anything outside is a bad measurement, not a person. */
        val ARM_RANGE = 0.40..0.95
        val SHOULDER_RANGE = 0.25..0.55
    }
}
