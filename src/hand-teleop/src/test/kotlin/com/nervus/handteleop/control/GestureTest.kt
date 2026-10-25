package com.nervus.handteleop.control

import com.nervus.handteleop.HandShapes
import com.nervus.handteleop.math.Quat
import com.nervus.handteleop.math.Vec3
import kotlin.test.Test
import kotlin.test.assertEquals
import kotlin.test.assertFalse
import kotlin.test.assertTrue

class GestureTest {
    private val ms = 1_000_000L

    @Test
    fun `a fist is a fist, an open hand is open, and pinches are neither`() {
        assertEquals(HandGesture.Shape.OPEN_HAND, HandGesture.shape(HandShapes.open))
        assertEquals(HandGesture.Shape.FIST, HandGesture.shape(HandShapes.fist))
        assertFalse(HandGesture.isFist(HandShapes.pinch), "thumb to index with the others out")
        assertFalse(HandGesture.isFist(HandShapes.beak), "thumb to all four fingertips")
        assertEquals(listOf(true, true, true), HandGesture.fingersExtended(HandShapes.pinch).drop(1), "middle, ring and little stay out in a pinch")
    }

    @Test
    fun `the shape doesn't change when the hand tilts or turns`() {
        for (q in listOf(Quat.about(Vec3(1.0, 0.0, 0.0), Math.toRadians(55.0)), Quat.about(Vec3(0.0, 1.0, 0.0), Math.toRadians(-70.0)), Quat.about(Vec3.Z, 2.0))) {
            assertTrue(HandGesture.isFist(HandShapes.fist.map { q.rotate(it) }))
            assertFalse(HandGesture.isFist(HandShapes.pinch.map { q.rotate(it) }))
            assertFalse(HandGesture.isFist(HandShapes.open.map { q.rotate(it) }))
        }
    }

    @Test
    fun `the hold timer fires once after 1_5 s and re-arms only after letting go`() {
        val t = HoldTimer()
        var now = 0L
        var fired = 0
        repeat(29) { if (t.update(true, now)) fired++; now += 50 * ms } // 0 .. 1.40 s
        assertEquals(0, fired)
        assertEquals(1.4 / 1.5, t.progress(now - 50 * ms), 1e-9)
        if (t.update(true, now)) fired++ // 1.45 s
        now += 50 * ms
        if (t.update(true, now)) fired++ // 1.50 s: fires
        assertEquals(1, fired)
        assertTrue(t.spent)
        assertEquals(0.0, t.progress(now), "nothing left to count once fired")
        repeat(60) { now += 50 * ms; if (t.update(true, now)) fired++ }
        assertEquals(1, fired, "a long fist doesn't fire again")
        now += 50 * ms; t.update(false, now)
        now += 200 * ms; t.update(false, now)
        assertFalse(t.held)
        repeat(31) { if (t.update(true, now)) fired++; now += 50 * ms }
        assertEquals(2, fired, "a new fist fires again")
    }

    @Test
    fun `a flicker shorter than the grace doesn't reset the count, a longer gap does`() {
        val t = HoldTimer()
        var now = 0L
        repeat(20) { t.update(true, now); now += 50 * ms } // 1.0 s held
        t.update(false, now); now += 50 * ms // two frames lost: 100 ms
        t.update(false, now); now += 50 * ms
        assertTrue(t.held)
        var fired = false
        repeat(10) { if (t.update(true, now)) fired = true; now += 50 * ms }
        assertTrue(fired, "the count carried on through the flicker")

        val u = HoldTimer()
        now = 0L
        repeat(20) { u.update(true, now); now += 50 * ms }
        repeat(5) { u.update(false, now); now += 50 * ms } // 250 ms off
        assertFalse(u.held)
        fired = false
        repeat(20) { if (u.update(true, now)) fired = true; now += 50 * ms }
        assertFalse(fired, "started again from zero: 1.0 s isn't enough")
    }
}
