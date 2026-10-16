package com.nervus.zring.sdk

import org.junit.jupiter.api.Test
import org.junit.jupiter.api.assertThrows
import kotlin.test.assertContentEquals
import kotlin.test.assertEquals
import kotlin.test.assertTrue

class PacketCodecTests {

    @Test
    fun crc16MatchesPythonAndStandardVector() {
        assertEquals(0x29B1, RingSoundProtocol.crc16("123456789".encodeToByteArray()))
        assertEquals(0xADAD, RingSoundProtocol.crc16(byteArrayOf(0x01, 0x02, 0x03)))
    }

    @Test
    fun encodePacketMatchesPythonVector() {
        val encoded = RingSoundProtocol.encodePacket(
            command = 0x0501,
            body = byteArrayOf(0x01, 0x02, 0x03)
        )
        assertEquals("3f0004050100000003adad010203", encoded.hexString)
    }

    @Test
    fun decodePacketReadsHeaderAndBody() {
        val encoded = "3f0004070200000003514a616263".hexToByteArray()
        val packet = RingSoundProtocol.decodePacket(encoded)

        assertEquals(0x0702, packet.command)
        assertEquals(4, packet.version)
        assertEquals(0x514A, packet.bodyCRC)
        assertContentEquals("abc".encodeToByteArray(), packet.body)
    }

    @Test
    fun decodePacketRejectsBadMagicVersionLengthAndCRC() {
        assertThrows<RingSoundError.Protocol> {
            RingSoundProtocol.decodePacket("0000040101000000000000".hexToByteArray())
        }
        assertThrows<RingSoundError.Protocol> {
            RingSoundProtocol.decodePacket("3f00050101000000000000".hexToByteArray())
        }
        assertThrows<RingSoundError.Protocol> {
            RingSoundProtocol.decodePacket("3f00040101000000010000".hexToByteArray())
        }
        assertThrows<RingSoundError.Protocol> {
            RingSoundProtocol.decodePacket("3f00040501000000030000010203".hexToByteArray())
        }
    }

    @Test
    fun packetStreamReassemblesFragmentsSkipsNoiseAndEmitsMultiplePackets() {
        val first = RingSoundProtocol.encodePacket(command = 0x0101)
        val second = RingSoundProtocol.encodePacket(command = 0x0701, body = byteArrayOf(0, 0, 0, 9))
        val stream = PacketStream()

        assertTrue(stream.feed(byteArrayOf(0xAA.toByte(), 0xBB.toByte()) + first.copyOf(5)).isEmpty())
        val packets = stream.feed(first.copyOfRange(5, first.size) + second)

        assertEquals(listOf(0x0101, 0x0701), packets.map { it.command })
        assertContentEquals(byteArrayOf(0, 0, 0, 9), packets[1].body)
        assertEquals(0, stream.bufferedByteCount)
    }

    @Test
    fun binaryReaderWriterRoundTripAndTruncation() {
        val writer = BinaryWriter()
        writer.writeUInt8(0xFE)
        writer.writeUInt16(0x1234)
        writer.writeInt16(-2)
        writer.writeUInt32(0x89AB_CDEFu)
        writer.writeStringUInt16("Ring ü")

        val reader = BinaryReader(writer.data)
        assertEquals(0xFE, reader.readUInt8())
        assertEquals(0x1234, reader.readUInt16())
        assertEquals(-2, reader.readInt16())
        assertEquals(0x89AB_CDEFu, reader.readUInt32())
        assertEquals("Ring ü", reader.readStringUInt16())
        assertEquals(0, reader.remaining)
        assertThrows<RingSoundError.Protocol> { reader.readUInt8() }
    }
}
