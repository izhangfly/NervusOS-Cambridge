package com.nervus.zring.sdk

/** Big-endian reader over a byte array, mirroring the Swift `BinaryReader`. */
class BinaryReader(private val bytes: ByteArray) {
    var offset: Int = 0
        private set

    constructor(bytes: Collection<Byte>) : this(bytes.toByteArray())

    val remaining: Int
        get() = bytes.size - offset

    private fun require(count: Int) {
        if (count < 0 || remaining < count) {
            throw RingSoundError.Protocol("Need $count bytes, only $remaining left")
        }
    }

    /** Reads an unsigned 8-bit value as an Int in 0..255. */
    fun readUInt8(): Int {
        require(1)
        return bytes[offset++].toInt() and 0xFF
    }

    /** Reads a big-endian unsigned 16-bit value as an Int in 0..65535. */
    fun readUInt16(): Int {
        require(2)
        val value = ((bytes[offset].toInt() and 0xFF) shl 8) or
            (bytes[offset + 1].toInt() and 0xFF)
        offset += 2
        return value
    }

    /** Reads a big-endian signed 16-bit value. */
    fun readInt16(): Short = readUInt16().toShort()

    /** Reads a big-endian unsigned 32-bit value. */
    fun readUInt32(): UInt {
        require(4)
        val value = ((bytes[offset].toUInt() and 0xFFu) shl 24) or
            ((bytes[offset + 1].toUInt() and 0xFFu) shl 16) or
            ((bytes[offset + 2].toUInt() and 0xFFu) shl 8) or
            (bytes[offset + 3].toUInt() and 0xFFu)
        offset += 4
        return value
    }

    fun read(count: Int): ByteArray {
        require(count)
        val value = bytes.copyOfRange(offset, offset + count)
        offset += count
        return value
    }

    /** Reads a UInt16 length-prefixed UTF-8 string. */
    fun readStringUInt16(): String {
        val count = readUInt16()
        return read(count).decodeToString()
    }
}

/** Big-endian writer, mirroring the Swift `BinaryWriter`. */
class BinaryWriter {
    private val buffer = java.io.ByteArrayOutputStream()

    val data: ByteArray
        get() = buffer.toByteArray()

    val size: Int
        get() = buffer.size()

    fun writeUInt8(value: Int) {
        buffer.write(value and 0xFF)
    }

    fun writeUInt16(value: Int) {
        buffer.write(value ushr 8 and 0xFF)
        buffer.write(value and 0xFF)
    }

    fun writeInt16(value: Short) {
        writeUInt16(value.toInt() and 0xFFFF)
    }

    fun writeUInt32(value: UInt) {
        buffer.write((value shr 24).toInt() and 0xFF)
        buffer.write((value shr 16).toInt() and 0xFF)
        buffer.write((value shr 8).toInt() and 0xFF)
        buffer.write(value.toInt() and 0xFF)
    }

    fun write(value: ByteArray) {
        buffer.write(value, 0, value.size)
    }

    fun writeStringUInt16(value: String) {
        val encoded = value.encodeToByteArray()
        require(encoded.size <= 0xFFFF) { "String is too long for a UInt16 length" }
        writeUInt16(encoded.size)
        write(encoded)
    }

    fun writeStringUInt32(value: String) {
        val encoded = value.encodeToByteArray()
        writeUInt32(encoded.size.toUInt())
        write(encoded)
    }
}
