package com.nervus.zring.sdk

/** v4 protocol packet codec: CRC16, encode/decode and stream reassembly. */
object RingSoundProtocol {

    /**
     * CRC-16/CCITT-FALSE (poly 0x1021, init 0xFFFF), matching the device's
     * `crc16_compute()` nibble-based implementation.
     */
    fun crc16(data: ByteArray, initial: Int = 0xFFFF): Int {
        var crc = initial and 0xFFFF
        for (byte in data) {
            crc = ((crc ushr 8) or (crc shl 8)) and 0xFFFF
            crc = crc xor (byte.toInt() and 0xFF)
            crc = crc xor ((crc and 0xFF) ushr 4)
            crc = crc xor (((crc shl 8) shl 4) and 0xFFFF)
            crc = crc xor ((((crc and 0xFF) shl 4) shl 1) and 0xFFFF)
        }
        return crc
    }

    fun encodePacket(command: Int, body: ByteArray = ByteArray(0)): ByteArray {
        val bodyCRC = if (body.isEmpty()) 0 else crc16(body)
        val writer = BinaryWriter()
        writer.writeUInt8(RingSoundPacketConstants.MAGIC)
        writer.writeUInt16(RingSoundPacketConstants.PROTOCOL_VERSION)
        writer.writeUInt16(command)
        writer.writeUInt32(body.size.toUInt())
        writer.writeUInt16(bodyCRC)
        writer.write(body)
        return writer.data
    }

    fun peekBodyLength(data: ByteArray): Int {
        if (data.size < RingSoundPacketConstants.HEADER_SIZE) {
            throw RingSoundError.Protocol("Not enough bytes to read packet header")
        }
        val reader = BinaryReader(data)
        reader.readUInt8()
        reader.readUInt16()
        reader.readUInt16()
        return reader.readUInt32().toInt()
    }

    fun decodePacket(data: ByteArray): Packet {
        if (data.size < RingSoundPacketConstants.HEADER_SIZE) {
            throw RingSoundError.Protocol("Packet too short: ${data.size} bytes")
        }

        val reader = BinaryReader(data)
        val magic = reader.readUInt8()
        val version = reader.readUInt16()
        val command = reader.readUInt16()
        val bodyLength = reader.readUInt32().toLong()
        val bodyCRC = reader.readUInt16()

        if (magic != RingSoundPacketConstants.MAGIC) {
            throw RingSoundError.Protocol("Invalid packet magic: 0x%02X".format(magic))
        }
        if (version > RingSoundPacketConstants.PROTOCOL_VERSION) {
            throw RingSoundError.Protocol("Unsupported protocol version: $version")
        }
        if (bodyLength > RingSoundPacketConstants.MAXIMUM_BODY_LENGTH) {
            throw RingSoundError.Protocol("Body too large: $bodyLength bytes")
        }
        if (reader.remaining < bodyLength) {
            throw RingSoundError.Protocol(
                "Incomplete packet: need ${RingSoundPacketConstants.HEADER_SIZE + bodyLength}, got ${data.size}"
            )
        }

        val body = reader.read(bodyLength.toInt())
        if (body.isNotEmpty()) {
            val actualCRC = crc16(body)
            if (actualCRC != bodyCRC) {
                throw RingSoundError.Protocol(
                    "Body CRC mismatch: expected 0x%04X, got 0x%04X".format(bodyCRC, actualCRC)
                )
            }
        }

        return Packet(command = command, body = body, version = version, bodyCRC = bodyCRC)
    }
}

/**
 * Incrementally reassembles protocol packets from arbitrary BLE chunks.
 * Skips noise before the magic byte and resynchronizes on the stream.
 */
class PacketStream {
    private var buffer = ByteArray(0)

    val bufferedByteCount: Int
        get() = buffer.size

    fun clear() {
        buffer = ByteArray(0)
    }

    fun feed(data: ByteArray): List<Packet> {
        buffer += data
        val packets = mutableListOf<Packet>()

        while (true) {
            if (buffer.isEmpty()) return packets

            val magicIndex = buffer.indexOfFirst { it.toInt() and 0xFF == RingSoundPacketConstants.MAGIC }
            if (magicIndex < 0) {
                buffer = ByteArray(0)
                return packets
            }
            if (magicIndex > 0) {
                buffer = buffer.copyOfRange(magicIndex, buffer.size)
            }

            if (buffer.size < RingSoundPacketConstants.HEADER_SIZE) return packets

            val bodyLength = ((buffer[5].toInt() and 0xFF) shl 24) or
                ((buffer[6].toInt() and 0xFF) shl 16) or
                ((buffer[7].toInt() and 0xFF) shl 8) or
                (buffer[8].toInt() and 0xFF)
            if (bodyLength > RingSoundPacketConstants.MAXIMUM_BODY_LENGTH) {
                clear()
                throw RingSoundError.Protocol("Body too large: $bodyLength bytes")
            }

            val packetLength = RingSoundPacketConstants.HEADER_SIZE + bodyLength
            if (buffer.size < packetLength) return packets

            val bytes = buffer.copyOf(packetLength)
            buffer = buffer.copyOfRange(packetLength, buffer.size)
            packets.add(RingSoundProtocol.decodePacket(bytes))
        }
    }
}
