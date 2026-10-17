package com.nervus.piperring

import com.nervus.zring.sdk.RingSoundDefaults
import com.nervus.zring.sdk.RingSoundTransport
import com.nervus.zring.sdk.RingSoundUUID
import java.util.logging.Logger

/**
 * BlueZ (Linux) implementation of nring's [RingSoundTransport] — the ring's Nordic UART Service
 * over the RDK's USB Bluetooth (UGREEN **CM390 / Realtek RTL8761B**, firmware `rtl8761b_fw.bin`).
 *
 * Contract (from nring):
 *  - `connect()`  : scan for the ring advertising service [RingSoundUUID.SERVICE], connect GATT,
 *                   enable notifications on TX char [RingSoundUUID.TRANSMIT] and route each
 *                   notification's bytes to the receive handler.
 *  - `write(data)`: split into ≤20-byte NUS chunks and write each to RX char
 *                   [RingSoundUUID.RECEIVE] (write-without-response).
 *  - `disconnect()`: drop the GATT link and fire the disconnect handler.
 *
 * !! INTEGRATION POINT (needs the CM390 plugged + BlueZ up to test):
 *   Wire the three GATT operations to BlueZ. Recommended: `com.github.hypfvieh:bluez-dbus`
 *   (BlueZ D-Bus GATT, pure-JVM, linux-arm64-friendly) — `DeviceManager` → scan by service UUID →
 *   `BluetoothDevice.connect()` → `BluetoothGattCharacteristic` for TRANSMIT (startNotify + a
 *   value handler) and RECEIVE (writeValue). On the RDK first bring BT up:
 *     sudo rfkill unblock bluetooth ; sudo hciconfig hci0 up ; bluetoothctl (scan on / pair)
 *   Until wired, connect() throws so the app surfaces the state instead of silently no-op'ing.
 */
class BlueZRingTransport(
    private val deviceName: String? = null,     // optional: match the ring by advertised name
) : RingSoundTransport {

    @Volatile private var connectedFlag = false
    private var receiveHandler: ((ByteArray) -> Unit)? = null
    private var disconnectHandler: (() -> Unit)? = null

    override val isConnected: Boolean get() = connectedFlag

    override fun setReceiveHandler(handler: ((ByteArray) -> Unit)?) { receiveHandler = handler }
    override fun setDisconnectHandler(handler: (() -> Unit)?) { disconnectHandler = handler }

    override suspend fun connect() {
        logger.info(
            "BlueZ connect: scan service ${RingSoundUUID.SERVICE}, notify ${RingSoundUUID.TRANSMIT}, " +
                "write ${RingSoundUUID.RECEIVE}; target=${deviceName ?: "<first ring>"}"
        )
        // TODO(GATT): scan → connect → startNotify(TRANSMIT){ bytes -> onNotify(bytes) }.
        throw NotImplementedError(
            "BlueZRingTransport GATT not wired yet — plug the CM390 and complete with bluez-dbus " +
                "(see class doc). onNotify()/chunking below are ready for it."
        )
    }

    override suspend fun disconnect() {
        connectedFlag = false
        disconnectHandler?.invoke()
    }

    override suspend fun write(data: ByteArray) {
        // Ready for GATT: NUS requires ≤20-byte chunks to the RECEIVE characteristic.
        for (chunk in chunk20(data)) {
            // TODO(GATT): receiveChar.writeValue(chunk, writeWithoutResponse)
        }
    }

    /** Called by the GATT notify callback once wired; delivers ring bytes to nring. */
    fun onNotify(bytes: ByteArray) { receiveHandler?.invoke(bytes) }

    private fun chunk20(data: ByteArray): List<ByteArray> {
        if (data.isEmpty()) return emptyList()
        val n = RingSoundDefaults.NUS_WRITE_CHUNK_SIZE
        val out = ArrayList<ByteArray>((data.size + n - 1) / n)
        var i = 0
        while (i < data.size) { out.add(data.copyOfRange(i, minOf(i + n, data.size))); i += n }
        return out
    }

    private companion object { val logger: Logger = Logger.getLogger("BlueZRingTransport") }
}
