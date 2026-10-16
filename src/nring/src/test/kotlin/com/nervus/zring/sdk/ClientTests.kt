package com.nervus.zring.sdk

import kotlin.time.Duration.Companion.seconds
import kotlinx.coroutines.CompletableDeferred
import kotlinx.coroutines.async
import kotlinx.coroutines.delay
import kotlinx.coroutines.runBlocking
import kotlinx.coroutines.supervisorScope
import kotlinx.coroutines.withTimeout
import org.junit.jupiter.api.Test
import kotlin.test.assertContentEquals
import kotlin.test.assertEquals
import kotlin.test.assertTrue
import kotlin.test.fail

class ClientTests {

    @Test
    fun requestWritesCommandAndRoutesFragmentedResponse() = runBlocking {
        val transport = MockTransport()
        val client = RingSoundClient(transport = transport, commandTimeout = 1.seconds)
        client.connect()
        val response = RingSoundProtocol.encodePacket(command = 0x0102, body = byteArrayOf(0, 0))
        transport.onWrite = { request ->
            assertEquals(0x0101, RingSoundProtocol.decodePacket(request).command)
            transport.push(response.copyOf(5))
            transport.push(response.copyOfRange(5, response.size))
        }

        val packet = client.request(command = 0x0101, responseCommand = 0x0102)

        assertEquals(0x0102, packet.command)
        assertContentEquals(byteArrayOf(0, 0), packet.body)
    }

    @Test
    fun waitTimesOutWithCommand() = runBlocking {
        val transport = MockTransport()
        val client = RingSoundClient(transport = transport, commandTimeout = 0.01.seconds)
        client.connect()

        try {
            client.waitForCommand(0x0701)
            fail("Expected timeout")
        } catch (error: RingSoundError.Timeout) {
            assertEquals(0x0701, error.command)
        }
    }

    @Test
    fun disconnectFailsPendingWait() = runBlocking {
        supervisorScope {
            val transport = MockTransport()
            val client = RingSoundClient(transport = transport, commandTimeout = 5.seconds)
            client.connect()

            val waiting = async { client.waitForCommand(0x0702) }
            delay(5)
            transport.dropConnection()

            try {
                waiting.await()
                fail("Expected transport error")
            } catch (error: RingSoundError.Transport) {
                assertTrue(error.message.orEmpty().contains("disconnected"))
            }
        }
    }

    @Test
    fun queuedPacketIsAvailableToLaterWaiter() = runBlocking {
        val transport = MockTransport()
        val client = RingSoundClient(transport = transport, commandTimeout = 1.seconds)
        client.connect()
        transport.push(RingSoundProtocol.encodePacket(command = 0x0701, body = byteArrayOf(0, 0, 0, 7)))
        delay(5)

        val packet = client.waitForCommand(0x0701)

        assertEquals(
            7u,
            RingSoundParsers.doubleTapEvent(packet.body).timestampMilliseconds
        )
    }

    @Test
    fun packetHandlerReceivesEvents() = runBlocking {
        val transport = MockTransport()
        val client = RingSoundClient(transport = transport, commandTimeout = 1.seconds)
        client.connect()
        val received = CompletableDeferred<Boolean>()
        val token = client.addPacketHandler(0x0701) { packet ->
            if (packet.body.contentEquals(byteArrayOf(0, 0, 0, 8))) {
                received.complete(true)
            }
        }

        transport.push(RingSoundProtocol.encodePacket(command = 0x0701, body = byteArrayOf(0, 0, 0, 8)))
        assertTrue(withTimeout(1_000) { received.await() })
        client.removePacketHandler(token)
    }

    @Test
    fun highLevelSystemInfoUsesExpectedCommands() = runBlocking {
        val transport = MockTransport()
        val client = RingSoundClient(transport = transport, commandTimeout = 1.seconds)
        client.connect()
        val body =
            "0000000456322e300000007b000003e8000001900055010002534e0003435055000452696e67".hexToByteArray()
        transport.onWrite = { request ->
            assertEquals(
                SystemCommand.GET_INFO.value,
                RingSoundProtocol.decodePacket(request).command
            )
            transport.push(
                RingSoundProtocol.encodePacket(
                    command = SystemCommand.INFO_RESPONSE.value,
                    body = body
                )
            )
        }

        val info = client.getSystemInfo()

        assertEquals("SN", info.serialNumber)
    }

    @Test
    fun audioFrameRequestIncludesFirmwarePadding() = runBlocking {
        val transport = MockTransport()
        val client = RingSoundClient(transport = transport, commandTimeout = 1.seconds)
        client.connect()
        transport.onWrite = { request ->
            val packet = RingSoundProtocol.decodePacket(request)
            assertEquals(AudioCommand.NEXT_FRAME.value, packet.command)
            assertEquals("000000000003000000080000", packet.body.hexString)

            val response = BinaryWriter()
            response.writeUInt16(0)
            response.writeUInt32(3u)
            response.writeUInt32(8u)
            response.writeUInt32(1u)
            response.writeUInt8(1)
            response.writeUInt8(0xAA)
            transport.push(
                RingSoundProtocol.encodePacket(
                    command = AudioCommand.DATA_FRAME.value,
                    body = response.data
                )
            )
        }

        val frame = client.readAudioFrame(fileIndex = 3u, frameOffset = 8u)

        assertContentEquals(byteArrayOf(0xAA.toByte()), frame.data)
    }

    @Test
    fun mockTransportConformsToTransport() {
        val transport: RingSoundTransport = MockTransport()
        assertTrue(transport is MockTransport)
    }
}
