package com.nervus.handteleop.math

import kotlin.math.abs
import kotlin.math.atan2
import kotlin.math.cos
import kotlin.math.hypot
import kotlin.math.sin
import kotlin.math.sqrt

// The geometry behind Hand Teleop's mapping, free of Compose and ONNX so it can be tested on its own.
// The methods follow arXiv 2508.14994 (CC BY 4.0) and github.com/Joeclinton1/hand-teleop (Apache-2.0):
// metric depth from the palm's own bones, a palm frame from MediaPipe's world landmarks, and only the
// roll part of the wrist's rotation.
//
// Camera axes throughout: x right, y down (the image's axes), z away from the camera. Lengths are in
// metres unless a name says pixels; angles are in radians.

/** A 3-D vector. */
data class Vec3(val x: Double, val y: Double, val z: Double) {
    operator fun plus(o: Vec3) = Vec3(x + o.x, y + o.y, z + o.z)
    operator fun minus(o: Vec3) = Vec3(x - o.x, y - o.y, z - o.z)
    operator fun times(k: Double) = Vec3(x * k, y * k, z * k)
    operator fun unaryMinus() = Vec3(-x, -y, -z)
    infix fun dot(o: Vec3) = x * o.x + y * o.y + z * o.z
    infix fun cross(o: Vec3) = Vec3(y * o.z - z * o.y, z * o.x - x * o.z, x * o.y - y * o.x)
    fun norm() = sqrt(this dot this)

    /** Length of the part in the image plane (x and y only). */
    fun normXy() = hypot(x, y)

    /** This direction at length 1, or null if it has none. */
    fun unit(): Vec3? {
        val n = norm()
        return if (n < 1e-9) null else this * (1.0 / n)
    }

    companion object {
        val ZERO = Vec3(0.0, 0.0, 0.0)
        val Z = Vec3(0.0, 0.0, 1.0)
    }
}

/** A 3x3 matrix, row-major. Here it is always a rotation. */
class Mat3(private val m: DoubleArray) {
    operator fun get(r: Int, c: Int) = m[3 * r + c]
    fun column(c: Int) = Vec3(m[c], m[3 + c], m[6 + c])
    fun transpose() = Mat3(DoubleArray(9) { m[3 * (it % 3) + it / 3] })

    operator fun times(o: Mat3) = Mat3(DoubleArray(9) {
        val r = it / 3
        val c = it % 3
        this[r, 0] * o[0, c] + this[r, 1] * o[1, c] + this[r, 2] * o[2, c]
    })

    operator fun times(v: Vec3) = Vec3(
        m[0] * v.x + m[1] * v.y + m[2] * v.z,
        m[3] * v.x + m[4] * v.y + m[5] * v.z,
        m[6] * v.x + m[7] * v.y + m[8] * v.z,
    )

    /** The same rotation as a unit quaternion (Shepperd's method, stable at any angle). */
    fun toQuat(): Quat {
        val tr = m[0] + m[4] + m[8]
        return when {
            tr > 0 -> {
                val s = sqrt(tr + 1.0) * 2
                Quat((m[7] - m[5]) / s, (m[2] - m[6]) / s, (m[3] - m[1]) / s, 0.25 * s)
            }
            m[0] > m[4] && m[0] > m[8] -> {
                val s = sqrt(1.0 + m[0] - m[4] - m[8]) * 2
                Quat(0.25 * s, (m[1] + m[3]) / s, (m[2] + m[6]) / s, (m[7] - m[5]) / s)
            }
            m[4] > m[8] -> {
                val s = sqrt(1.0 + m[4] - m[0] - m[8]) * 2
                Quat((m[1] + m[3]) / s, 0.25 * s, (m[5] + m[7]) / s, (m[2] - m[6]) / s)
            }
            else -> {
                val s = sqrt(1.0 + m[8] - m[0] - m[4]) * 2
                Quat((m[2] + m[6]) / s, (m[5] + m[7]) / s, 0.25 * s, (m[3] - m[1]) / s)
            }
        }
    }

    companion object {
        fun ofColumns(a: Vec3, b: Vec3, c: Vec3) = Mat3(doubleArrayOf(a.x, b.x, c.x, a.y, b.y, c.y, a.z, b.z, c.z))
    }
}

/** A rotation quaternion, in the order Manipulator@1's Pose uses (x, y, z, w). */
data class Quat(val x: Double, val y: Double, val z: Double, val w: Double) {
    /**
     * Hamilton product: [o] first, then this. For an orientation q, q * r turns by r about q's own
     * axes, which is how the tool turns about its own z.
     */
    operator fun times(o: Quat) = Quat(
        w * o.x + x * o.w + y * o.z - z * o.y,
        w * o.y - x * o.z + y * o.w + z * o.x,
        w * o.z + x * o.y - y * o.x + z * o.w,
        w * o.w - x * o.x - y * o.y - z * o.z,
    )

