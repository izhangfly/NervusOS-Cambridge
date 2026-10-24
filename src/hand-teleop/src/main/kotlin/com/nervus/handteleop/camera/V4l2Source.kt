package com.nervus.handteleop.camera

import com.nervus.handteleop.vision.RgbFrame
import java.io.BufferedInputStream
import java.io.DataInputStream
import java.io.File
import java.util.concurrent.TimeUnit
import java.util.concurrent.atomic.AtomicLong
import java.util.concurrent.atomic.AtomicReference
import java.util.logging.Logger

/** Which V4L2 device to read: a path given outright, or the first capture node with this name. */
sealed class CameraChoice {
    data class Path(val device: String) : CameraChoice()
    data class Named(val name: String) : CameraChoice()

    override fun toString() = when (this) {
        is Path -> device
        is Named -> "\"$name\""
    }
}

/** A device's capture format, as V4L2 reports it. */
data class V4l2Format(val width: Int, val height: Int, /** FourCC, such as "YUYV" or "MJPG". */ val fourcc: String) {
    /** ffmpeg's -input_format for this FourCC, or null to let ffmpeg pick. */
    val ffmpegInput: String?
        get() = when (fourcc) {
            "YUYV", "YUY2" -> "yuyv422"
            "MJPG", "JPEG" -> "mjpeg"
            else -> null
        }
}

/** The pure parts of reading a V4L2 camera through ffmpeg, kept apart so they can be tested. */
object V4l2 {
    /**
     * The device node whose V4L2 name is [name], from /sys/class/video4linux/<node>/name. A UVC
     * camera shows up twice under one name (capture, then metadata), so the node with the lowest
     * `index` wins, then the lowest number.
     */
    fun findByName(name: String, sysRoot: File = File("/sys/class/video4linux")): String? =
        sysRoot.listFiles { f -> f.name.matches(Regex("video\\d+")) }.orEmpty()
            .filter { runCatching { File(it, "name").readText().trim() }.getOrNull() == name }
            .sortedWith(compareBy({ runCatching { File(it, "index").readText().trim().toInt() }.getOrDefault(0) }, { it.name.removePrefix("video").toInt() }))
            .firstOrNull()?.let { "/dev/${it.name}" }

    /** The V4L2 names present, for telling the operator what there is instead. */
    fun names(sysRoot: File = File("/sys/class/video4linux")): List<String> =
        sysRoot.listFiles { f -> f.name.matches(Regex("video\\d+")) }.orEmpty()
            .mapNotNull { runCatching { File(it, "name").readText().trim() }.getOrNull() }
            .distinct().sorted()

    /** Parses `v4l2-ctl --get-fmt-video`: "Width/Height : 1280/720" and "Pixel Format : 'YUYV' (...)". */
    fun parseV4l2Ctl(text: String): V4l2Format? {
        val wh = Regex("""Width/Height\s*:\s*(\d+)\s*/\s*(\d+)""").find(text) ?: return null
        val fcc = Regex("""Pixel Format\s*:\s*'([^']+)'""").find(text)?.groupValues?.get(1)?.trim() ?: ""
        return V4l2Format(wh.groupValues[1].toInt(), wh.groupValues[2].toInt(), fcc)
    }

    /** Parses `ffprobe -show_entries stream=width,height,codec_name,pix_fmt -of default=nw=1`. */
    fun parseFfprobe(text: String): V4l2Format? {
        val kv = text.lines().mapNotNull { l -> l.split('=', limit = 2).takeIf { it.size == 2 }?.let { it[0].trim() to it[1].trim() } }.toMap()
        val w = kv["width"]?.toIntOrNull() ?: return null
        val h = kv["height"]?.toIntOrNull() ?: return null
        val fcc = when {
            kv["codec_name"] == "mjpeg" -> "MJPG"
            kv["pix_fmt"] == "yuyv422" -> "YUYV"
            else -> ""
        }
        return V4l2Format(w, h, fcc)
    }

    /** ffmpeg reading [device] and writing raw RGB24 frames to stdout. */
    fun ffmpegCommand(device: String, f: V4l2Format): List<String> = buildList {
        // Wall-clock timestamps: a v4l2loopback camera (the Surface bridge) hands out garbage ones, and ffmpeg
        // then holds frames back (one frame in 5 s instead of 30 fps).
        addAll(listOf("ffmpeg", "-hide_banner", "-loglevel", "error", "-nostdin",
            "-use_wallclock_as_timestamps", "1", "-f", "v4l2"))
        f.ffmpegInput?.let { addAll(listOf("-input_format", it)) }
        addAll(listOf("-video_size", "${f.width}x${f.height}", "-i", device, "-f", "rawvideo", "-pix_fmt", "rgb24", "-"))
    }

    /** RGB24 bytes to the 0xAARRGGBB pixels the rest of the app uses. */
    fun rgb24ToArgb(src: ByteArray, out: IntArray) {
        var s = 0
        for (i in out.indices) {
            out[i] = (0xFF shl 24) or ((src[s].toInt() and 0xFF) shl 16) or ((src[s + 1].toInt() and 0xFF) shl 8) or (src[s + 2].toInt() and 0xFF)
            s += 3
        }
    }
}

/**
 * Frames straight from a V4L2 device, for running without NervusOS: ffmpeg decodes the device to
 * raw RGB24 on its stdout, and each fixed-size frame read from there becomes the latest frame. The
 * device is found by name and its format probed each time it is opened (the Surface's camera bridge
 * can come and go), and ffmpeg is restarted whenever it exits or stalls. While there are no frames,
 * [error] says why ("no camera: ..."); nothing here throws at the UI.
 */
