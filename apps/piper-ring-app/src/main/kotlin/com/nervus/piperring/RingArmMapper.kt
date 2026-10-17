package com.nervus.piperring

import com.nervus.zring.sdk.SensorDataSample
import kotlin.math.atan2
import kotlin.math.cos
import kotlin.math.hypot
import kotlin.math.sin
import kotlin.math.sqrt

/**
 * Maps the ring's 6-axis IMU to an absolute end-effector target for Manipulator@1.
 *
 * - Dip the hand (tilt) → the gripper's orientation follows (gimbal).
 * - Move the hand while roughly flat → the gripper translates (leaky-integrated, clamped).
 *
 * Pure calculation (no I/O) so it can be unit-tested. Units match Manipulator@1: position in
 * METERS, quaternion [w,x,y,z]. Tune the gains on real hardware.
 */
class RingArmMapper(
    private val accelRangeG: Double,
    private val gyroRangeDps: Double,
) {
    // Seed pose (a safe, reachable TCP pose for the Piper) + workspace clamp (meters).
    private val pos = doubleArrayOf(0.20, 0.0, 0.30)
    private val vel = doubleArrayOf(0.0, 0.0, 0.0)
    private var yaw = 0.0
    private var lastTsMs: Long = -1

    private val aScale = accelRangeG * G / 32768.0     // raw Short → m/s^2
    private val gScale = gyroRangeDps * DEG2RAD / 32768.0  // raw Short → rad/s

    data class Target(val pos: DoubleArray, val quat: DoubleArray, val tiltDeg: Double)

    /**
     * 6-DOF mapping, both halves ALWAYS live (matches the webctl's two control sets):
     *  - ring **rotation** → gripper **orientation** (gimbal): roll/pitch from the gravity
     *    vector (accel), yaw integrated from the Z gyro → an absolute quaternion.
     *  - ring **translation** → gripper **XYZ**: linear accel (gravity removed) leaky-integrated
     *    into a workspace-clamped position target.
     */
    fun update(s: SensorDataSample): Target {
        val ax = s.accelerationX * aScale; val ay = s.accelerationY * aScale; val az = s.accelerationZ * aScale
        val gz = s.gyroscopeZ * gScale
        val ts = s.timestampMilliseconds.toLong()
        val dt = if (lastTsMs < 0) 0.02 else ((ts - lastTsMs).coerceAtLeast(1)) / 1000.0
        lastTsMs = ts

        // --- rotation → gimbal (always) ---
        val roll = atan2(ay, az)
        val pitch = atan2(-ax, sqrt(ay * ay + az * az) + 1e-9)
        yaw += gz * dt
        val tiltDeg = Math.toDegrees(hypot(roll, pitch))

        // --- translation → XYZ (always) ---
        val lin = doubleArrayOf(ax, ay, az - G)   // gravity-removed linear accel
        for (i in 0..2) {
            vel[i] = vel[i] * VEL_DAMP + lin[i] * dt
            pos[i] += vel[i] * TRANS_GAIN
        }
        clamp(pos, WORKSPACE)

        return Target(pos.copyOf(), eulerToQuat(roll, pitch, yaw), tiltDeg)
    }

    companion object {
        private const val G = 9.80665
        private const val DEG2RAD = Math.PI / 180.0
        private const val TILT_GIMBAL_DEG = 25.0
        private const val VEL_DAMP = 0.86
        private const val TRANS_GAIN = 0.9            // m of target motion per (m/s) of hand velocity
        private val WORKSPACE = arrayOf(0.05 to 0.55, -0.35 to 0.35, 0.05 to 0.55)

        private fun clamp(p: DoubleArray, box: Array<Pair<Double, Double>>) {
            for (i in 0..2) p[i] = p[i].coerceIn(box[i].first, box[i].second)
        }

        /** roll=X, pitch=Y, yaw=Z (ZYX) → [w,x,y,z]. */
        fun eulerToQuat(roll: Double, pitch: Double, yaw: Double): DoubleArray {
            val cr = cos(roll / 2); val sr = sin(roll / 2)
            val cp = cos(pitch / 2); val sp = sin(pitch / 2)
            val cy = cos(yaw / 2); val sy = sin(yaw / 2)
            return doubleArrayOf(
                cr * cp * cy + sr * sp * sy,
                sr * cp * cy - cr * sp * sy,
                cr * sp * cy + sr * cp * sy,
                cr * cp * sy - sr * sp * cy,
            )
        }
    }
}
