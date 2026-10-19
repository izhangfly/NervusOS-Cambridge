package com.nervus.handteleop.math

import com.nervus.handteleop.SyntheticHand
import com.nervus.handteleop.math.OutlierGate.Verdict
import kotlin.math.abs
import kotlin.test.Test
import kotlin.test.assertEquals
import kotlin.test.assertNotNull
import kotlin.test.assertTrue

class HandMathTest {
    private val f = 448.0 // 0.7 x a 640-pixel frame
    private val cx = 320.0
    private val cy = 240.0

    private fun deg(d: Double) = Math.toRadians(d)

    private fun depthOf(q: Quat, at: Vec3): Double {
        val px = SyntheticHand.project(q, at, f, cx, cy)
        return assertNotNull(HandMath.depth(HandMath.palmBonesXy(SyntheticHand.turned(q)), HandMath.palmBonesXy(px), f))
    }

    private fun palmOf(q: Quat, left: Boolean = false) = assertNotNull(HandMath.palmFrame(SyntheticHand.turned(q), left))

    // ------------------------------------------------------------------ depth

    @Test
    fun `depth from the palm bones is unchanged by turning the hand in the image plane`() {
        val at = Vec3(0.12, -0.05, 0.55)
        for (a in 0 until 360 step 30) {
            val z = depthOf(Quat.about(Vec3.Z, deg(a.toDouble())), at)
            assertEquals(0.55, z, 1e-9, "in-plane turn of $a degrees")
        }
    }

    @Test
    fun `depth from the palm bones survives tilting the palm, where apparent size does not`() {
        // On the camera axis. Off it, a strongly tilted palm also shifts sideways in the image
        // (parallax), which camera-aligned world landmarks like these don't contain, so the error
        // there depends on how the real model frames them.
        val at = Vec3(0.0, 0.0, 0.50)
        val sizeFlat = HandMath.palmBonesXy(SyntheticHand.project(Quat.IDENTITY, at, f, cx, cy))
        val pitch = Vec3(1.0, 0.0, 0.0) // fingers toward / away from the camera
        val yaw = Vec3(0.0, 1.0, 0.0) // palm turned sideways
        for (axis in listOf(pitch, yaw)) {
            for (t in listOf(15.0, 30.0, 45.0, 50.0)) {
                val q = Quat.about(axis, deg(t))
                assertEquals(0.50, depthOf(q, at), 0.50 * 0.03, "tilt of $t degrees about $axis")
            }
        }
        // The old measure (pixel size against the size at engage) reads a pitch as a big move away.
        val naive = 0.50 * sizeFlat / HandMath.palmBonesXy(SyntheticHand.project(Quat.about(pitch, deg(45.0)), at, f, cx, cy))
        assertTrue(abs(naive - 0.50) > 0.10, "apparent size should be fooled by a 45 degree pitch (got $naive)")
    }

    @Test
    fun `back projection puts the image centre on the camera axis`() {
        assertEquals(Vec3(0.0, 0.0, 0.6), HandMath.backProject(cx, cy, 0.6, f, cx, cy))
        val p = HandMath.backProject(cx + 448.0, cy - 224.0, 0.5, f, cx, cy)
        assertEquals(0.5, p.x, 1e-12)
        assertEquals(-0.25, p.y, 1e-12)
    }

    // ------------------------------------------------------------------ palm frame and roll

    @Test
    fun `the palm frame is a proper rotation for either hand`() {
        for (left in listOf(false, true)) {
            val r = palmOf(Quat.about(Vec3(0.3, -0.5, 0.8).unit()!!, 0.7), left)
            for (i in 0..2) for (j in 0..2) {
                assertEquals(if (i == j) 1.0 else 0.0, r.column(i) dot r.column(j), 1e-12)
            }
            assertEquals(1.0, r.column(0) cross r.column(1) dot r.column(2), 1e-12, "right-handed")
        }
    }

    @Test
    fun `roll recovers a known turn about the fingers`() {
        val r0 = palmOf(Quat.IDENTITY)
        val fingers = r0.column(0)
        for (d in listOf(30.0, -70.0, 10.0)) {
            val r = palmOf(Quat.about(fingers, deg(d)))
            assertEquals(deg(d), HandMath.roll(r0, r), 1e-9)
        }
    }