class V4l2Source(private val choice: CameraChoice, private val log: Logger) : FrameSource {
    private val latest = AtomicReference<RgbFrame?>(null)
    private var thread: Thread? = null
    @Volatile private var process: Process? = null
    @Volatile private var running = false

    @Volatile
    override var fps: Float = 0f
        private set

    @Volatile
    override var format: String = "-"
        private set

    @Volatile
    override var error: String? = null
        private set

    override val waiting = "Opening camera $choice…"

    override fun latest(): RgbFrame? = latest.get()

    override fun start() {
        running = true
        thread = Thread({ loop() }, "camera").apply { isDaemon = true; start() }
    }

    private fun loop() {
        var seq = 0L
        while (running) {
            try {
                val device = when (choice) {
                    is CameraChoice.Path -> choice.device
                    is CameraChoice.Named -> V4l2.findByName(choice.name)
                        ?: throw NoCamera("no V4L2 device named \"${choice.name}\" (found: ${V4l2.names().joinToString().ifEmpty { "none" }})")
                }
                if (!File(device).exists()) throw NoCamera("$device does not exist")
                val fmt = probe(device) ?: throw NoCamera("could not read the format of $device")
                val cmd = V4l2.ffmpegCommand(device, fmt)
                log.info("camera: ${cmd.joinToString(" ")}")
                val p = ProcessBuilder(cmd).redirectError(ProcessBuilder.Redirect.PIPE).start()
                process = p
                val stderr = StringBuilder()
                Thread({ p.errorStream.bufferedReader().forEachLine { synchronized(stderr) { stderr.setLength(0); stderr.append(it) } } }, "ffmpeg-stderr")
                    .apply { isDaemon = true; start() }
                format = "${fmt.fourcc.ifEmpty { "?" }} ${fmt.width}x${fmt.height} · $device"
                val frameBytes = fmt.width * fmt.height * 3
                val buf = ByteArray(frameBytes)
                val input = DataInputStream(BufferedInputStream(p.inputStream, frameBytes))
                // A device that stops delivering (the bridge went away) leaves ffmpeg blocked, not dead:
                // kill it after a quiet spell so the loop can look for the camera again.
                val lastFrame = AtomicLong(System.nanoTime())
                val watchdog = Thread({
                    try {
                        while (p.isAlive) {
                            Thread.sleep(500)
                            if (System.nanoTime() - lastFrame.get() > 4_000_000_000L) p.destroyForcibly()
                        }
                    } catch (e: InterruptedException) {
                        // ffmpeg is being shut down anyway
                    }
                }, "ffmpeg-watchdog").apply { isDaemon = true; start() }
                var count = 0
                var windowStart = System.nanoTime()
                try {
                    while (running) {
                        input.readFully(buf)
                        val now = System.nanoTime()
                        lastFrame.set(now)
                        val argb = IntArray(fmt.width * fmt.height)
                        V4l2.rgb24ToArgb(buf, argb)
                        latest.set(RgbFrame(fmt.width, fmt.height, argb, now, ++seq))
                        error = null
                        count++
                        if (now - windowStart > 1_000_000_000L) {
                            fps = count * 1e9f / (now - windowStart)
                            count = 0
                            windowStart = now
                        }
                    }
                } catch (e: java.io.IOException) {
                    if (running) {
                        val why = synchronized(stderr) { stderr.toString() }.ifEmpty { "ffmpeg stopped" }
                        throw NoCamera("$device: $why")
                    }
                } finally {
                    p.destroy()
                    if (!p.waitFor(1, TimeUnit.SECONDS)) p.destroyForcibly()
                    watchdog.interrupt()
                }
            } catch (e: Exception) {
                if (!running) break
                val msg = if (e is NoCamera) e.message else (e.message ?: e.javaClass.simpleName)
                if (error != "no camera: $msg") log.warning("camera: $msg; retrying in 2 s")
                error = "no camera: $msg"
                fps = 0f
                latest.set(null)
                runCatching { Thread.sleep(2000) }
            }
        }
    }

    /** The device's current format: v4l2-ctl first, ffprobe if that isn't there. */
    private fun probe(device: String): V4l2Format? =
        run("v4l2-ctl", "-d", device, "--get-fmt-video")?.let { V4l2.parseV4l2Ctl(it) }
            ?: run("ffprobe", "-v", "error", "-f", "v4l2", "-i", device, "-select_streams", "v:0",
                "-show_entries", "stream=width,height,codec_name,pix_fmt", "-of", "default=nw=1")?.let { V4l2.parseFfprobe(it) }

    private fun run(vararg cmd: String): String? = runCatching {
        val p = ProcessBuilder(*cmd).redirectErrorStream(true).start()
        val out = p.inputStream.bufferedReader().readText()
        if (!p.waitFor(5, TimeUnit.SECONDS)) { p.destroyForcibly(); return null }
        out.takeIf { p.exitValue() == 0 }
    }.getOrNull()

    override fun close() {
        running = false
        process?.destroy()
        thread?.join(2000)
        process?.destroyForcibly()
    }

    private class NoCamera(message: String) : Exception(message)

    companion object {
        /** The Surface's camera bridge (nervus-surface-camera.service) publishes under this name. */
        const val DEFAULT_NAME = "Surface Camera"
    }
}
