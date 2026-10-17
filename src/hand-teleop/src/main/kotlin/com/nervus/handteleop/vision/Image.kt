package com.nervus.handteleop.vision

import java.io.ByteArrayInputStream
import java.nio.FloatBuffer
import javax.imageio.ImageIO
import kotlin.math.cos
import kotlin.math.floor
import kotlin.math.sin

/**
 * One camera frame as packed 0xAARRGGBB pixels, unmirrored (as the camera sees it).
 * The UI mirrors it for display; the vision code works on what the camera saw.
 */
class RgbFrame(val width: Int, val height: Int, val argb: IntArray, val timestampNanos: Long, val sequence: Long)

object Image {

    /** YUYV 4:2:2 (two pixels per four bytes: Y0 U Y1 V) to ARGB, BT.601 limited range. */
    fun yuyvToArgb(src: ByteArray, width: Int, height: Int, out: IntArray) {
        var si = 0
        var di = 0
        val pairs = width * height / 2
        for (p in 0 until pairs) {
            val y0 = (src[si].toInt() and 0xFF) - 16
            val u = (src[si + 1].toInt() and 0xFF) - 128
            val y1 = (src[si + 2].toInt() and 0xFF) - 16
            val v = (src[si + 3].toInt() and 0xFF) - 128
            si += 4
            val rv = 409 * v
            val guv = -100 * u - 208 * v
            val bu = 516 * u
            val c0 = 298 * y0
            val c1 = 298 * y1
            out[di++] = pack((c0 + rv + 128) shr 8, (c0 + guv + 128) shr 8, (c0 + bu + 128) shr 8)
            out[di++] = pack((c1 + rv + 128) shr 8, (c1 + guv + 128) shr 8, (c1 + bu + 128) shr 8)
        }
    }

    private fun pack(r: Int, g: Int, b: Int): Int =
        (0xFF shl 24) or (clamp(r) shl 16) or (clamp(g) shl 8) or clamp(b)

    private fun clamp(v: Int) = if (v < 0) 0 else if (v > 255) 255 else v

    /**
     * Decodes one MJPEG frame. UVC cameras usually leave out the Huffman tables (they use the
     * standard ones from the JPEG spec), and Java's decoder refuses a JPEG without them, so the
     * standard tables are inserted when the frame has none.
     */
    fun decodeMjpeg(jpeg: ByteArray, out: IntArray, width: Int, height: Int): Boolean {
        val bytes = if (hasHuffmanTables(jpeg)) jpeg else insertStandardHuffman(jpeg)
        val img = runCatching { ImageIO.read(ByteArrayInputStream(bytes)) }.getOrNull() ?: return false
        if (img.width != width || img.height != height) return false
        img.getRGB(0, 0, width, height, out, 0, width)
        return true
    }

    private fun hasHuffmanTables(j: ByteArray): Boolean {
        var i = 2
        while (i + 3 < j.size) {
            if (j[i] != 0xFF.toByte()) return false
            val marker = j[i + 1].toInt() and 0xFF
            if (marker == 0xC4) return true
            if (marker == 0xDA) return false // start of scan: no tables before it
            val len = ((j[i + 2].toInt() and 0xFF) shl 8) or (j[i + 3].toInt() and 0xFF)
            i += 2 + len
        }
        return false
    }

    private fun insertStandardHuffman(j: ByteArray): ByteArray {
        // Insert right after SOI. A DHT anywhere before SOS is valid.
        val out = ByteArray(j.size + STANDARD_DHT.size)
        System.arraycopy(j, 0, out, 0, 2)
        System.arraycopy(STANDARD_DHT, 0, out, 2, STANDARD_DHT.size)
        System.arraycopy(j, 2, out, 2 + STANDARD_DHT.size, j.size - 2)
        return out
    }

    /**
     * Letterboxes [src] into a size x size RGB float tensor (NCHW, 0..1) the way YOLO was trained:
     * scale to fit, pad with grey 114. Returns the scale and padding to map results back.
     */
    fun letterboxNchw(src: RgbFrame, size: Int, out: FloatBuffer): Letterbox {
        val r = minOf(size.toFloat() / src.width, size.toFloat() / src.height)
        val nw = Math.round(src.width * r)
        val nh = Math.round(src.height * r)
        val px = (size - nw) / 2
        val py = (size - nh) / 2
        val plane = size * size
        val pad = 114f / 255f
        out.clear()
        val arr = FloatArray(3 * plane) { pad }
        val inv = 1f / r
        for (y in 0 until nh) {
            val sy = (y + 0.5f) * inv - 0.5f
            for (x in 0 until nw) {
                val sx = (x + 0.5f) * inv - 0.5f
                val c = sampleBilinear(src, sx, sy)
                val o = (y + py) * size + (x + px)
                arr[o] = ((c shr 16) and 0xFF) / 255f
                arr[plane + o] = ((c shr 8) and 0xFF) / 255f
                arr[2 * plane + o] = (c and 0xFF) / 255f
            }
        }
        out.put(arr)
        out.rewind()
        return Letterbox(r, px.toFloat(), py.toFloat())
    }

