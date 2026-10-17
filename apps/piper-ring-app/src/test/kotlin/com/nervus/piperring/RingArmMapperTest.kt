package com.nervus.piperring

import com.nervus.zring.sdk.SensorDataSample
import kotlin.math.abs
import kotlin.test.Test
import kotlin.test.assertTrue

/**
 * Verifies the 6-DOF mapping with synthetic IMU — no ring/BT hardware needed.
 * accelRange = 8 g → 1 g ≈ raw 4096; gyroRange = 2000 dps.
 */
class RingArmMapperTest {

    private fun sample(t: Long, ax: Int, ay: Int, az: Int, gz: Int = 0) =
        SensorDataSample(
            timestampMilliseconds = t.toUInt(),
            accelerationX = ax.toShort(), accelerationY = ay.toShort(), accelerationZ = az.toShort(),
            gyroscopeX = 0, gyroscopeY = 0, gyroscopeZ = gz.toShort(),
        )

    @Test
    fun flatRing_identityOrientation_noTranslation() {
        val m = RingArmMapper(8.0, 2000.0)
        var t = m.update(sample(0, 0, 0, 4096))
        t = m.update(sample(20, 0, 0, 4096))
        println("flat -> quat=${t.quat.joinToString()} tilt=${t.tiltDeg} pos=${t.pos.joinToString()}")
        assertTrue(abs(t.quat[0] - 1.0) < 0.02, "w≈1 (identity)")
        assertTrue(t.tiltDeg < 2.0, "no tilt")
        assertTrue(abs(t.pos[0] - 0.20) < 0.01 && abs(t.pos[2] - 0.30) < 0.01, "no drift at rest")
    }

    @Test
    fun tiltedRing_drivesGimbal() {
        val m = RingArmMapper(8.0, 2000.0)
        // ~30° roll: gravity splits into +Y (sin30·g) and +Z (cos30·g)
        val t = m.update(sample(0, 0, 2048, 3547))
        println("tilt30 -> quat=${t.quat.joinToString()} tilt=${t.tiltDeg}")
        assertTrue(t.tiltDeg in 25.0..35.0, "tilt ≈ 30°")
        assertTrue(abs(t.quat[1]) > 0.1, "roll shows up in the orientation quaternion (gimbal)")
    }

    @Test
    fun handTranslation_movesGripperXYZ() {
        val m = RingArmMapper(8.0, 2000.0)
        m.update(sample(0, 0, 0, 4096))
        var t = m.update(sample(20, 0, 0, 4096))
        val x0 = t.pos[0]
        // sustained forward linear accel (~0.5 g on X) for ~12 ticks
        for (i in 1..12) t = m.update(sample(20L + i * 20, 2048, 0, 4096))
        println("translate -> x0=$x0 x=${t.pos[0]} pos=${t.pos.joinToString()}")
        assertTrue(t.pos[0] > x0 + 0.01, "hand +X translation moves the gripper +X")
    }
}
