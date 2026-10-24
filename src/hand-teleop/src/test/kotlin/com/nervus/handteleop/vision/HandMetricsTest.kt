package com.nervus.handteleop.vision

import com.nervus.handteleop.SyntheticHand
import com.nervus.handteleop.math.Quat
import com.nervus.handteleop.math.Vec3
import kotlin.math.hypot
import kotlin.test.Test
import kotlin.test.assertEquals
import kotlin.test.assertNotNull
import kotlin.test.assertTrue

class HandMetricsTest {
    private val f = 0.7 * 1280 // a 1280x720 frame
    private val cx = 640.0
    private val cy = 360.0

    private fun seen(q: Quat, at: Vec3): HandLandmarks {
        val px = SyntheticHand.project(q, at, f, cx, cy).map { Kp(it.x.toFloat(), it.y.toFloat(), 1f) }
        return HandLandmarks(px, 1f, HandLandmarks.Side.RIGHT, SyntheticHand.turned(q), 0.5f)
    }

    /** The old 2-D ratio: thumb tip to index tip over wrist to middle knuckle, in pixels. */
    private fun pinch2d(h: HandLandmarks): Double {
        fun d(a: Int, b: Int) = hypot((h[a].x - h[b].x).toDouble(), (h[a].y - h[b].y).toDouble())
        return d(HandLandmarks.THUMB_TIP, HandLandmarks.INDEX_TIP) / d(HandLandmarks.WRIST, HandLandmarks.MIDDLE_MCP)
    }

    @Test
    fun `pinch and distance hold when the palm tilts`() {
        val at = Vec3(0.0, 0.0, 0.45)
        val flat = seen(Quat.IDENTITY, at)
        val tilted = seen(Quat.about(Vec3(1.0, 0.0, 0.0), Math.toRadians(45.0)), at)
        val a = HandMetrics.of(flat)
        val b = HandMetrics.of(tilted)
        assertEquals(a.pinchIndex, b.pinchIndex, 1e-5f)
        assertEquals(a.pinchFour, b.pinchFour, 1e-5f)
        assertTrue(kotlin.math.abs(pinch2d(flat) - pinch2d(tilted)) > 0.1, "the 2-D ratio is fooled by the tilt")
        assertEquals(0.45, assertNotNull(a.depth(f)), 1e-9)
        assertEquals(0.45, assertNotNull(b.depth(f)), 0.45 * 0.03)
    }
}
