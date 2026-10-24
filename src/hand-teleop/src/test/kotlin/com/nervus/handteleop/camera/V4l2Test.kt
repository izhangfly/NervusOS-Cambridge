package com.nervus.handteleop.camera

import com.nervus.handteleop.app.StandaloneOptions
import java.io.File
import java.nio.file.Files
import kotlin.test.Test
import kotlin.test.assertContentEquals
import kotlin.test.assertEquals
import kotlin.test.assertFailsWith
import kotlin.test.assertNull

class V4l2Test {
    private fun sysfs(vararg nodes: Triple<String, String, Int?>): File {
        val root = Files.createTempDirectory("v4l2").toFile()
        for ((node, name, index) in nodes) {
            val d = File(root, node).apply { mkdirs() }
            File(d, "name").writeText("$name\n")
            index?.let { File(d, "index").writeText("$it\n") }
        }
        return root
    }

    @Test
    fun `a camera is found by its V4L2 name, capture node first`() {
        val root = sysfs(
            Triple("v4l-subdev4", "Surface Camera", null), // not a video node
            Triple("video35", "UVC Camera (046d:0825)", 1),
            Triple("video34", "UVC Camera (046d:0825)", 0),
            Triple("video60", "Surface Camera", 0),
            Triple("video2", "Intel IPU6 ISYS Capture 2", 0),
        )
        assertEquals("/dev/video60", V4l2.findByName("Surface Camera", root))
        assertEquals("/dev/video34", V4l2.findByName("UVC Camera (046d:0825)", root), "the metadata node has index 1")
        assertNull(V4l2.findByName("Nope", root))
        assertEquals(listOf("Intel IPU6 ISYS Capture 2", "Surface Camera", "UVC Camera (046d:0825)"), V4l2.names(root))
        root.deleteRecursively()
    }

    @Test
    fun `the format is read from v4l2-ctl or ffprobe`() {
        val ctl = """
            Format Video Capture:
            	Width/Height      : 960/720
            	Pixel Format      : 'YUYV' (YUYV 4:2:2)
            	Field             : None
        """.trimIndent()
        assertEquals(V4l2Format(960, 720, "YUYV"), V4l2.parseV4l2Ctl(ctl))
        assertNull(V4l2.parseV4l2Ctl("VIDIOC_G_FMT: failed: Invalid argument"))
        assertEquals(V4l2Format(1280, 720, "MJPG"), V4l2.parseFfprobe("codec_name=mjpeg\nwidth=1280\nheight=720\npix_fmt=yuvj422p\n"))
        assertEquals(V4l2Format(960, 720, "YUYV"), V4l2.parseFfprobe("codec_name=rawvideo\nwidth=960\nheight=720\npix_fmt=yuyv422\n"))
    }

    @Test
    fun `ffmpeg is asked for the probed size and format, as raw RGB on stdout`() {
        assertEquals(
            listOf("ffmpeg", "-hide_banner", "-loglevel", "error", "-nostdin", "-use_wallclock_as_timestamps", "1", "-f", "v4l2", "-input_format", "yuyv422",
                "-video_size", "960x720", "-i", "/dev/video60", "-f", "rawvideo", "-pix_fmt", "rgb24", "-"),
            V4l2.ffmpegCommand("/dev/video60", V4l2Format(960, 720, "YUYV")),
        )
        assertEquals("yuyv422", V4l2Format(1, 1, "YUY2").ffmpegInput)
        assertEquals("mjpeg", V4l2Format(1, 1, "MJPG").ffmpegInput)
        assertNull(V4l2Format(1, 1, "NV12").ffmpegInput, "anything else: ffmpeg picks")
    }

    @Test
    fun `RGB24 becomes ARGB pixels in order`() {
        val out = IntArray(2)
        V4l2.rgb24ToArgb(byteArrayOf(0x11, 0x22, 0x33, 0xFF.toByte(), 0x00, 0x80.toByte()), out)
        assertContentEquals(intArrayOf(0xFF112233.toInt(), 0xFFFF0080.toInt()), out)
    }

    @Test
    fun `standalone options`() {
        val d = StandaloneOptions.parse(listOf("--standalone"))
        assertEquals(CameraChoice.Named("Surface Camera"), d.camera)
        assertEquals("http://127.0.0.1:8770", d.driver)
        assertEquals(CameraChoice.Path("/dev/video3"), StandaloneOptions.parse(listOf("--standalone", "--camera", "/dev/video3")).camera)
        assertEquals(CameraChoice.Named("UVC Camera"), StandaloneOptions.parse(listOf("--camera-name", "UVC Camera", "--standalone")).camera)
        assertEquals("http://localhost:9000", StandaloneOptions.parse(listOf("--standalone", "--driver", "http://localhost:9000/")).driver)
        assertFailsWith<IllegalArgumentException> { StandaloneOptions.parse(listOf("--standalone", "--camera")) }
        assertFailsWith<IllegalArgumentException> { StandaloneOptions.parse(listOf("--standalone", "--bogus")) }
        assertFailsWith<IllegalArgumentException>("the driver moves the arm unauthenticated: loopback only") {
            StandaloneOptions.parse(listOf("--standalone", "--driver", "http://10.0.0.5:8770"))
        }
    }
}
