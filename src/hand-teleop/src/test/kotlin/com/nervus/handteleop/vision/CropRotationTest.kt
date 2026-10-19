package com.nervus.handteleop.vision

import com.nervus.handteleop.math.HandMath
import com.nervus.handteleop.math.Vec3
import java.nio.ByteBuffer
import java.nio.ByteOrder
import kotlin.math.cos
import kotlin.math.sin
import kotlin.test.Test
import kotlin.test.assertEquals
import kotlin.test.assertTrue

/**
 * World landmarks come out of the model in the crop's axes. These check that turning them into the
 * frame's axes uses the same rotation, with the same sign, as the crop itself.
 */
class CropRotationTest {
    private val angles = listOf(-2.6, -Math.PI / 2, -1.0, 0.0, 0.4, Math.PI / 2, 2.5)

    @Test
    fun `a dot found in a real crop points the same way as its rotated world offset`() {
        // A black frame with one small white square; crop around a point near it at several angles,
        // find the square in the crop, and check that its crop-axes offset, rotated as world landmarks
        // are, lands on the square's offset in the frame.
        val w = 640
        val h = 480
        val argb = IntArray(w * h) { 0xFF000000.toInt() }
        val dotX = 340
        val dotY = 195
        for (y in dotY - 2..dotY + 2) for (x in dotX - 2..dotX + 2) argb[y * w + x] = 0xFFFFFFFF.toInt()
        val frame = RgbFrame(w, h, argb, 0, 0)
        val cx = 300.5f
        val cy = 220.5f
        val size = 224
        val buf = ByteBuffer.allocateDirect(size * size * 3 * 4).order(ByteOrder.nativeOrder()).asFloatBuffer()

        for (angle in angles) {
            val side = 224f // one crop pixel per frame pixel
            val xf = Image.rotatedCropNhwc(frame, cx, cy, side, angle.toFloat(), size, buf)
            // Brightness-weighted centroid of the dot in crop coordinates (crop pixel i samples at i + 0.5;
            // frame pixel i sits at i).
            var sx = 0.0
            var sy = 0.0
            var n = 0.0
            for (y in 0 until size) for (x in 0 until size) {
                val v = buf.get((y * size + x) * 3).toDouble()
                sx += (x + 0.5) * v; sy += (y + 0.5) * v; n += v
            }
            assertTrue(n > 0, "the dot is inside the crop at angle $angle")
            val half = size / 2.0
            val world = xf.worldToFrame((sx / n - half).toFloat(), (sy / n - half).toFloat(), 0.07f)
            assertEquals(dotX - cx.toDouble(), world.x, 0.3, "x at angle $angle")
            assertEquals(dotY - cy.toDouble(), world.y, 0.3, "y at angle $angle")
            assertEquals(0.07, world.z, 1e-6, "depth is left alone")
        }
    }

    @Test
    fun `world offsets turn exactly as crop pixel offsets do`() {
        val buf = ByteBuffer.allocateDirect(8 * 8 * 3 * 4).order(ByteOrder.nativeOrder()).asFloatBuffer()
        val frame = RgbFrame(8, 8, IntArray(64), 0, 0)
        for (angle in angles) {
            val xf = Image.rotatedCropNhwc(frame, 4f, 4f, 16f, angle.toFloat(), 8, buf) // 2 frame pixels per crop pixel
            val (ox, oy) = xf.toFrame(4f, 4f)
            val (px, py) = xf.toFrame(4f + 1.5f, 4f - 2.5f)
            val w = xf.worldToFrame(1.5f, -2.5f, 0f)
            assertEquals((px - ox) / 2.0, w.x, 1e-5)
            assertEquals((py - oy) / 2.0, w.y, 1e-5)
        }
    }

    @Test
    fun `fingers up in the crop point along the crop angle in the frame`() {
        // The crop is cut so that `angle` (the forearm, then the hand) points up (-y) in it.
        for (angle in angles) {
            val t = angle + Math.PI / 2
            val d = HandMath.cropToFrame(Vec3(0.0, -1.0, 0.0), cos(t), sin(t))
            assertEquals(cos(angle), d.x, 1e-12)
            assertEquals(sin(angle), d.y, 1e-12)
        }
    }
}
