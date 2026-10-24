package com.nervus.handteleop.vision

import com.nervus.handteleop.math.HandMath
import com.nervus.handteleop.math.Vec3
import kotlin.math.atan2
import kotlin.math.hypot
import kotlin.math.max

/** What one pass of the vision pipeline found in a frame. */
class Observation(
    val frame: RgbFrame,
    val body: BodyPose?,
    val hands: List<HandLandmarks>,
    val poseMs: Float,
    val handMs: Float,
    /** System.nanoTime() when processing finished: how fresh this view of the hand is. */
    val atNanos: Long = System.nanoTime(),
)

/**
 * Body pose every frame, then the hands.
 *
 * A hand is found the way MediaPipe tracks one, but seeded from the body instead of a palm
 * detector: the first crop is aligned with the forearm (elbow to wrist) just past the wrist; once
 * landmarks are found, the next frame's crop is derived from them, which follows the hand closely
 * even when the wrist keypoint lags. If the hand is lost, it falls back to the forearm again.
 */
class Tracker(private val pose: PoseEstimator, private val hand: HandEstimator) {
    private val track = HashMap<HandLandmarks.Side, HandRoi>()

    fun process(frame: RgbFrame): Observation {
        val t0 = System.nanoTime()
        val body = pose.run(frame)
        val t1 = System.nanoTime()
        val hands = ArrayList<HandLandmarks>(2)
        if (body != null) {
            for (side in HandLandmarks.Side.values()) {
                val (e, w) = when (side) {
                    HandLandmarks.Side.LEFT -> BodyPose.L_ELBOW to BodyPose.L_WRIST
                    HandLandmarks.Side.RIGHT -> BodyPose.R_ELBOW to BodyPose.R_WRIST
                }
                val elbow = body[e]
                val wrist = body[w]
                val roi = track[side] ?: (if (wrist.score > 0.3f && elbow.score > 0.3f) roiFromArm(elbow, wrist) else null)
                if (roi == null) {
                    track.remove(side)
                    continue
                }
                val lm = hand.run(frame, roi, side)
                if (lm != null) {
                    hands += lm
                    track[side] = roiFromLandmarks(lm)
                } else {
                    track.remove(side)
                }
            }
        } else {
            track.clear()
        }
        val t2 = System.nanoTime()
        return Observation(frame, body, hands, (t1 - t0) / 1e6f, (t2 - t1) / 1e6f)
    }

    private fun roiFromArm(elbow: Kp, wrist: Kp): HandRoi? {
        val vx = wrist.x - elbow.x
        val vy = wrist.y - elbow.y
        val len = hypot(vx, vy)
        if (len < 15f) return null
        return HandRoi(wrist.x + vx * 0.35f, wrist.y + vy * 0.35f, 1.5f * len, atan2(vy, vx))
    }

    private fun roiFromLandmarks(h: HandLandmarks): HandRoi {
        val w = h[HandLandmarks.WRIST]
        val m = h[HandLandmarks.MIDDLE_MCP]
        val vx = m.x - w.x
        val vy = m.y - w.y
        var cx = 0f
        var cy = 0f
        for (i in intArrayOf(0, 5, 9, 17)) {
            cx += h[i].x / 4f
            cy += h[i].y / 4f
        }
        var minX = Float.MAX_VALUE; var maxX = -Float.MAX_VALUE
        var minY = Float.MAX_VALUE; var maxY = -Float.MAX_VALUE
        for (p in h.points) {
            minX = minOf(minX, p.x); maxX = maxOf(maxX, p.x)
            minY = minOf(minY, p.y); maxY = maxOf(maxY, p.y)
        }
        val span = max(maxX - minX, maxY - minY)
        return HandRoi(cx + vx * 0.25f, cy + vy * 0.25f, max(2.2f * hypot(vx, vy), 1.35f * span), atan2(vy, vx))
    }
}

