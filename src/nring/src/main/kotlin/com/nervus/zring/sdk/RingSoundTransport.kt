package com.nervus.zring.sdk

import java.util.UUID

/**
 * BLE transport abstraction (Nordic UART Service).
 *
 * The SDK is platform independent: on Android, implement this interface with
 * `BluetoothGatt`; in tests, use a mock. The Swift SDK ships a CoreBluetooth
 * implementation (`NusClient`).
 */
interface RingSoundTransport {
    val isConnected: Boolean

    fun setReceiveHandler(handler: ((ByteArray) -> Unit)?)
    fun setDisconnectHandler(handler: (() -> Unit)?)
    suspend fun connect()
    suspend fun disconnect()
    suspend fun write(data: ByteArray)
}

data class PacketHandlerToken(
    val command: Int,
    val id: UUID = UUID.randomUUID()
)

typealias AudioProgressHandler = (receivedBytes: Int, totalBytes: Int) -> Unit

/** Splits outgoing NUS writes into fixed 20-byte chunks, matching the device contract. */
internal object NUSWriteStrategy {
    fun chunks(data: ByteArray): List<ByteArray> {
        if (data.isEmpty()) return emptyList()
        val chunkSize = RingSoundDefaults.NUS_WRITE_CHUNK_SIZE
        val result = ArrayList<ByteArray>((data.size + chunkSize - 1) / chunkSize)
        var offset = 0
        while (offset < data.size) {
            result.add(data.copyOfRange(offset, minOf(offset + chunkSize, data.size)))
            offset += chunkSize
        }
        return result
    }
}
