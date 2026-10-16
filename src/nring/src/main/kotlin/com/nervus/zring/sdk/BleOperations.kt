package com.nervus.zring.sdk

import kotlin.time.Duration

/**
 * Platform BLE GATT operations. Implement this with Android `BluetoothGatt`,
 * desktop BLE library, or a test double.
 */
interface BleOperations {
    val isConnected: Boolean

    /** Connect to the device with the given [address] (MAC or platform UUID). */
    suspend fun connect(address: String)

    /** Disconnect from the current device. */
    suspend fun disconnect()

    /** Start notifications on [characteristicUuid]; [callback] receives raw bytes. */
    suspend fun startNotify(
        serviceUuid: String,
        characteristicUuid: String,
        callback: (ByteArray) -> Unit
    )

    /** Stop notifications on [characteristicUuid]. */
    suspend fun stopNotify(serviceUuid: String, characteristicUuid: String)

    /** Write [data] to [characteristicUuid]. */
    suspend fun write(
        serviceUuid: String,
        characteristicUuid: String,
        data: ByteArray,
        withResponse: Boolean
    )

    /** Register a callback invoked when the BLE link drops. */
    fun setDisconnectCallback(callback: (() -> Unit)?)
}

/**
 * BLE device scanner. Implement with Android `BluetoothLeScanner` or equivalent.
 */
interface BleDiscovery {
    /**
     * Scan for BLE devices advertising [serviceUuid] (null = scan all).
     * Returns all devices found within [timeout].
     */
    suspend fun discover(serviceUuid: String?, timeout: Duration): List<BLEDeviceInfo>

    /**
     * Find a specific device by [address]. Returns null if not found.
     * Default implementation scans and filters by address.
     */
    suspend fun findByAddress(address: String, timeout: Duration): BLEDeviceInfo? {
        val devices = discover(null, timeout)
        return devices.firstOrNull { it.address.equals(address, ignoreCase = true) }
    }
}