/** Measurements of one hand used for control. */
class HandMetrics(
    /** Palm centre (wrist and the four knuckles), frame pixels. */
    val palmX: Float,
    val palmY: Float,
    /** The palm bones' summed length in the image, pixels ([HandMath.palmBonesXy]). */
    val bonesPx: Double,
    /** The same bones in the world landmarks with depth dropped, metres: what the camera sees of them. */
    val bonesM: Double,
    /**
     * Thumb tip to index tip in 3-D, over wrist to middle knuckle in 3-D: the hand's own ruler, so the
     * ratio changes with neither distance nor how the palm is tilted (the 2-D one did, with tilt).
     */
    val pinchIndex: Float,
    /** Thumb tip to the centre of the four fingertips, the same way. */
    val pinchFour: Float,
) {
    /** Distance from the camera to the hand, metres, for a focal length of [focalPx]; null if unmeasurable. */
    fun depth(focalPx: Double): Double? = HandMath.depth(bonesM, bonesPx, focalPx)

    companion object {
        fun of(h: HandLandmarks): HandMetrics {
            var px = 0f
            var py = 0f
            for (i in HandMath.PALM_POINTS) {
                px += h[i].x / 5f
                py += h[i].y / 5f
            }
            val pixels = h.points.map { Vec3(it.x.toDouble(), it.y.toDouble(), 0.0) }
            val w = h.world
            val ruler = max(1e-3, (w[HandLandmarks.MIDDLE_MCP] - w[HandLandmarks.WRIST]).norm())
            val tips = (w[HandLandmarks.INDEX_TIP] + w[HandLandmarks.MIDDLE_TIP] + w[HandLandmarks.RING_TIP] + w[HandLandmarks.PINKY_TIP]) * 0.25
            val thumb = w[HandLandmarks.THUMB_TIP]
            return HandMetrics(
                px, py,
                HandMath.palmBonesXy(pixels),
                HandMath.palmBonesXy(w),
                ((thumb - w[HandLandmarks.INDEX_TIP]).norm() / ruler).toFloat(),
                ((thumb - tips).norm() / ruler).toFloat(),
            )
        }
    }
}

/**
 * The One Euro filter (Casiez et al. 2012): heavy smoothing when the hand is still, little lag when
 * it moves fast. The standard choice for jitter-free pointing from noisy landmarks.
 */
class OneEuro(private val minCutoff: Double = 1.2, private val beta: Double = 0.02, private val dCutoff: Double = 1.0) {
    private var x: Double = Double.NaN
    private var dx: Double = 0.0
    private var lastT: Long = 0

    fun reset() {
        x = Double.NaN
        dx = 0.0
    }

    fun filter(value: Double, tNanos: Long): Double {
        if (x.isNaN()) {
            x = value
            lastT = tNanos
            return value
        }
        val dt = ((tNanos - lastT) / 1e9).coerceIn(1e-3, 0.5)
        lastT = tNanos
        val a = alpha(dCutoff, dt)
        dx = a * ((value - x) / dt) + (1 - a) * dx
        val cutoff = minCutoff + beta * kotlin.math.abs(dx)
        x += alpha(cutoff, dt) * (value - x)
        return x
    }

    private fun alpha(cutoff: Double, dt: Double): Double {
        val tau = 1.0 / (2 * Math.PI * cutoff)
        return 1.0 / (1.0 + tau / dt)
    }
}

/** [OneEuro] on each axis of a point. */
class OneEuro3(minCutoff: Double, beta: Double) {
    private val x = OneEuro(minCutoff, beta)
    private val y = OneEuro(minCutoff, beta)
    private val z = OneEuro(minCutoff, beta)

    fun reset() {
        x.reset(); y.reset(); z.reset()
    }

    fun filter(v: Vec3, tNanos: Long) = Vec3(x.filter(v.x, tNanos), y.filter(v.y, tNanos), z.filter(v.z, tNanos))
}