    fun conjugate() = Quat(-x, -y, -z, w)

    /** At length 1, or null for a zero quaternion (a pose that carries no orientation). */
    fun unit(): Quat? {
        val n = sqrt(x * x + y * y + z * z + w * w)
        return if (n < 1e-9) null else Quat(x / n, y / n, z / n, w / n)
    }

    fun rotate(v: Vec3): Vec3 {
        val r = this * Quat(v.x, v.y, v.z, 0.0) * conjugate()
        return Vec3(r.x, r.y, r.z)
    }

    companion object {
        val IDENTITY = Quat(0.0, 0.0, 0.0, 1.0)

        /** A right-handed turn of [angle] about the unit vector [axis]. */
        fun about(axis: Vec3, angle: Double): Quat {
            val s = sin(angle / 2)
            return Quat(axis.x * s, axis.y * s, axis.z * s, cos(angle / 2))
        }
    }
}

object HandMath {
    /**
     * The palm's rigid bones: wrist to each knuckle, and index knuckle to little-finger knuckle. They
     * don't bend with the fingers, so their summed length is a steady ruler for the hand.
     */
    val PALM_BONES = listOf(0 to 5, 0 to 9, 0 to 13, 0 to 17, 5 to 17)

    /** Wrist and the four knuckles: where the palm centre and the palm frame come from. */
    val PALM_POINTS = intArrayOf(0, 5, 9, 13, 17)

    /**
     * Rotates a world landmark from the hand crop's axes into the frame's. The model gives world
     * landmarks in the axes of the rotated crop it was shown, so without this they would turn every
     * time the crop follows the hand. [c], [s] are the crop's rotation as in Image.rotatedCropNhwc: a
     * crop offset (dx, dy) lands at frame offset (dx c - dy s, dx s + dy c). Depth is along the camera
     * axis in both, so z stays.
     */
    fun cropToFrame(v: Vec3, c: Double, s: Double) = Vec3(v.x * c - v.y * s, v.x * s + v.y * c, v.z)

    /**
     * Summed length of [PALM_BONES] in the image plane. Takes frame pixels (z ignored) or world
     * landmarks (metres, depth dropped): the same bones as the camera sees them, either way.
     */
    fun palmBonesXy(points: List<Vec3>): Double = PALM_BONES.sumOf { (i, j) -> (points[i] - points[j]).normXy() }

    /**
     * Distance from the camera to the hand, metres, from the pinhole relation px = f m / Z applied to
     * the palm bones: [bonesPx] in the image, [bonesWorldXy] from the world landmarks with depth
     * dropped. Tilting the palm shortens both alike, so unlike apparent hand size the ratio doesn't
     * read a tilt as a change in distance. Null when there is too little hand to measure.
     */
    fun depth(bonesWorldXy: Double, bonesPx: Double, focalPx: Double): Double? =
        if (bonesPx < 1.0 || bonesWorldXy < 1e-4) null else focalPx * bonesWorldXy / bonesPx

    /** Pixel (u, v) seen at depth [z], back through a pinhole with focal length [focalPx] and centre (cx, cy). */
    fun backProject(u: Double, v: Double, z: Double, focalPx: Double, cx: Double, cy: Double) =
        Vec3((u - cx) * z / focalPx, (v - cy) * z / focalPx, z)

    /**
     * The palm's frame, as columns:
     *   f, wrist to the centre of the knuckles (along the fingers);
     *   a, across the knuckles toward the index finger, made square to f;
     *   n = f x a, the palm normal, negated for a left hand (the mirror image) so it means the same
     *   side of the hand for both.
     * Null when the points are degenerate.
     */
    fun palmFrame(wrist: Vec3, index: Vec3, middle: Vec3, ring: Vec3, little: Vec3, left: Boolean): Mat3? {
        val f = ((index + middle + ring + little) * 0.25 - wrist).unit() ?: return null
        val across = (index - little).unit() ?: return null
        val n0 = (f cross across).unit() ?: return null
        val n = if (left) -n0 else n0
        return Mat3.ofColumns(f, n cross f, n)
    }

    /** [palmFrame] from all 21 world landmarks. */
    fun palmFrame(world: List<Vec3>, left: Boolean): Mat3? =
        palmFrame(world[0], world[5], world[9], world[13], world[17], left)

    /**
     * How far [q] turns about the unit [axis] (swing-twist decomposition): the twist alone, with the
     * swing that tilts the axis left out. Right-handed, -pi..pi.
     */
    fun twist(q: Quat, axis: Vec3): Double = wrap(2.0 * atan2(q.x * axis.x + q.y * axis.y + q.z * axis.z, q.w))

