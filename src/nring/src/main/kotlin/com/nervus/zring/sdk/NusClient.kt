package com.nervus.zring.sdk

import java.util.concurrent.locks.ReentrantLock
import kotlin.concurrent.withLock
import kotlin.time.Duration

/**
 * Nordic UART Service (NUS) BLE transport, mirroring the Python `NusClient`.
 *
 * Uses [BleOperations] for the platform BLE link and [BleDiscovery] for
 * scanning. Writes are split into fixed 20-byte chunks per the device contract.
 */
class NusClient(
    private val ble: BleOperations,
    private val discovery: BleDiscovery? = null,
    val address: String? = null,
    val serviceUuid: String = RingSoundUUID.SERVICE,
    val transmitUuid: String = RingSoundUUID.TRANSMIT,
    val receiveUuid: String = RingSoundUUID.RECEIVE,
    val scanTimeout: Duration = RingSoundDefaults.SCAN_TIMEOUT,
    val writeWithResponse: Boolean = false
) : RingSoundTransport {

    private val lock = ReentrantLock()
    private var receiveHandler: ((ByteArray) -> Unit)? = null
    private var disconnectHandler: (() -> Unit)? = null
    @Volatile
    private var notifyStarted = false

    override val isConnected: Boolean
        get() = ble.isConnected

    override fun setReceiveHandler(handler: ((ByteArray) -> Unit)?) {
        lock.withLock { receiveHandler = handler }
    }

    override fun setDisconnectHandler(handler: (() -> Unit)?) {
        lock.withLock { disconnectHandler = handler }
    }

    override suspend fun connect() {
        val targetAddress = address
            ?: throw RingSoundError.Transport(
                "BLE address is required; pass address=\"F1:C1:8A:35:40:FB\""
            )

        // Set up the disconnect bridge before connecting.
        ble.setDisconnectCallback {
            notifyStarted = false
            val handler = lock.withLock { disconnectHandler }
            handler?.invoke()
        }

        try {
            ble.connect(targetAddress)
            ble.startNotify(serviceUuid, transmitUuid) { data ->
                val handler = lock.withLock { receiveHandler }
                handler?.invoke(data)
            }
            notifyStarted = true
        } catch (error: RingSoundError) {
            throw error
        } catch (error: Throwable) {
            throw RingSoundError.Transport(
                "BLE connect failed for address=$targetAddress. " +
                    "The device was not found during scanning, then direct address connect also failed."
            )
        }
    }

    override suspend fun disconnect() {
        try {
            if (notifyStarted) {
                ble.stopNotify(serviceUuid, transmitUuid)
            }
        } finally {
            notifyStarted = false
            if (ble.isConnected) {
                ble.disconnect()
            }
        }
    }

    override suspend fun write(data: ByteArray) {
        if (!ble.isConnected) {
            throw RingSoundError.Transport("BLE client is not connected")
        }
        val chunks = NUSWriteStrategy.chunks(data)
        for (chunk in chunks) {
            ble.write(serviceUuid, receiveUuid, chunk, writeWithResponse)
        }
    }

    companion object {
        /**
         * Scan for nearby BLE devices, optionally filtered by [address].
         * Mirrors Python `NusClient.discover()`.
         */
        suspend fun discover(
            bleDiscovery: BleDiscovery,
            address: String? = null,
            timeout: Duration = RingSoundDefaults.SCAN_TIMEOUT
        ): List<BLEDeviceInfo> {
            val devices = bleDiscovery.discover(RingSoundUUID.SERVICE, timeout)
            return if (address != null) {
                devices.filter { it.address.equals(address, ignoreCase = true) }
            } else {
                devices
            }
        }
    }
}
