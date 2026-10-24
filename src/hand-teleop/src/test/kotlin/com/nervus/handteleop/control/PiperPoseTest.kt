package com.nervus.handteleop.control

import com.nervus.handteleop.math.Quat
import com.nervus.handteleop.math.Vec3
import kotlin.math.abs
import kotlin.random.Random
import kotlin.test.Test
import kotlin.test.assertEquals
import kotlin.test.assertNotNull
import kotlin.test.assertNull

class PiperPoseTest {
    private fun r(d: Double) = Math.toRadians(d)

    /** The same rotation, allowing for q and -q. */
    private fun assertSameRotation(a: Quat, b: Quat, tol: Double = 1e-9) {
        val dot = abs(a.x * b.x + a.y * b.y + a.z * b.z + a.w * b.w)
        assertEquals(1.0, dot, tol, "$a vs $b")
    }

    @Test
    fun `angles are static XYZ, R = Rz Ry Rx`() {
        val x = Vec3(1.0, 0.0, 0.0)
        val y = Vec3(0.0, 1.0, 0.0)
        for ((rx, ry, rz) in listOf(Triple(10.0, 20.0, 30.0), Triple(-170.0, 45.0, 90.0), Triple(179.0, -60.0, -120.0))) {
            val expected = Quat.about(Vec3.Z, r(rz)) * Quat.about(y, r(ry)) * Quat.about(x, r(rx))
            assertSameRotation(expected, PiperPose.eulerToQuat(r(rx), r(ry), r(rz)))
        }
    }

    @Test
    fun `Euler to quaternion and back round-trips`() {
        val rnd = Random(7)
        repeat(500) {
            val rx = r(rnd.nextDouble(-179.0, 179.0))
            val ry = r(rnd.nextDouble(-85.0, 85.0)) // away from gimbal lock, where the angles aren't unique
            val rz = r(rnd.nextDouble(-179.0, 179.0))
            val e = PiperPose.quatToEuler(PiperPose.eulerToQuat(rx, ry, rz))
            assertEquals(rx, e[0], 1e-9)
            assertEquals(ry, e[1], 1e-9)
            assertEquals(rz, e[2], 1e-9)
        }
    }

    @Test
    fun `at gimbal lock the rotation still round-trips`() {
        val q = PiperPose.eulerToQuat(r(30.0), r(90.0), r(20.0))
        val e = PiperPose.quatToEuler(q)
        assertEquals(0.0, e[2], 1e-12, "yaw folds into roll")
        assertSameRotation(q, PiperPose.eulerToQuat(e[0], e[1], e[2]), 1e-6)
    }

    @Test
    fun `driver pose to Manipulator pose and back`() {
        val driver = listOf(250.0, -40.0, 310.0, 179.5, 12.0, -88.0)
        val pose = assertNotNull(PiperPose.toPose(driver))
        assertEquals("base_link", pose.frame)
        assertEquals(0.25, pose.x, 1e-12)
        assertEquals(-0.04, pose.y, 1e-12)
        assertEquals(0.31, pose.z, 1e-12)
        val back = assertNotNull(PiperPose.fromPose(pose))
        for (i in 0..5) assertEquals(driver[i], back[i], 1e-7, "element $i")
    }

    @Test
    fun `rolling the tool about its own z changes the driver angles the way piperd would read them`() {
        // The control loop's q0 * Rz(phi) arrives at the driver as Euler angles that turn back into the same rotation.
        val q0 = PiperPose.eulerToQuat(r(180.0), r(30.0), r(0.0))
        val q = q0 * Quat.about(Vec3.Z, r(25.0))
        val pose = PiperPose.toPose(listOf(300.0, 0.0, 300.0, 0.0, 0.0, 0.0))!!.toBuilder()
            .setQx(q.x).setQy(q.y).setQz(q.z).setQw(q.w).build()
        val d = assertNotNull(PiperPose.fromPose(pose))
        assertSameRotation(q, PiperPose.eulerToQuat(r(d[3]), r(d[4]), r(d[5])))
    }

    @Test
    fun `bad poses are refused`() {
        val good = PiperPose.toPose(listOf(250.0, 0.0, 300.0, 0.0, 0.0, 0.0))!!
        assertNull(PiperPose.fromPose(good.toBuilder().setFrame("tool").build()))
        assertNull(PiperPose.fromPose(good.toBuilder().setQx(0.0).setQy(0.0).setQz(0.0).setQw(0.0).build()))
        assertNull(PiperPose.fromPose(good.toBuilder().setX(Double.NaN).build()))
        assertNull(PiperPose.toPose(listOf(1.0, 2.0)))
    }
}