    /**
     * Samples a size x size crop centred on (cx, cy), side [side] pixels, rotated so that the
     * direction [angle] (radians, image coordinates) points straight up in the crop. Writes NHWC RGB
     * floats 0..1. Returns the transform from crop pixels back to frame pixels.
     */
    fun rotatedCropNhwc(src: RgbFrame, cx: Float, cy: Float, side: Float, angle: Float, size: Int, out: FloatBuffer): CropTransform {
        // Rotation that maps "up" (-y) in the crop onto `angle` in the frame.
        val theta = angle + (Math.PI / 2).toFloat()
        val c = cos(theta)
        val s = sin(theta)
        val k = side / size
        val arr = FloatArray(size * size * 3)
        var o = 0
        val half = size / 2f
        for (y in 0 until size) {
            val dy = (y + 0.5f - half) * k
            for (x in 0 until size) {
                val dx = (x + 0.5f - half) * k
                val fx = cx + dx * c - dy * s
                val fy = cy + dx * s + dy * c
                val col = if (fx < 0 || fy < 0 || fx > src.width - 1 || fy > src.height - 1) 0 else sampleBilinear(src, fx, fy)
                arr[o++] = ((col shr 16) and 0xFF) / 255f
                arr[o++] = ((col shr 8) and 0xFF) / 255f
                arr[o++] = (col and 0xFF) / 255f
            }
        }
        out.clear()
        out.put(arr)
        out.rewind()
        return CropTransform(cx, cy, k, c, s, half)
    }

    private fun sampleBilinear(src: RgbFrame, x: Float, y: Float): Int {
        val xc = x.coerceIn(0f, (src.width - 1).toFloat())
        val yc = y.coerceIn(0f, (src.height - 1).toFloat())
        val x0 = floor(xc).toInt()
        val y0 = floor(yc).toInt()
        val x1 = minOf(x0 + 1, src.width - 1)
        val y1 = minOf(y0 + 1, src.height - 1)
        val fx = xc - x0
        val fy = yc - y0
        val a = src.argb[y0 * src.width + x0]
        val b = src.argb[y0 * src.width + x1]
        val c = src.argb[y1 * src.width + x0]
        val d = src.argb[y1 * src.width + x1]
        fun ch(shift: Int): Int {
            val top = ((a shr shift) and 0xFF) * (1 - fx) + ((b shr shift) and 0xFF) * fx
            val bot = ((c shr shift) and 0xFF) * (1 - fx) + ((d shr shift) and 0xFF) * fx
            return (top * (1 - fy) + bot * fy).toInt()
        }
        return (0xFF shl 24) or (ch(16) shl 16) or (ch(8) shl 8) or ch(0)
    }

    // The standard Huffman tables from the JPEG spec (Annex K.3), the ones MJPEG cameras assume.
    // Built from the spec's BITS/HUFFVAL arrays rather than a hex blob so they can be checked.
    private val STANDARD_DHT: ByteArray by lazy {
        val tables = listOf(
            // class/id, BITS (code counts for lengths 1..16), HUFFVAL
            0x00 to (intArrayOf(0, 1, 5, 1, 1, 1, 1, 1, 1, 0, 0, 0, 0, 0, 0, 0) to IntArray(12) { it }),
            0x01 to (intArrayOf(0, 3, 1, 1, 1, 1, 1, 1, 1, 1, 1, 0, 0, 0, 0, 0) to IntArray(12) { it }),
            0x10 to (intArrayOf(0, 2, 1, 3, 3, 2, 4, 3, 5, 5, 4, 4, 0, 0, 1, 0x7d) to AC_LUMA),
            0x11 to (intArrayOf(0, 2, 1, 2, 4, 4, 3, 4, 7, 5, 4, 4, 0, 1, 2, 0x77) to AC_CHROMA),
        )
        val body = java.io.ByteArrayOutputStream()
        for ((tc, t) in tables) {
            val (bits, vals) = t
            check(bits.sum() == vals.size) { "Huffman table ${tc.toString(16)} is inconsistent" }
            body.write(tc)
            bits.forEach { body.write(it) }
            vals.forEach { body.write(it) }
        }
        val b = body.toByteArray()
        val len = b.size + 2
        byteArrayOf(0xFF.toByte(), 0xC4.toByte(), (len shr 8).toByte(), len.toByte()) + b
    }

