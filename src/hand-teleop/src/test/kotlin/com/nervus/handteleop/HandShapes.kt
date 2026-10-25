package com.nervus.handteleop

import com.nervus.handteleop.math.Vec3

/**
 * Hand shapes as world landmarks, built from [SyntheticHand]'s open right hand (fingers up, -y; palm
 * toward the camera, -z). Only the geometry the gesture rules look at is made realistic: where the
 * fingertips are relative to the wrist and the knuckles.
 */
object HandShapes {
    val open: List<Vec3> = SyntheticHand.world

    /** All four fingers curled into the palm: tips about two thirds of the way from the wrist to the knuckle, in front of the palm. */
    val fist: List<Vec3> = open.toMutableList().apply {
        for ((mcp, joints) in listOf(5 to (6..8), 9 to (10..12), 13 to (14..16), 17 to (18..20))) {
            val k = this[mcp]
            val w = this[0]
            this[joints.first] = k + Vec3(0.0, -0.005, -0.030) // PIP: bent forward, toward the camera
            this[joints.first + 1] = w + (k - w) * 0.85 + Vec3(0.0, 0.0, -0.040)
            this[joints.last] = w + (k - w) * 0.65 + Vec3(0.0, 0.0, -0.025) // tip back toward the wrist
        }
        // Thumb across the curled fingers.
        this[3] = Vec3(0.030, -0.010, -0.040)
        this[4] = Vec3(0.010, -0.030, -0.045)
    }

    /** Thumb and index tips together, the other three fingers out (the pinch that works the gripper). */
    val pinch: List<Vec3> = open.toMutableList().apply {
        this[3] = Vec3(0.055, -0.040, -0.030)
        this[4] = Vec3(0.045, -0.070, -0.040)
        this[7] = Vec3(0.045, -0.085, -0.030)
        this[8] = Vec3(0.047, -0.075, -0.042)
    }

    /** All four fingertips gathered on the thumb tip in front of the palm (the four-finger pinch). */
    val beak: List<Vec3> = open.toMutableList().apply {
        val at = Vec3(0.020, -0.085, -0.055)
        this[4] = at + Vec3(0.010, 0.005, 0.0)
        this[3] = Vec3(0.045, -0.040, -0.035)
        for ((i, dx) in listOf(8 to 0.012, 12 to 0.004, 16 to -0.004, 20 to -0.012)) this[i] = at + Vec3(dx, 0.0, 0.002)
    }
}
