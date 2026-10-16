package com.nervus.zring.sdk

import java.util.concurrent.locks.ReentrantLock
import kotlin.concurrent.withLock
import kotlin.time.Duration

/** Parses an even-length hex string into bytes. */
fun String.hexToByteArray(): ByteArray {
    require(length % 2 == 0) { "Hex string must have an even length" }
    return ByteArray(length / 2) { i ->
        substring(i * 2, i * 2 + 2).toInt(16).toByte()
    }
}

val ByteArray.hexString: String
    get() = joinToString("") { "%02x".format(it) }

class MockTransport : RingSoundTransport {
    private val lock = ReentrantLock()
    private var receiveHandler: ((ByteArray) -> Unit)? = null
    private var disconnectHandler: (() -> Unit)? = null
    private var connected = false
    private var capturedWrites = mutableListOf<ByteArray>()

    var onWrite: (suspend (ByteArray) -> Unit)? = null

    override val isConnected: Boolean
        get() = lock.withLock { connected }

    val writes: List<ByteArray>
        get() = lock.withLock { capturedWrites.toList() }

    override fun setReceiveHandler(handler: ((ByteArray) -> Unit)?) {
        lock.withLock { receiveHandler = handler }
    }

    override fun setDisconnectHandler(handler: (() -> Unit)?) {
        lock.withLock { disconnectHandler = handler }
    }

    override suspend fun connect() {
        lock.withLock { connected = true }
    }

    override suspend fun disconnect() {
        val callback = lock.withLock {
            connected = false
            disconnectHandler
        }
        callback?.invoke()
    }

    override suspend fun write(data: ByteArray) {
        lock.withLock { capturedWrites.add(data) }
        onWrite?.invoke(data)
    }

    fun push(data: ByteArray) {
        val callback = lock.withLock { receiveHandler }
        callback?.invoke(data)
    }

    fun dropConnection() {
        val callback = lock.withLock {
            connected = false
            disconnectHandler
        }
        callback?.invoke()
    }
}

class MockBleOperations : BleOperations {
    private val lock = ReentrantLock()
    private var connected = false
    private var notifyCallback: ((ByteArray) -> Unit)? = null
    private var disconnectCallback: (() -> Unit)? = null
    private val writtenChunks = mutableListOf<ByteArray>()

    override val isConnected: Boolean
        get() = lock.withLock { connected }

    val writes: List<ByteArray>
        get() = lock.withLock { writtenChunks.toList() }

    override suspend fun connect(address: String) {
        lock.withLock { connected = true }
    }

    override suspend fun disconnect() {
        lock.withLock { connected = false }
    }

    override suspend fun startNotify(
        serviceUuid: String,
        characteristicUuid: String,
        callback: (ByteArray) -> Unit
    ) {
        lock.withLock { notifyCallback = callback }
    }

    override suspend fun stopNotify(serviceUuid: String, characteristicUuid: String) {
        lock.withLock { notifyCallback = null }
    }

    override suspend fun write(
        serviceUuid: String,
        characteristicUuid: String,
        data: ByteArray,
        withResponse: Boolean
    ) {
        lock.withLock { writtenChunks.add(data) }
    }

    override fun setDisconnectCallback(callback: (() -> Unit)?) {
        lock.withLock { disconnectCallback = callback }
    }

    fun pushNotify(data: ByteArray) {
        val cb = lock.withLock { notifyCallback }
        cb?.invoke(data)
    }

    fun dropConnection() {
        lock.withLock { connected = false }
        val cb = lock.withLock { disconnectCallback }
        cb?.invoke()
    }
}

class MockBleDiscovery(
    private val devices: List<BLEDeviceInfo> = emptyList()
) : BleDiscovery {
    override suspend fun discover(serviceUuid: String?, timeout: Duration): List<BLEDeviceInfo> =
        devices
}
