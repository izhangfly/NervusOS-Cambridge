package com.nervus.handteleop.math

import com.nervus.handteleop.SyntheticPerson
import com.nervus.handteleop.control.Teleop
import com.nervus.handteleop.math.OutlierGate.Verdict
import com.nervus.handteleop.vision.BodyPose
import com.nervus.handteleop.vision.HandMetrics
import kotlin.test.Test
import kotlin.test.assertEquals
import kotlin.test.assertNotNull
import kotlin.test.assertNull
import kotlin.test.assertTrue

class BodyMathTest {
    private val f = SyntheticPerson.F
    private val cx = SyntheticPerson.W / 2.0
    private val cy = SyntheticPerson.H / 2.0

    /** The shoulder-to-hand vector as the app measures it from the synthetic person's keypoints and hand. */
    private fun measured(depth: Double, reach: Vec3, q: Quat = Quat.IDENTITY, assumedWidth: Double = 0.37): Vec3 {
        val o = SyntheticPerson.reaching(depth, reach, 0L, q)
        val b = o.body!!
        val s = b[BodyPose.R_SHOULDER]
        val other = b[BodyPose.L_SHOULDER]
        val wPx = kotlin.math.hypot((s.x - other.x).toDouble(), (s.y - other.y).toDouble())
        val zt = assertNotNull(BodyMath.torsoDepth(f, assumedWidth, wPx))
        val m = HandMetrics.of(o.hands[0])
        val zh = assertNotNull(m.depth(f))
        return BodyMath.shoulderToHand(s.x.toDouble(), s.y.toDouble(), zt, m.palmX.toDouble(), m.palmY.toDouble(), zh, f, cx, cy)
    }

    private fun assertVec(expected: Vec3, actual: Vec3, tol: Double, what: String) =
        assertTrue((expected - actual).norm() <= tol, "$what: expected $expected, got $actual (off by ${"%.1f".format((expected - actual).norm() * 100)} cm)")

    @Test
    fun `the torso's distance comes from the shoulder width`() {
        for (z in listOf(1.0, 1.6, 2.4)) {
            val (r, l) = SyntheticPerson.shoulders(z)
            val wPx = (SyntheticPerson.px(l).x - SyntheticPerson.px(r).x).toDouble()
            assertEquals(z, assertNotNull(BodyMath.torsoDepth(f, 0.37, wPx)), 1e-4)
        }
        assertNull(BodyMath.torsoDepth(f, 0.37, 0.0))
    }

    @Test
    fun `shoulder to hand is recovered in metres for an arm forward, up and sideways, near and far`() {
        val reaches = mapOf(
            "forward" to Vec3(0.0, 0.0, -0.50), // toward the camera
            "up" to Vec3(0.0, -0.50, -0.10),
            "sideways" to Vec3(-0.55, 0.0, 0.0), // the operator's right, the camera's left
            "down and in" to Vec3(0.10, 0.25, -0.30),
        )
        for (depth in listOf(1.2, 1.8, 2.5)) {
            for ((name, v) in reaches) {
                // The only modelling error left is perspective between the palm's points: millimetres.
                assertVec(v, measured(depth, v), 0.01, "$name at ${depth} m")
            }
        }
    }

    @Test
    fun `a tilted palm barely changes it`() {
        // The hand on the camera axis (in front of the chest): there the palm-bone depth holds to about 2 %
        // for a 40 to 45 degree tilt. Off the axis, a strongly pitched palm reads several per cent long in
        // this synthetic, camera-aligned model (perspective; SPEC.md, "Limits"), which is why it isn't asserted.
        val v = Vec3(0.185, 0.10, -0.40)
        for (q in listOf(Quat.about(Vec3(1.0, 0.0, 0.0), Math.toRadians(40.0)), Quat.about(Vec3(0.0, 1.0, 0.0), Math.toRadians(-45.0)), Quat.about(Vec3.Z, 1.2))) {
            assertVec(v, measured(1.6, v, q), 0.04, "tilted $q")
        }
    }

    @Test
    fun `a wrong shoulder width scales the torso's distance, and so the forward reach`() {
        // 10 % too wide an assumption puts the torso 10 % too far: forward reach reads that much longer.
        val v = Vec3(0.0, 0.0, -0.40)
        val got = measured(1.5, v, assumedWidth = 0.37 * 1.1)
        assertEquals(-0.40 - 0.15, got.z, 0.02)
    }

