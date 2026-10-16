package com.nervus.zring.sdk

import java.security.MessageDigest
import org.junit.jupiter.api.Test
import kotlin.test.assertContentEquals
import kotlin.test.assertEquals
import kotlin.test.assertNull
import kotlin.test.assertTrue

class AudioCodecTests {

    @Test
    fun pcmConfigNormalizationAndFormatting() {
        assertEquals(
            PCMConfig(sampleRate = 1, channels = 2, bitDepth = 16),
            PCMConfig(sampleRate = 0, channels = 4, bitDepth = 24)
        )
        assertEquals(
            "8000Hz / 1ch / 8bit",
            PCMConfig(sampleRate = 8_000, channels = 1, bitDepth = 8).toString()
        )
    }

    @Test
    fun wavMatchesPythonVector() {
        val wav = AudioCodec.buildWAV(pcm = byteArrayOf(1, 2, 3, 4))

        assertEquals(
            "524946462800000057415645666d74201000000001000100803e0000007d000002001000646174610400000001020304",
            wav.hexString
        )
        assertTrue(AudioCodec.isWAV(wav))
    }

    @Test
    fun packetizedSpeexUsesLittleEndianLengthsAndPadding() {
        val raw = byteArrayOf(2, 0, 0xAA.toByte(), 0xBB.toByte(), 1, 0, 0xCC.toByte(), 0, 0, 0xFF.toByte())
        val packets = AudioCodec.parsePacketizedSpeex(raw)
        assertEquals(2, packets?.size)
        assertContentEquals(byteArrayOf(0xAA.toByte(), 0xBB.toByte()), packets?.get(0))
        assertContentEquals(byteArrayOf(0xCC.toByte()), packets?.get(1))
        assertNull(AudioCodec.parsePacketizedSpeex(byteArrayOf(3, 0, 0xAA.toByte())))
    }

    @Test
    fun legacyFramedBlocksAndRawPacketSplitting() {
        val first = byteArrayOf(1, 0, 0xAA.toByte(), 0)
        val second = byteArrayOf(1, 0, 0xBB.toByte(), 0)
        val packets = AudioCodec.parsePacketizedSpeex(
            first + second,
            allowFramedBlocks = true,
            framedBlockSize = 4
        )
        assertEquals(2, packets?.size)
        assertContentEquals(byteArrayOf(0xAA.toByte()), packets?.get(0))
        assertContentEquals(byteArrayOf(0xBB.toByte()), packets?.get(1))

        val raw = AudioCodec.splitRawSpeexPackets(ByteArray(40) { 1 })
        assertEquals(2, raw?.size)
        assertContentEquals(ByteArray(20) { 1 }, raw?.get(0))
        assertContentEquals(ByteArray(20) { 1 }, raw?.get(1))
        assertNull(AudioCodec.splitRawSpeexPackets(ByteArray(21) { 1 }))
    }

    @Test
    fun speexDefaultsMatchPython() {
        assertEquals(0, AudioCodec.speexMode(sampleRate = 8_000))
        assertEquals(1, AudioCodec.speexMode(sampleRate = 16_000))
        assertEquals(2, AudioCodec.speexMode(sampleRate = 48_000))
        assertEquals(160, AudioCodec.frameSize(sampleRate = 8_000))
        assertEquals(320, AudioCodec.frameSize(sampleRate = 16_000))
        assertEquals(640, AudioCodec.frameSize(sampleRate = 48_000))
        assertEquals(20, AudioCodec.bitsSize(quality = 3))
        assertEquals(46, AudioCodec.bitsSize(quality = 9))
        assertEquals(20, AudioCodec.bitsSize(quality = 99))
    }

    @Test
    fun oggSpeexMatchesPythonByteForByte() {
        val ogg = AudioCodec.buildOggSpeex(
            packets = listOf(
                byteArrayOf(0xAA.toByte(), 0xBB.toByte()),
                byteArrayOf(0xCC.toByte())
            )
        )

        assertTrue(AudioCodec.isOggSpeex(ogg))
        assertEquals(220, ogg.size)
        val digest = MessageDigest.getInstance("SHA-256").digest(ogg)
        assertEquals(
            "7233fa97a3960e682198a233b523dc64370c6cca63cfff27a5d09f95d2a56276",
            digest.joinToString("") { "%02x".format(it) }
        )
    }

    @Test
    fun normalizeDecodedPCMRemovesPerPacketPadding() {
        val first = ByteArray(640) { 0x11 } + ByteArray(640)
        val second = ByteArray(640) { 0x22 } + ByteArray(640)
        val normalized = AudioCodec.normalizeDecodedPCM(first + second, packetCount = 2)

        assertContentEquals(ByteArray(640) { 0x11 } + ByteArray(640) { 0x22 }, normalized)
    }

    @Test
    fun injectedSpeexDecoderBuildsPlayableWAV() {
        val decoder: SpeexDecoder = { _, options ->
            SpeexDecodeResult(
                pcmData = byteArrayOf(1, 2, 3, 4),
                pcmConfig = options.pcmConfig,
                sourceType = "test-speex",
                sourceExtension = "spx",
                packetCount = 1
            )
        }

        val playable = AudioCodec.buildPlayableAudio(
            data = byteArrayOf(2, 0, 0xAA.toByte(), 0xBB.toByte()),
            decoder = decoder
        )

        assertEquals("speex-decode", playable.playMode)
        assertEquals("test-speex", playable.sourceType)
        assertTrue(AudioCodec.isWAV(playable.data))
    }

    @Test
    fun wavInputIsReturnedWithoutDecoder() {
        val wav = AudioCodec.buildWAV(pcm = byteArrayOf(1, 2))
        val playable = AudioCodec.buildPlayableAudio(data = wav)

        assertContentEquals(wav, playable.data)
        assertEquals("direct", playable.playMode)
        assertEquals("wav", playable.sourceType)
    }
}
