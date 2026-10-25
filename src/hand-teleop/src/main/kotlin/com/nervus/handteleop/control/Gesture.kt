package com.nervus.handteleop.control

import com.nervus.handteleop.math.Vec3

/**
 * Hand shapes from the 21 world landmarks (metres, 3-D). Using the 3-D landmarks rather than image
 * points means tilting the hand toward or away from the camera doesn't change the answer.
 */
object HandGesture {
    enum class Shape { OPEN_HAND, FIST, OTHER }

    // (tip, MCP knuckle) for index, middle, ring and little finger.
    private val FINGERS = listOf(8 to 5, 12 to 9, 16 to 13, 20 to 17)

    // Adapted from agilexrobotics/AgileX-College/piper/handpose_det/scripts/handpose_det_new_ik.py
    // (recognize_gesture): a finger is extended when its tip is farther from the wrist than its MCP
    // joint, and the thumb when its tip is farther from the wrist than its IP joint.
    fun fingersExtended(w: List<Vec3>): List<Boolean> =
        FINGERS.map { (tip, mcp) -> (w[tip] - w[0]).norm() > (w[mcp] - w[0]).norm() }

    fun thumbExtended(w: List<Vec3>): Boolean = (w[4] - w[0]).norm() > (w[3] - w[0]).norm()

    // Adapted from the same recognize_gesture: all five extended is an open hand, none of the four
    // fingers extended is a fist (the thumb doesn't count: it can sit inside or outside a fist).
    fun shape(w: List<Vec3>): Shape {
        val ext = fingersExtended(w)
        return when {
            ext.all { it } && thumbExtended(w) -> Shape.OPEN_HAND
            ext.none { it } -> Shape.FIST
            else -> Shape.OTHER
        }
    }

    fun isFist(w: List<Vec3>) = shape(w) == Shape.FIST
}

/**
 * A gesture that has to be held: [update] says true once, when it has been held for [holdNanos].
 * Brief dropouts (up to [graceNanos], landmark flicker) don't reset it; after it fires, the gesture has
 * to be let go before it can fire again, so one long fist can't engage and then disengage.
 */
class HoldTimer(private val holdNanos: Long = 1_500_000_000L, private val graceNanos: Long = 150_000_000L) {
    private var start = NONE
    private var lastOn = NONE
    private var fired = false

    /** The gesture is being held (dropouts within the grace included). */
    val held: Boolean get() = start != NONE

    /** It fired and hasn't been let go since. */
    val spent: Boolean get() = fired

    /** Feeds one observation: is the gesture showing at [nowNanos]? True on the update where it completes. */
    fun update(on: Boolean, nowNanos: Long): Boolean {
        if (on) {
            lastOn = nowNanos
            if (start == NONE) start = nowNanos
            if (!fired && nowNanos - start >= holdNanos) {
                fired = true
                return true
            }
        } else if (start != NONE && nowNanos - lastOn > graceNanos) {
            reset()
        }
        return false
    }

    /** How far through the hold, 0..1; 0 when not held or already fired (nothing left to count down). */
    fun progress(nowNanos: Long): Double =
        if (start == NONE || fired) 0.0 else ((nowNanos - start).toDouble() / holdNanos).coerceIn(0.0, 1.0)

    fun reset() {
        start = NONE
        lastOn = NONE
        fired = false
    }

    private companion object {
        const val NONE = Long.MIN_VALUE
    }
}