    @Test
    fun `roll ignores a pure pitch or yaw of the hand`() {
        val r0 = palmOf(Quat.IDENTITY)
        val across = r0.column(1)
        val normal = r0.column(2)
        assertEquals(0.0, HandMath.roll(r0, palmOf(Quat.about(across, deg(35.0)))), 1e-9, "pitch")
        assertEquals(0.0, HandMath.roll(r0, palmOf(Quat.about(normal, deg(25.0)))), 1e-9, "yaw")
    }

    @Test
    fun `roll is read through a pitch either way round`() {
        val r0 = palmOf(Quat.IDENTITY)
        val rollQ = Quat.about(r0.column(0), deg(20.0))
        val pitchQ = Quat.about(r0.column(1), deg(30.0))
        assertEquals(deg(20.0), HandMath.roll(r0, palmOf(pitchQ * rollQ)), 1e-9)
        assertEquals(deg(20.0), HandMath.roll(r0, palmOf(rollQ * pitchQ)), 1e-9)
    }

    @Test
    fun `the left hand's flipped normal doesn't change the roll`() {
        val q = Quat.about(Vec3(0.2, -1.0, 0.1).unit()!!, deg(40.0))
        val right = HandMath.roll(palmOf(Quat.IDENTITY, false), palmOf(q, false))
        val left = HandMath.roll(palmOf(Quat.IDENTITY, true), palmOf(q, true))
        assertEquals(right, left, 1e-12)
    }

    @Test
    fun `a matrix and its quaternion are the same rotation`() {
        for (q in listOf(Quat.about(Vec3(1.0, 0.0, 0.0), 2.9), Quat.about(Vec3(0.0, 0.6, 0.8), -1.2), Quat.about(Vec3.Z, 3.1))) {
            val m = Mat3.ofColumns(q.rotate(Vec3(1.0, 0.0, 0.0)), q.rotate(Vec3(0.0, 1.0, 0.0)), q.rotate(Vec3.Z))
            val back = m.toQuat()
            val v = Vec3(0.3, -0.7, 0.2)
            val a = q.rotate(v)
            val b = back.rotate(v)
            assertEquals(0.0, (a - b).norm(), 1e-12)
        }
        // q and -q are one rotation, and have one twist.
        val q = Quat.about(Vec3.Z, deg(50.0))
        assertEquals(HandMath.twist(q, Vec3.Z), HandMath.twist(Quat(-q.x, -q.y, -q.z, -q.w), Vec3.Z), 1e-12)
    }

    @Test
    fun `turning about the tool's own z keeps the tool's axis`() {
        val q0 = Quat.about(Vec3(0.0, 1.0, 0.0), deg(90.0)) // tool z points along base +x
        val q = q0 * Quat.about(Vec3.Z, deg(30.0))
        val toolZ = q.rotate(Vec3.Z)
        assertEquals(0.0, (toolZ - q0.rotate(Vec3.Z)).norm(), 1e-12)
        assertEquals(deg(30.0), HandMath.twist(q * q0.conjugate(), q0.rotate(Vec3.Z)), 1e-12)
    }

    // ------------------------------------------------------------------ shaping

    @Test
    fun `the rate limiter moves at most its rate`() {
        val r = RateLimiter(deg(60.0))
        assertEquals(deg(3.0), r.step(deg(90.0), 0.05), 1e-12)
        repeat(9) { r.step(deg(90.0), 0.05) }
        assertEquals(deg(30.0), r.value, 1e-12)
        repeat(100) { r.step(deg(90.0), 0.05) }
        assertEquals(deg(90.0), r.value, 1e-12, "reaches the goal and stops there")
        assertEquals(deg(87.0), r.step(deg(-90.0), 0.05), 1e-12, "limited on the way back too")
        assertEquals(deg(87.0), r.step(deg(-90.0), 0.0), 1e-12, "no time, no movement")
    }

    @Test
    fun `deadband then clamp`() {
        val w = deg(5.0)
        assertEquals(0.0, HandMath.deadband(deg(3.0), w))
        assertEquals(0.0, HandMath.deadband(deg(-5.0), w))
        assertEquals(deg(5.0), HandMath.deadband(deg(10.0), w), 1e-12)
        assertEquals(deg(-5.0), HandMath.deadband(deg(-10.0), w), 1e-12)
        assertEquals(deg(0.01), HandMath.deadband(deg(5.01), w), 1e-12, "no step at the edge")

        val s = RollShaper()
        assertEquals(0.0, s.goal(deg(4.0)))
        assertEquals(deg(25.0), s.goal(deg(30.0)), 1e-12)
        assertEquals(deg(45.0), s.goal(deg(100.0)), 1e-12)
        assertEquals(deg(-45.0), s.goal(deg(-100.0)), 1e-12)
    }

