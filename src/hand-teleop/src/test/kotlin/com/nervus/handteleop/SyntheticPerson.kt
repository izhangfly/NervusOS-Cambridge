package com.nervus.handteleop

import com.nervus.handteleop.math.Quat
import com.nervus.handteleop.math.Vec3
import com.nervus.handteleop.vision.BodyPose
import com.nervus.handteleop.vision.HandLandmarks
import com.nervus.handteleop.vision.Kp
import com.nervus.handteleop.vision.Observation
import com.nervus.handteleop.vision.RgbFrame

/**
 * An operator in front of a 1280x720 camera (f = 0.7 x width), as the pose and hand models would report
 * them: points in camera metres (x right, y down, z away), projected through a pinhole. The person faces
 * the camera, so their right shoulder is on the camera's left (-x).
 */
object SyntheticPerson {
    const val W = 1280
    const val H = 720
    const val F = 0.7 * W

    fun px(p: Vec3) = Kp((F * p.x / p.z + W / 2.0).toFloat(), (F * p.y / p.z + H / 2.0).toFloat(), 0.95f)

    /** Right and left shoulder of a person whose shoulders are [depth] m from the camera, [width] apart, [height] above the image centre line. */
    fun shoulders(depth: Double, width: Double = 0.37, height: Double = 0.10) =
        Vec3(-width / 2, -height, depth) to Vec3(width / 2, -height, depth)

    /** COCO-17 keypoints with the shoulders, the right elbow and the right wrist; the rest unseen. */
    fun body(rShoulder: Vec3, lShoulder: Vec3, rElbow: Vec3, rWrist: Vec3): BodyPose {
        val kps = MutableList(17) { Kp(0f, 0f, 0f) }
        kps[BodyPose.R_SHOULDER] = px(rShoulder)
        kps[BodyPose.L_SHOULDER] = px(lShoulder)
        kps[BodyPose.R_ELBOW] = px(rElbow)
        kps[BodyPose.R_WRIST] = px(rWrist)
        return BodyPose(kps, 0.9f)
    }

    private val palmCentre: Vec3 = intArrayOf(0, 5, 9, 13, 17).map { SyntheticHand.world[it] }
        .reduce { a, b -> a + b } * (1.0 / 5)

    /** A right hand whose palm centre is at [at] (camera metres), turned by [q], in the shape [shape]. */
    fun hand(at: Vec3, q: Quat = Quat.IDENTITY, shape: List<Vec3> = HandShapes.open): HandLandmarks {
        val world = shape.map { q.rotate(it - palmCentre) }
        return HandLandmarks(world.map { px(it + at) }, 1f, HandLandmarks.Side.RIGHT, world, 0.5f)
    }

    /** Shoulders at [depth], and the right palm at [reach] from the right shoulder (camera axes), the elbow halfway. */
    fun reaching(depth: Double, reach: Vec3, t: Long, q: Quat = Quat.IDENTITY, shape: List<Vec3> = HandShapes.open, width: Double = 0.37): Observation {
        val (r, l) = shoulders(depth, width)
        val palm = r + reach
        val elbow = r + reach * 0.5 + Vec3(0.0, 0.03, 0.0)
        return Observation(RgbFrame(W, H, IntArray(0), t, t), body(r, l, elbow, palm), listOf(hand(palm, q, shape)), 0f, 0f, atNanos = t)
    }
}