    @Test
    fun `operator axes, the robot target, and the reach band`() {
        val forward = BodyMath.toUser(Vec3(0.0, 0.0, -0.62), invertSide = false)
        assertVec(Vec3(0.62, 0.0, 0.0), forward, 1e-12, "toward the camera is forward")
        assertVec(Vec3(0.0, 0.0, 0.3), BodyMath.toUser(Vec3(0.0, -0.3, 0.0), false), 1e-12, "image up is up")
        assertEquals(0.2, BodyMath.toUser(Vec3(0.2, 0.0, 0.0), false).y)
        assertEquals(-0.2, BodyMath.toUser(Vec3(0.2, 0.0, 0.0), true).y, "FACE ME flips the side")

        val s = 0.50 / 0.62
        val t = BodyMath.mirrorTarget(forward, s)
        assertEquals(0.50, t.x, 1e-12)
        assertEquals(0.123, t.z, 1e-12, "the robot's shoulder is the origin")
        assertEquals(0.223, BodyMath.mirrorTarget(Vec3(0.0, 0.0, 0.0), s, lift = 0.1).z, 1e-12)

        val o = BodyMath.ROBOT_SHOULDER
        assertEquals(BodyMath.MIN_REACH_M, (BodyMath.clampReach(o + Vec3(0.02, 0.0, 0.0)) - o).norm(), 1e-12, "not inside the robot")
        assertEquals(BodyMath.MAX_REACH_M, (BodyMath.clampReach(o + Vec3(0.9, 0.0, 0.0)) - o).norm(), 1e-12, "not past its reach")
        assertVec(Vec3(0.3, 0.1, 0.2), BodyMath.clampReach(Vec3(0.3, 0.1, 0.2)), 1e-12, "inside the band: unchanged")
        val ahead = BodyMath.clampReach(o)
        assertEquals(o.x + BodyMath.MIN_REACH_M, ahead.x, 1e-12, "on the origin: straight ahead")

        // Then the driver's box: an arm straight up asks for the space above the base, clamped forward a little.
        val up = Teleop.MIRROR_WORKSPACE.clamp(BodyMath.clampReach(BodyMath.mirrorTarget(Vec3(0.0, 0.0, 0.62), s)))
        assertEquals(0.06, up.x, 1e-12)
        assertEquals(0.60, up.z, 0.03)
        // And the floor: a hand well below the shoulder stops at 8 cm above the table.
        val low = Teleop.MIRROR_WORKSPACE.clamp(BodyMath.clampReach(BodyMath.mirrorTarget(Vec3(0.3, 0.0, -0.4), s)))
        assertEquals(0.08, low.z, 1e-12)
    }

    @Test
    fun `calibration measures the arm and shoulders from an arm held out sideways`() {
        val c = ArmCalibrator()
        val reach = Vec3(-0.66, 0.0, 0.0) // shoulder to palm: 66 cm, straight out to the operator's right
        var result: BodySize? = null
        var t = 0L
        repeat(60) {
            val o = SyntheticPerson.reaching(1.8, reach, t, Quat.about(Vec3.Z, Math.PI / 2), width = 0.40)
            val b = o.body!!
            val s = b[BodyPose.R_SHOULDER]; val e = b[BodyPose.R_ELBOW]; val l = b[BodyPose.L_SHOULDER]
            val m = HandMetrics.of(o.hands[0])
            val why = c.check(s.x.toDouble(), s.y.toDouble(), e.x.toDouble(), e.y.toDouble(), m.palmX.toDouble(), m.palmY.toDouble())
            assertNull(why, "a straight, level arm counts")
            val r = c.update(why, t, (l.x - s.x).toDouble(), kotlin.math.hypot(m.palmX.toDouble() - s.x, m.palmY.toDouble() - s.y), m.depth(f)!!, f)
            if (r != null && result == null) result = r
            t += 50_000_000L
        }
        val r = assertNotNull(result, "done within 3 s")
        assertEquals(0.66, r.armLengthM, 0.02)
        assertEquals(0.40, r.shoulderWidthM, 0.01)
    }

    @Test
    fun `calibration refuses a bent or raised arm`() {
        val c = ArmCalibrator()
        // Bent: the elbow well off the shoulder-hand line.
        assertEquals("straighten your arm", c.check(500.0, 300.0, 400.0, 380.0, 300.0, 300.0))
        // Straight but pointing up at 45 degrees.
        assertEquals("hold your arm level, out to the side", c.check(500.0, 300.0, 400.0, 200.0, 300.0, 100.0))
        // Nothing comes out of bad frames, and a long run of them starts over.
        var t = 0L
        repeat(10) { c.update(null, t, 100.0, 300.0, 1.5, f); t += 50_000_000L }
        assertTrue(c.progress(t) > 0.2)
        repeat(10) { c.update("straighten your arm", t, 0.0, 0.0, 1.0, f); t += 50_000_000L }
        assertEquals(0.0, c.progress(t))
    }

    @Test
    fun `the mirror gate drops a 20 cm jump but follows a 10 cm frame`() {
        val g = OutlierGate(maxStep = Teleop.MIRROR_JUMP_M)
        assertEquals(Verdict.ACCEPT, g.check(Vec3(0.0, 0.0, -0.4), null))
        assertEquals(Verdict.ACCEPT, g.check(Vec3(0.0, -0.1, -0.4), null), "a fast but real move")
        assertEquals(Verdict.DROP, g.check(Vec3(0.0, 0.1, -0.4), null), "20 cm in one frame")
    }
}
