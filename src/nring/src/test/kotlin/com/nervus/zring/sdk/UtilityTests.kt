package com.nervus.zring.sdk

import java.io.ByteArrayOutputStream
import java.io.PrintStream
import org.junit.jupiter.api.Test
import kotlin.test.assertEquals
import kotlin.test.assertTrue

class UtilityTests {

    @Test
    fun normalizePcmConfigDefaults() {
        val config = normalizePcmConfig()
        assertEquals(16_000, config.sampleRate)
        assertEquals(1, config.channels)
        assertEquals(16, config.bitDepth)
    }

    @Test
    fun normalizePcmConfigOverrides() {
        val config = normalizePcmConfig(sampleRate = 8_000, channels = 2, bitDepth = 8)
        assertEquals(8_000, config.sampleRate)
        assertEquals(2, config.channels)
        assertEquals(8, config.bitDepth)
    }

    @Test
    fun normalizePcmConfigPartialOverrides() {
        val config = normalizePcmConfig(sampleRate = 48_000)
        assertEquals(48_000, config.sampleRate)
        assertEquals(1, config.channels)
        assertEquals(16, config.bitDepth)
    }

    @Test
    fun formatPcmConfigDefault() {
        assertEquals("16000Hz / 1ch / 16bit", formatPcmConfig())
    }

    @Test
    fun formatPcmConfigCustom() {
        assertEquals(
            "8000Hz / 1ch / 8bit",
            formatPcmConfig(PCMConfig(sampleRate = 8_000, channels = 1, bitDepth = 8))
        )
    }

    @Test
    fun progressPrinterPrintsPercentageChanges() {
        val out = ByteArrayOutputStream()
        val original = System.out
        System.setOut(PrintStream(out))
        try {
            val printer = ProgressPrinter("download")
            printer.onProgress(0, 100)
            printer.onProgress(50, 100)
            printer.onProgress(50, 100)  // same percent, suppressed
            printer.onProgress(100, 100)
        } finally {
            System.setOut(original)
        }

        val output = out.toString()
        assertTrue(output.contains("download: 0/100 bytes (0%)"))
        assertTrue(output.contains("download: 50/100 bytes (50%)"))
        assertTrue(output.contains("download: 100/100 bytes (100%)"))
        // Same percent should not be printed twice.
        assertEquals(3, output.lines().filter { it.isNotBlank() }.size)
    }

    @Test
    fun progressPrinterHandlesZeroTotal() {
        val out = ByteArrayOutputStream()
        val original = System.out
        System.setOut(PrintStream(out))
        try {
            val printer = ProgressPrinter()
            printer.onProgress(10, 0)
        } finally {
            System.setOut(original)
        }

        assertTrue(out.toString().contains("(0%)"))
    }

    @Test
    fun progressPrinterAsHandlerWorks() {
        val out = ByteArrayOutputStream()
        val original = System.out
        System.setOut(PrintStream(out))
        try {
            val printer = ProgressPrinter("test")
            val handler = printer.asHandler()
            handler(25, 100)
        } finally {
            System.setOut(original)
        }

        assertTrue(out.toString().contains("test: 25/100 bytes (25%)"))
    }
}
