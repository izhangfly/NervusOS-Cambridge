package com.nervus.handteleop.vision

import ai.onnxruntime.OnnxTensor
import ai.onnxruntime.OrtEnvironment
import ai.onnxruntime.OrtSession
import com.nervus.handteleop.math.Vec3
import java.nio.ByteBuffer
import java.nio.ByteOrder
import java.nio.FloatBuffer
import kotlin.math.max

/** A 2-D point in frame pixels with a confidence. */
data class Kp(val x: Float, val y: Float, val score: Float)

/** COCO-17 body keypoints for the one person in front of the camera. */
class BodyPose(val keypoints: List<Kp>, val score: Float) {
    operator fun get(i: Int) = keypoints[i]

    companion object {
        const val L_SHOULDER = 5
        const val R_SHOULDER = 6
        const val L_ELBOW = 7
        const val R_ELBOW = 8
        const val L_WRIST = 9
        const val R_WRIST = 10
        const val L_HIP = 11
        const val R_HIP = 12

        /** Limbs to draw: pairs of keypoint indices. */
        val LIMBS = listOf(
            5 to 7, 7 to 9, 6 to 8, 8 to 10, 5 to 6, 5 to 11, 6 to 12, 11 to 12,
            11 to 13, 13 to 15, 12 to 14, 14 to 16, 0 to 1, 0 to 2, 1 to 3, 2 to 4,
        )
    }
}

/** 21 MediaPipe hand landmarks in frame pixels (0 = wrist, 4 = thumb tip, 8 = index tip...). */
class HandLandmarks(
    val points: List<Kp>,
    val presence: Float,
    /** Which of the operator's arms the crop was seeded from (the body pose knows left from right). */
    val side: Side,
    /**
     * The same 21 landmarks in metres, centred on the hand, in the frame's axes (x right, y down, z away
     * from the camera). Real 3-D lengths: what depth, pinch and wrist roll are measured from.
     */
    val world: List<Vec3>,
    /**
     * The model's own handedness score. MediaPipe defines it for a mirrored (selfie) image and these
     * frames are not mirrored, so [side] is what decides which hand this is; kept for diagnostics.
     */
    val handedness: Float,
) {
    enum class Side { LEFT, RIGHT }

    operator fun get(i: Int) = points[i]

    companion object {
        const val WRIST = 0
        const val THUMB_TIP = 4
        const val INDEX_MCP = 5
        const val INDEX_TIP = 8
        const val MIDDLE_MCP = 9
        const val MIDDLE_TIP = 12
        const val RING_MCP = 13
        const val RING_TIP = 16
        const val PINKY_MCP = 17
        const val PINKY_TIP = 20

        /** The bones of each finger, thumb first, for drawing in per-finger colours. */
        val FINGERS = listOf(
            listOf(0, 1, 2, 3, 4),
            listOf(0, 5, 6, 7, 8),
            listOf(9, 10, 11, 12),
            listOf(13, 14, 15, 16),
            listOf(0, 17, 18, 19, 20),
        )
        val PALM = listOf(5, 9, 13, 17)
    }
}

/**
 * YOLO pose (YOLO26n-pose by default, exported to ONNX at 320x320). Output (1, 56, N): box cx, cy,
 * w, h, person score, then 17 x (x, y, score) in letterboxed input pixels. The operator stands alone
 * in front of the laptop, so the highest-scoring candidate is the person; no NMS needed.
 */
class PoseEstimator(modelBytes: ByteArray, threads: Int) : AutoCloseable {
    private val env = OrtEnvironment.getEnvironment()
    private val session: OrtSession = env.createSession(modelBytes, sessionOptions(threads))
    private val size: Int = (session.inputInfo.values.first().info as ai.onnxruntime.TensorInfo).shape[2].toInt()
    private val inputName = session.inputNames.first()
    private val input: FloatBuffer = direct(3 * size * size)