    /**
     * Wrist roll: how far the palm turned about the anchor's finger axis to get from [anchor] to
     * [now]. Pitching or waving the hand turns the finger axis itself, which is swing, so it reads 0.
     */
    fun roll(anchor: Mat3, now: Mat3): Double = twist((now * anchor.transpose()).toQuat(), anchor.column(0))

    /** Zero inside +-[width], and from there on continuing from zero, so there is no step at the edge. */
    fun deadband(x: Double, width: Double): Double = when {
        x > width -> x - width
        x < -width -> x + width
        else -> 0.0
    }

    /** An angle brought into -pi..pi. */
    fun wrap(a: Double): Double = Math.IEEEremainder(a, 2 * Math.PI)
}

/** Follows a goal no faster than [maxRate] per second. */
class RateLimiter(private val maxRate: Double) {
    var value = 0.0
        private set

    fun reset(to: Double = 0.0) {
        value = to
    }

    fun step(goal: Double, dtSeconds: Double): Double {
        val most = maxRate * dtSeconds.coerceAtLeast(0.0)
        value += (goal - value).coerceIn(-most, most)
        return value
    }
}

/**
 * Hand roll to tool roll: a deadband so a steady hand doesn't wobble the tool, then gain, a clamp well
 * inside the wrist's range, and a rate limit so that anything odd turns the tool slowly, not at once.
 */
class RollShaper(
    private val deadband: Double = Math.toRadians(5.0),
    private val gain: Double = 1.0,
    private val limit: Double = Math.toRadians(45.0),
    maxRate: Double = Math.toRadians(60.0),
) {
    private val limiter = RateLimiter(maxRate)

    /** The roll last commanded. */
    val value: Double get() = limiter.value

    fun reset() = limiter.reset()

    /** Where the tool should end up for [handRoll], before rate limiting. */
    fun goal(handRoll: Double) = (HandMath.deadband(handRoll, deadband) * gain).coerceIn(-limit, limit)

    fun step(handRoll: Double, dtSeconds: Double) = limiter.step(goal(handRoll), dtSeconds)
}

/**
 * Drops vision frames that jump. A hand doesn't move 5 cm or turn 20 degrees in one frame (50 ms) on
 * purpose, but a landmark glitch (fingers swapped, the crop on the wrong hand) can. Each frame is
 * compared with the last accepted one, not the previous one, so a run of bad frames can't drag the
 * reference along. After [maxDrops] in a row the jump is taken as real: [check] says RESET, and the
 * caller holds and re-anchors as it does when the hand is lost.
 */
class OutlierGate(
    private val maxStep: Double = 0.05,
    private val maxRoll: Double = Math.toRadians(20.0),
    private val maxDrops: Int = 3,
) {
    enum class Verdict { ACCEPT, DROP, RESET }

    private var lastPos: Vec3? = null
    private var lastPalm: Mat3? = null

    /** Frames dropped in a row so far. */
    var drops = 0
        private set

    fun reset() {
        lastPos = null
        lastPalm = null
        drops = 0
    }

    /** [pos]: the hand in camera metres; [palm]: its palm frame, or null when roll isn't followed. */
    fun check(pos: Vec3, palm: Mat3?): Verdict {
        val p = lastPos
        val q = lastPalm
        val jumped = p != null && ((pos - p).norm() > maxStep || (palm != null && q != null && abs(HandMath.roll(q, palm)) > maxRoll))
        if (!jumped) {
            lastPos = pos
            lastPalm = palm
            drops = 0
            return Verdict.ACCEPT
        }
        if (++drops >= maxDrops) {
            reset()
            return Verdict.RESET
        }
        return Verdict.DROP
    }
}

/** An axis-aligned box, such as the arm's teleop workspace in base_link (metres). */
class Box(
    val x: ClosedFloatingPointRange<Double>,
    val y: ClosedFloatingPointRange<Double>,
    val z: ClosedFloatingPointRange<Double>,
) {
    operator fun contains(p: Vec3) = p.x in x && p.y in y && p.z in z
    fun clamp(p: Vec3) = Vec3(p.x.coerceIn(x), p.y.coerceIn(y), p.z.coerceIn(z))
}

/**
 * Where the hand's anchor lands on the arm, slipping at the edge of [box]. Without the slip, pushing
 * the hand 10 cm past the edge banks 10 cm that the hand has to come back before the arm moves again.
 * With it, the anchor shifts by whatever the box clamps off, so the arm follows as soon as the hand
 * turns back.
 */
class SlipAnchor(base: Vec3, private val box: Box) {
    var base = base
        private set

    /** The target for a hand movement of [offset] (already in base_link metres) from the anchor. */
    fun target(offset: Vec3): Vec3 {
        val raw = base + offset
        val clamped = box.clamp(raw)
        base += clamped - raw
        return clamped
    }
}
