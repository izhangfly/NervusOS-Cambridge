package com.nervus.handteleop.vision

import com.nervus.handteleop.camera.V4l2
import com.nervus.handteleop.math.BodyMath
import java.io.BufferedInputStream
import java.io.DataInputStream
import java.io.EOFException
import java.io.File
import java.util.Locale
import java.util.concurrent.TimeUnit
import kotlin.test.Test
import kotlin.test.assertTrue

/**
 * The real models on a recorded video, no arm: YOLO pose and hand landmarks frame by frame, and the
 * mirror mode's shoulder-to-hand vector from them, written to build/replay.csv with a summary. Skipped
 * unless HAND_TELEOP_REPLAY names a video file (record one with
 * `ffmpeg -f v4l2 -i /dev/video60 -t 30 me.mp4`, or use any front-facing clip of a person).
 */
class ReplayTest {
    private fun resource(path: String) = javaClass.classLoader.getResourceAsStream(path)!!.use { it.readBytes() }

    @Test
    fun `a recorded video gives plausible shoulder-to-hand vectors`() {
        val path = System.getenv("HAND_TELEOP_REPLAY")?.takeIf { it.isNotBlank() } ?: return
        val probe = ProcessBuilder("ffprobe", "-v", "error", "-select_streams", "v:0", "-show_entries", "stream=width,height", "-of", "csv=p=0", path)
            .redirectErrorStream(true).start()
        val (w0, h0) = probe.inputStream.bufferedReader().readText().trim().split(",").map { it.trim().toInt() }
        probe.waitFor(10, TimeUnit.SECONDS)
        val w = 1280
        val h = (h0 * w / w0.toDouble() / 2).toInt() * 2
        val maxFrames = System.getenv("HAND_TELEOP_REPLAY_FRAMES")?.toIntOrNull() ?: 300
        val ff = ProcessBuilder("ffmpeg", "-hide_banner", "-loglevel", "error", "-i", path, "-vf", "fps=10,scale=$w:$h",
            "-frames:v", "$maxFrames", "-f", "rawvideo", "-pix_fmt", "rgb24", "-").start()
        val input = DataInputStream(BufferedInputStream(ff.inputStream, w * h * 3))
        val tracker = Tracker(PoseEstimator(resource("models/yolo26n-pose_320.onnx"), 2), HandEstimator(resource("models/hand_landmark.onnx"), 2))
        val f = 0.7 * w
        val buf = ByteArray(w * h * 3)
        val rows = ArrayList<String>()
        var frames = 0
        var withBody = 0
        val torso = ArrayList<Double>()
        val reach = ArrayList<Double>()
        try {
            while (true) {
                input.readFully(buf)
                val argb = IntArray(w * h)
                V4l2.rgb24ToArgb(buf, argb)
                val obs = tracker.process(RgbFrame(w, h, argb, frames * 100_000_000L, frames.toLong()))
                frames++
                val b = obs.body ?: continue
                withBody++
                for (hand in obs.hands) {
                    val (s, other) = if (hand.side == HandLandmarks.Side.LEFT) BodyPose.L_SHOULDER to BodyPose.R_SHOULDER else BodyPose.R_SHOULDER to BodyPose.L_SHOULDER
                    if (b[s].score < 0.3f || b[other].score < 0.3f) continue
                    val wPx = kotlin.math.hypot((b[s].x - b[other].x).toDouble(), (b[s].y - b[other].y).toDouble())
                    val zt = BodyMath.torsoDepth(f, 0.37, wPx) ?: continue
                    val m = HandMetrics.of(hand)
                    val zh = m.depth(f) ?: continue
                    val v = BodyMath.toUser(BodyMath.shoulderToHand(b[s].x.toDouble(), b[s].y.toDouble(), zt, m.palmX.toDouble(), m.palmY.toDouble(), zh, f, w / 2.0, h / 2.0), false)
                    torso += zt
                    reach += v.norm()
                    rows += String.format(Locale.ROOT, "%.1f,%s,%.0f,%.3f,%.3f,%.3f,%.3f,%.3f", frames / 10.0, hand.side, wPx, zt, zh, v.x, v.y, v.z)
                }
            }
        } catch (e: EOFException) {
            // end of the video
        } finally {
            ff.destroy()
        }
        File("build").mkdirs()
        File("build/replay.csv").writeText("t_s,side,shoulder_px,torso_z,hand_z,fwd,side_m,up\n" + rows.joinToString("\n") + "\n")
        fun med(v: List<Double>) = v.sorted().let { if (it.isEmpty()) Double.NaN else it[it.size / 2] }
        println("replay: $frames frames, body in $withBody, ${rows.size} hand vectors; torso median %.2f m, reach median %.2f m (build/replay.csv)".format(med(torso), med(reach)))
        assertTrue(frames > 0, "no frames decoded from $path")
        assertTrue(rows.size >= frames / 5, "shoulders and a hand in too few frames: ${rows.size} of $frames")
        assertTrue(med(torso) in 0.5..5.0, "torso median ${med(torso)} m")
        assertTrue(med(reach) in 0.05..1.0, "reach median ${med(reach)} m")
    }
}