    private val AC_LUMA = intArrayOf(
        0x01, 0x02, 0x03, 0x00, 0x04, 0x11, 0x05, 0x12, 0x21, 0x31, 0x41, 0x06, 0x13, 0x51, 0x61, 0x07,
        0x22, 0x71, 0x14, 0x32, 0x81, 0x91, 0xa1, 0x08, 0x23, 0x42, 0xb1, 0xc1, 0x15, 0x52, 0xd1, 0xf0,
        0x24, 0x33, 0x62, 0x72, 0x82, 0x09, 0x0a, 0x16, 0x17, 0x18, 0x19, 0x1a, 0x25, 0x26, 0x27, 0x28,
        0x29, 0x2a, 0x34, 0x35, 0x36, 0x37, 0x38, 0x39, 0x3a, 0x43, 0x44, 0x45, 0x46, 0x47, 0x48, 0x49,
        0x4a, 0x53, 0x54, 0x55, 0x56, 0x57, 0x58, 0x59, 0x5a, 0x63, 0x64, 0x65, 0x66, 0x67, 0x68, 0x69,
        0x6a, 0x73, 0x74, 0x75, 0x76, 0x77, 0x78, 0x79, 0x7a, 0x83, 0x84, 0x85, 0x86, 0x87, 0x88, 0x89,
        0x8a, 0x92, 0x93, 0x94, 0x95, 0x96, 0x97, 0x98, 0x99, 0x9a, 0xa2, 0xa3, 0xa4, 0xa5, 0xa6, 0xa7,
        0xa8, 0xa9, 0xaa, 0xb2, 0xb3, 0xb4, 0xb5, 0xb6, 0xb7, 0xb8, 0xb9, 0xba, 0xc2, 0xc3, 0xc4, 0xc5,
        0xc6, 0xc7, 0xc8, 0xc9, 0xca, 0xd2, 0xd3, 0xd4, 0xd5, 0xd6, 0xd7, 0xd8, 0xd9, 0xda, 0xe1, 0xe2,
        0xe3, 0xe4, 0xe5, 0xe6, 0xe7, 0xe8, 0xe9, 0xea, 0xf1, 0xf2, 0xf3, 0xf4, 0xf5, 0xf6, 0xf7, 0xf8,
        0xf9, 0xfa,
    )

    private val AC_CHROMA = intArrayOf(
        0x00, 0x01, 0x02, 0x03, 0x11, 0x04, 0x05, 0x21, 0x31, 0x06, 0x12, 0x41, 0x51, 0x07, 0x61, 0x71,
        0x13, 0x22, 0x32, 0x81, 0x08, 0x14, 0x42, 0x91, 0xa1, 0xb1, 0xc1, 0x09, 0x23, 0x33, 0x52, 0xf0,
        0x15, 0x62, 0x72, 0xd1, 0x0a, 0x16, 0x24, 0x34, 0xe1, 0x25, 0xf1, 0x17, 0x18, 0x19, 0x1a, 0x26,
        0x27, 0x28, 0x29, 0x2a, 0x35, 0x36, 0x37, 0x38, 0x39, 0x3a, 0x43, 0x44, 0x45, 0x46, 0x47, 0x48,
        0x49, 0x4a, 0x53, 0x54, 0x55, 0x56, 0x57, 0x58, 0x59, 0x5a, 0x63, 0x64, 0x65, 0x66, 0x67, 0x68,
        0x69, 0x6a, 0x73, 0x74, 0x75, 0x76, 0x77, 0x78, 0x79, 0x7a, 0x82, 0x83, 0x84, 0x85, 0x86, 0x87,
        0x88, 0x89, 0x8a, 0x92, 0x93, 0x94, 0x95, 0x96, 0x97, 0x98, 0x99, 0x9a, 0xa2, 0xa3, 0xa4, 0xa5,
        0xa6, 0xa7, 0xa8, 0xa9, 0xaa, 0xb2, 0xb3, 0xb4, 0xb5, 0xb6, 0xb7, 0xb8, 0xb9, 0xba, 0xc2, 0xc3,
        0xc4, 0xc5, 0xc6, 0xc7, 0xc8, 0xc9, 0xca, 0xd2, 0xd3, 0xd4, 0xd5, 0xd6, 0xd7, 0xd8, 0xd9, 0xda,
        0xe2, 0xe3, 0xe4, 0xe5, 0xe6, 0xe7, 0xe8, 0xe9, 0xea, 0xf2, 0xf3, 0xf4, 0xf5, 0xf6, 0xf7, 0xf8,
        0xf9, 0xfa,
    )
}

/** Maps letterboxed model coordinates back to frame pixels. */
class Letterbox(val scale: Float, val padX: Float, val padY: Float) {
    fun toFrameX(x: Float) = (x - padX) / scale
    fun toFrameY(y: Float) = (y - padY) / scale
}

/** Maps crop pixel coordinates back to frame pixels (inverse of [Image.rotatedCropNhwc]). */
class CropTransform(
    private val cx: Float, private val cy: Float, private val k: Float,
    private val c: Float, private val s: Float, private val half: Float,
) {
    fun toFrame(x: Float, y: Float): Pair<Float, Float> {
        val dx = (x - half) * k
        val dy = (y - half) * k
        return (cx + dx * c - dy * s) to (cy + dx * s + dy * c)
    }
}