    @Test
    fun `the roll shaper rate limits what it commands`() {
        val s = RollShaper()
        assertEquals(deg(3.0), s.step(deg(80.0), 0.05), 1e-12)
        repeat(40) { s.step(deg(80.0), 0.05) }
        assertEquals(deg(45.0), s.value, 1e-12, "settles on the clamp")
        s.reset()
        assertEquals(0.0, s.value)
    }

    // ------------------------------------------------------------------ outlier gate

    @Test
    fun `the gate drops a jump and resets after three in a row`() {
        val g = OutlierGate()
        assertEquals(Verdict.ACCEPT, g.check(Vec3(0.0, 0.0, 0.5), null), "first frame")
        assertEquals(Verdict.ACCEPT, g.check(Vec3(0.02, 0.0, 0.5), null))
        assertEquals(Verdict.DROP, g.check(Vec3(0.20, 0.0, 0.5), null), "18 cm in one frame")
        assertEquals(Verdict.ACCEPT, g.check(Vec3(0.03, 0.01, 0.5), null), "back near the last accepted frame")
        assertEquals(0, g.drops)
        assertEquals(Verdict.DROP, g.check(Vec3(0.3, 0.0, 0.5), null))
        assertEquals(Verdict.DROP, g.check(Vec3(0.3, 0.0, 0.5), null))
        assertEquals(Verdict.RESET, g.check(Vec3(0.3, 0.0, 0.5), null), "three in a row: the jump was real")
        assertEquals(Verdict.ACCEPT, g.check(Vec3(0.3, 0.0, 0.5), null), "starts again from there")
    }

    @Test
    fun `the gate drops a sudden wrist turn`() {
        val g = OutlierGate()
        val fingers = palmOf(Quat.IDENTITY).column(0)
        fun at(d: Double) = palmOf(Quat.about(fingers, deg(d)))
        val p = Vec3(0.0, 0.0, 0.5)
        assertEquals(Verdict.ACCEPT, g.check(p, at(0.0)))
        assertEquals(Verdict.ACCEPT, g.check(p, at(15.0)))
        assertEquals(Verdict.DROP, g.check(p, at(40.0)), "25 degrees from the last accepted frame")
        assertEquals(Verdict.ACCEPT, g.check(p, at(30.0)), "15 degrees from the last accepted frame")
        assertEquals(Verdict.ACCEPT, g.check(p, null), "roll not followed: only the position counts")
    }

    // ------------------------------------------------------------------ anchor slip

    @Test
    fun `anchor slip lets the arm turn back at once`() {
        val box = Box(0.08..0.46, -0.30..0.30, 0.10..0.55)
        val a = SlipAnchor(Vec3(0.30, 0.0, 0.30), box)
        assertVec(Vec3(0.35, 0.05, 0.35), a.target(Vec3(0.05, 0.05, 0.05)), "inside the box: no slip")
        assertEquals(Vec3(0.30, 0.0, 0.30), a.base)

        // Push 14 cm past the front edge, then bring the hand back 2 cm.
        assertEquals(0.46, a.target(Vec3(0.30, 0.0, 0.0)).x, 1e-12)
        assertEquals(0.16, a.base.x, 1e-12)
        assertEquals(0.44, a.target(Vec3(0.28, 0.0, 0.0)).x, 1e-12, "the arm comes back with the hand")
        // A plain clamp would still be pinned at the edge.
        assertEquals(0.46, (Vec3(0.30, 0.0, 0.30) + Vec3(0.28, 0.0, 0.0)).x.coerceIn(box.x), 1e-12)

        // Each axis slips on its own.
        assertVec(Vec3(0.44, -0.30, 0.55), a.target(Vec3(0.28, -0.50, 0.40)))
        assertVec(Vec3(0.44, -0.28, 0.53), a.target(Vec3(0.28, -0.48, 0.38)), "and comes back on each axis at once")
    }

    private fun assertVec(expected: Vec3, actual: Vec3, message: String? = null) =
        assertEquals(0.0, (expected - actual).norm(), 1e-12, "expected $expected, got $actual${message?.let { ": $it" } ?: ""}")
}
