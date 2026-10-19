package com.nervus.handteleop

import com.nervus.handteleop.math.Quat
import com.nervus.handteleop.math.Vec3

/**
 * A right hand shaped like MediaPipe's world landmarks: metres, about centred on the palm, fingers up
 * (-y), palm facing the camera with the thumb on the image's right (+x). The palm points (wrist and
 * knuckles) are flat at z = 0; the fingers curl a little toward the camera.
 */
object SyntheticHand {
    val world: List<Vec3> = listOf(
        Vec3(0.000, 0.045, 0.000), // 0 wrist
        Vec3(0.025, 0.035, -0.005), Vec3(0.045, 0.015, -0.010), Vec3(0.060, -0.005, -0.015), Vec3(0.070, -0.020, -0.020), // thumb
        Vec3(0.030, -0.045, 0.000), Vec3(0.034, -0.080, -0.002), Vec3(0.036, -0.100, -0.004), Vec3(0.038, -0.120, -0.006), // index
        Vec3(0.008, -0.050, 0.000), Vec3(0.009, -0.090, -0.002), Vec3(0.010, -0.115, -0.004), Vec3(0.010, -0.135, -0.006), // middle
        Vec3(-0.014, -0.045, 0.000), Vec3(-0.016, -0.082, -0.002), Vec3(-0.017, -0.105, -0.004), Vec3(-0.018, -0.125, -0.006), // ring
        Vec3(-0.034, -0.035, 0.000), Vec3(-0.038, -0.062, -0.002), Vec3(-0.041, -0.080, -0.004), Vec3(-0.044, -0.095, -0.006), // little
    )

    /** The hand turned by [q] (about its own centre): what the world landmarks would read. */
    fun turned(q: Quat): List<Vec3> = world.map { q.rotate(it) }

    /**
     * The hand turned by [q] and placed with its centre at [at] (camera metres), seen through a pinhole
     * with focal length [f] and centre (cx, cy): frame pixels, z = 0.
     */
    fun project(q: Quat, at: Vec3, f: Double, cx: Double, cy: Double): List<Vec3> =
        turned(q).map {
            val c = it + at
            Vec3(f * c.x / c.z + cx, f * c.y / c.z + cy, 0.0)
        }
}
