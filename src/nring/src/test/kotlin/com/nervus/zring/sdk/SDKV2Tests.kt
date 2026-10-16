package com.nervus.zring.sdk

import kotlin.time.Duration.Companion.seconds
import org.junit.jupiter.api.Test
import kotlin.test.assertContentEquals
import kotlin.test.assertEquals
import kotlin.test.assertTrue

class SDKV2Tests {

    @Test
    fun sdkVersionAndScanDefaultMatchUpstreamV2() {
        assertEquals("0.4.1", RingSoundSDK.VERSION)
        assertEquals(25.seconds, RingSoundDefaults.SCAN_TIMEOUT)
    }

    @Test
    fun nusWritesAlwaysUseTwentyByteChunks() {
        val source = ByteArray(41) { it.toByte() }

        val chunks = NUSWriteStrategy.chunks(source)

        assertEquals(20, RingSoundDefaults.NUS_WRITE_CHUNK_SIZE)
        assertEquals(listOf(20, 20, 1), chunks.map { it.size })
        assertContentEquals(source, chunks.fold(ByteArray(0)) { acc, chunk -> acc + chunk })
    }

    @Test
    fun nusWriteStrategyHandlesEmptyPayload() {
        assertTrue(NUSWriteStrategy.chunks(ByteArray(0)).isEmpty())
    }
}
