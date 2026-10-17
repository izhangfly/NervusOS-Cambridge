package com.nervus.handteleop.camera

import com.nervus.handteleop.vision.Image
import com.nervus.handteleop.vision.RgbFrame
import com.nervus.sdk.transfer.TransferConsumer
import io.github.nervusos.iface.camera.v1.CameraMethod
import io.github.nervusos.iface.camera.v1.CloseStreamRequest
import io.github.nervusos.iface.camera.v1.DescribeStreamRequest
import io.github.nervusos.iface.camera.v1.OpenStreamRequest
import io.github.nervusos.iface.camera.v1.OpenStreamResponse
import io.github.nervusos.iface.camera.v1.PixelFormat
import io.github.nervusos.iface.camera.v1.StreamDescription
import io.github.nervusos.iface.camera.v1.StreamFormat
import java.util.concurrent.atomic.AtomicReference
import java.util.logging.Logger

/** The calls the camera needs from the app's NervusOS connection. */
fun interface Caller {
    fun call(interfaceId: String, methodId: Int, payload: ByteArray, timeoutSeconds: Long): ByteArray
}

/**
 * Live frames from the front camera, through NervusOS: Camera@1 on nervus.camerad opens the stream,
 * and frames arrive over the Transfer data plane (a shared-memory ring) without passing through
 * the kernel. Needs perm.camera.capture.
 *
 * Picks 640x480 at 30 fps, uncompressed YUYV when the camera offers it (no decode cost, no JPEG
 * quirks), MJPEG otherwise.
 */
class CameraSource(private val caller: Caller, private val log: Logger) : AutoCloseable {
    private val latest = AtomicReference<RgbFrame?>(null)
    private var consumer: TransferConsumer? = null
    private var streamId: Long = 0
    private var thread: Thread? = null

    @Volatile
    var fps: Float = 0f
        private set

    @Volatile
    var format: String = "-"
        private set

    @Volatile
    var error: String? = null
        private set

    @Volatile
    private var running = false

    fun latest(): RgbFrame? = latest.get()

    fun start() {
        running = true
        thread = Thread({ loop() }, "camera").apply { isDaemon = true; start() }
    }

    private fun open(): Pair<StreamFormat, Int> {
        val desc = StreamDescription.parseFrom(
            caller.call(CAMERA, CameraMethod.CAMERA_METHOD_DESCRIBE_STREAM_VALUE, DescribeStreamRequest.getDefaultInstance().toByteArray(), 10)
        )
        val pick = pickFormat(desc.formatsList) ?: throw IllegalStateException("the camera offers no usable format")
        val rate = if (30 in pick.frameRatesList) 30 else pick.frameRatesList.maxOrNull() ?: 30
        val req = OpenStreamRequest.newBuilder()
            .setFormat(StreamFormat.newBuilder().setPixelFormat(pick.pixelFormat).setWidth(pick.width).setHeight(pick.height))
            .setFrameRate(rate)
            .build()
        val resp = OpenStreamResponse.parseFrom(caller.call(CAMERA, CameraMethod.CAMERA_METHOD_OPEN_STREAM_VALUE, req.toByteArray(), 10))
        streamId = resp.streamId
        consumer = TransferConsumer.attach(resp.transfer)
        format = "${pick.pixelFormat.name.removePrefix("PIXEL_FORMAT_")} ${pick.width}x${pick.height} @$rate"
        log.info("camera stream open: $format, transfer ${consumer!!.mode}")
        return pick to rate
    }

    private fun loop() {
        while (running) {
            try {
                val (fmt, _) = open()
                error = null
                val w = fmt.width
                val h = fmt.height
                var count = 0
                var windowStart = System.nanoTime()
                while (running) {
                    val f = consumer!!.readLatest(500) ?: continue
                    val argb = IntArray(w * h)
                    val ok = when (fmt.pixelFormat) {
                        PixelFormat.PIXEL_FORMAT_YUYV -> {
                            if (f.payload.size < w * h * 2) false else { Image.yuyvToArgb(f.payload, w, h, argb); true }
                        }
                        PixelFormat.PIXEL_FORMAT_MJPEG -> Image.decodeMjpeg(f.payload, argb, w, h)
                        else -> false
                    }
                    if (!ok) continue
                    latest.set(RgbFrame(w, h, argb, f.timestampNanos, f.sequence))
                    count++
                    val now = System.nanoTime()
                    if (now - windowStart > 1_000_000_000L) {
                        fps = count * 1e9f / (now - windowStart)
                        count = 0
                        windowStart = now
                    }
                }
            } catch (e: Exception) {
                error = e.message ?: e.javaClass.simpleName
                fps = 0f
                log.warning("camera: ${error}; retrying in 2 s")
                closeStream()
                if (running) Thread.sleep(2000)
            }
        }
        closeStream()
    }

    private fun closeStream() {
        runCatching { consumer?.close() }
        consumer = null
        if (streamId != 0L) {
            val id = streamId
            streamId = 0
            runCatching {
                caller.call(CAMERA, CameraMethod.CAMERA_METHOD_CLOSE_STREAM_VALUE,
                    CloseStreamRequest.newBuilder().setStreamId(id).build().toByteArray(), 5)
            }
        }
    }

    override fun close() {
        running = false
        thread?.join(2000)
        closeStream()
    }

    companion object {
        const val CAMERA = "nervus.interface.camera"

        fun pickFormat(formats: List<StreamFormat>): StreamFormat? {
            fun find(pf: PixelFormat, w: Int, h: Int) = formats.firstOrNull {
                it.pixelFormat == pf && it.width == w && it.height == h && it.frameRatesList.any { r -> r >= 25 }
            }
            return find(PixelFormat.PIXEL_FORMAT_YUYV, 640, 480)
                ?: find(PixelFormat.PIXEL_FORMAT_MJPEG, 640, 480)
                ?: formats.filter {
                    (it.pixelFormat == PixelFormat.PIXEL_FORMAT_YUYV || it.pixelFormat == PixelFormat.PIXEL_FORMAT_MJPEG) &&
                        it.width in 320..1280
                }.minByOrNull { kotlin.math.abs(it.width - 640) }
        }
    }
}
