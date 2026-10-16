package com.nervus.zring.sdk

import org.junit.jupiter.api.Test
import org.junit.jupiter.api.assertThrows
import kotlin.test.assertContentEquals
import kotlin.test.assertEquals
import kotlin.test.assertTrue
import kotlin.test.fail

class ParserTests {

    @Test
    fun parseSystemInfoMatchesPythonVector() {
        val body =
            "0000000456322e300000007b000003e8000001900055010002534e0003435055000452696e67".hexToByteArray()

        val info = RingSoundParsers.systemInfo(body)

        assertEquals("V2.0", info.firmwareVersion)
        assertEquals(123u, info.systemTime)
        assertEquals(1_000u, info.audioStorageTotal)
        assertEquals(400u, info.audioStorageAvailable)
        assertEquals(85, info.batteryPercent)
        assertTrue(info.batteryCharging)
        assertEquals("SN", info.serialNumber)
        assertEquals("CPU", info.cpuID)
        assertEquals("Ring", info.model)
    }

    @Test
    fun nonzeroDeviceErrorPreservesCodeAndMessage() {
        try {
            RingSoundParsers.audioFileInfo(byteArrayOf(0x00, 0x02))
            fail("Expected device error")
        } catch (error: RingSoundError.Device) {
            assertEquals(2, error.code)
            assertEquals("device busy", error.deviceMessage)
        }
    }

    @Test
    fun parseAudioFileInfoRejectsTrailingBytes() {
        val body = BinaryWriter()
        body.writeUInt16(0)
        body.writeUInt32(7u)
        body.writeUInt32(1_700_000_000u)
        body.writeUInt32(1_234u)
        val info = RingSoundParsers.audioFileInfo(body.data)
        assertEquals(AudioFileInfo(fileIndex = 7u, recordTime = 1_700_000_000u, dataSize = 1_234u), info)

        body.writeUInt8(0xFF)
        assertThrows<RingSoundError.Protocol> {
            RingSoundParsers.audioFileInfo(body.data)
        }
    }

    @Test
    fun parseAudioFrameValidatesDeclaredSize() {
        val body = BinaryWriter()
        body.writeUInt16(0)
        body.writeUInt32(3u)
        body.writeUInt32(8u)
        body.writeUInt32(2u)
        body.writeUInt8(1)
        body.write(byteArrayOf(0xAA.toByte(), 0xBB.toByte()))

        val frame = RingSoundParsers.audioDataFrame(body.data)
        assertEquals(3u, frame.fileIndex)
        assertEquals(8u, frame.frameOffset)
        assertEquals(2u, frame.frameSize)
        assertTrue(frame.isEnd)
        assertContentEquals(byteArrayOf(0xAA.toByte(), 0xBB.toByte()), frame.data)

        assertThrows<RingSoundError.Protocol> {
            RingSoundParsers.audioDataFrame(body.data.dropLast(1).toByteArray())
        }
    }

    @Test
    fun parseSensorBatchMatchesPythonVector() {
        val body = "00000000000700010010000004d2ffff0002fffd0004fffb0006".hexToByteArray()

        val batch = RingSoundParsers.sensorDataBatch(body)

        assertEquals(7u, batch.sequenceStart)
        assertEquals(1, batch.frameCount)
        assertEquals(16, batch.sampleSize)
        assertEquals(
            listOf(
                SensorDataSample(
                    timestampMilliseconds = 1_234u,
                    accelerationX = -1,
                    accelerationY = 2,
                    accelerationZ = -3,
                    gyroscopeX = 4,
                    gyroscopeY = -5,
                    gyroscopeZ = 6
                )
            ),
            batch.samples
        )
    }

    @Test
    fun sensorBatchRejectsUnsupportedSampleSizeAndWrongLength() {
        val wrongSize = BinaryWriter()
        wrongSize.writeUInt16(0)
        wrongSize.writeUInt32(0u)
        wrongSize.writeUInt16(0)
        wrongSize.writeUInt16(15)
        assertThrows<RingSoundError.Protocol> {
            RingSoundParsers.sensorDataBatch(wrongSize.data)
        }

        val wrongLength = BinaryWriter()
        wrongLength.writeUInt16(0)
        wrongLength.writeUInt32(0u)
        wrongLength.writeUInt16(1)
        wrongLength.writeUInt16(16)
        assertThrows<RingSoundError.Protocol> {
            RingSoundParsers.sensorDataBatch(wrongLength.data)
        }
    }

    @Test
    fun eventParsersAndGestureNames() {
        assertEquals(
            SensorDoubleTapEvent(timestampMilliseconds = 9u),
            RingSoundParsers.doubleTapEvent(byteArrayOf(0, 0, 0, 9))
        )
        assertEquals(
            SensorGestureEvent(timestampMilliseconds = 10u, gestureID = 3),
            RingSoundParsers.gestureEvent(byteArrayOf(0, 0, 0, 10, 3))
        )
        assertEquals("wave", RingSoundParsers.gestureName(3))
        assertEquals("unknown(99)", RingSoundParsers.gestureName(99))
        assertThrows<RingSoundError.Protocol> {
            RingSoundParsers.gestureEvent(byteArrayOf(0, 0, 0, 10, 3, 0))
        }
    }
}