    fun run(frame: RgbFrame, minScore: Float = 0.35f): BodyPose? {
        val lb = Image.letterboxNchw(frame, size, input)
        OnnxTensor.createTensor(env, input, longArrayOf(1, 3, size.toLong(), size.toLong())).use { t ->
            session.run(mapOf(inputName to t)).use { res ->
                @Suppress("UNCHECKED_CAST")
                val out = (res[0].value as Array<Array<FloatArray>>)[0] // [56][N]
                val n = out[4].size
                var best = -1
                var bestScore = minScore
                for (i in 0 until n) {
                    if (out[4][i] > bestScore) {
                        bestScore = out[4][i]
                        best = i
                    }
                }
                if (best < 0) return null
                val kps = (0 until 17).map { k ->
                    Kp(lb.toFrameX(out[5 + 3 * k][best]), lb.toFrameY(out[6 + 3 * k][best]), out[7 + 3 * k][best])
                }
                return BodyPose(kps, bestScore)
            }
        }
    }

    override fun close() = session.close()
}

/**
 * MediaPipe hand landmarks (the 2023 hand-landmark model, ONNX from OpenCV Zoo). Input: a 224x224
 * RGB crop, NHWC, 0..1, with the hand roughly upright. Outputs: 21 x (x, y, z) in crop pixels, a
 * hand presence score, a handedness score, and 21 x (x, y, z) world landmarks in metres.
 */
class HandEstimator(modelBytes: ByteArray, threads: Int) : AutoCloseable {
    private val env = OrtEnvironment.getEnvironment()
    private val session: OrtSession = env.createSession(modelBytes, sessionOptions(threads))
    private val inputName = session.inputNames.first()
    private val input: FloatBuffer = direct(SIZE * SIZE * 3)

    init {
        // Outputs are read by name: a re-export can reorder them, and world landmarks read from the
        // wrong tensor would still look like plausible numbers.
        val missing = listOf(OUT_LANDMARKS, OUT_PRESENCE, OUT_HANDEDNESS, OUT_WORLD) - session.outputNames
        require(missing.isEmpty()) { "the hand landmark model has no output ${missing.joinToString()}" }
    }

    /** Runs on the crop described by [roi]; returns null when no hand is in it. */
    fun run(frame: RgbFrame, roi: HandRoi, side: HandLandmarks.Side, minPresence: Float = 0.5f): HandLandmarks? {
        val xf = Image.rotatedCropNhwc(frame, roi.cx, roi.cy, roi.side, roi.angle, SIZE, input)
        OnnxTensor.createTensor(env, input, longArrayOf(1, SIZE.toLong(), SIZE.toLong(), 3)).use { t ->
            session.run(mapOf(inputName to t)).use { res ->
                val presence = res.floats(OUT_PRESENCE)[0]
                if (presence < minPresence) return null
                val lm = res.floats(OUT_LANDMARKS)
                val world = res.floats(OUT_WORLD)
                val pts = (0 until 21).map { i ->
                    val (x, y) = xf.toFrame(lm[3 * i], lm[3 * i + 1])
                    Kp(x, y, presence)
                }
                val wpts = (0 until 21).map { i -> xf.worldToFrame(world[3 * i], world[3 * i + 1], world[3 * i + 2]) }
                return HandLandmarks(pts, presence, side, wpts, res.floats(OUT_HANDEDNESS)[0])
            }
        }
    }

    private fun OrtSession.Result.floats(name: String): FloatArray {
        @Suppress("UNCHECKED_CAST")
        return (get(name).orElseThrow().value as Array<FloatArray>)[0]
    }

    override fun close() = session.close()

    companion object {
        const val SIZE = 224

        /** [1, 63]: x, y, z of each landmark in crop pixels. */
        const val OUT_LANDMARKS = "Identity"
        /** [1, 1]: hand presence, 0..1. */
        const val OUT_PRESENCE = "Identity_1"
        /** [1, 1]: handedness, 0..1. */
        const val OUT_HANDEDNESS = "Identity_2"
        /** [1, 63]: world landmarks, metres, centred on the hand, in the crop's axes. */
        const val OUT_WORLD = "Identity_3"
    }
}

/** Where to look for a hand: a square of [side] pixels around (cx, cy), with fingers toward [angle]. */
data class HandRoi(val cx: Float, val cy: Float, val side: Float, val angle: Float)

private fun sessionOptions(threads: Int) = OrtSession.SessionOptions().apply {
    setIntraOpNumThreads(max(1, threads))
    setOptimizationLevel(OrtSession.SessionOptions.OptLevel.ALL_OPT)
}

private fun direct(floats: Int): FloatBuffer =
    ByteBuffer.allocateDirect(floats * 4).order(ByteOrder.nativeOrder()).asFloatBuffer()
