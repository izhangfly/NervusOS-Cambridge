package com.nervus.zring.sdk

import kotlin.time.Duration.Companion.seconds
import kotlinx.coroutines.runBlocking
import org.junit.jupiter.api.Test
import kotlin.test.assertEquals
import kotlin.test.assertFailsWith
import kotlin.test.assertFalse
import kotlin.test.assertTrue

class NusClientTests {

    @Test
    fun connectRequiresAddress() = runBlocking {
        val ble = MockBleOperations()
        val client = NusClient(ble = ble, address = null)

        val error = assertFailsWith<RingSoundError.Transport> {
            client.connect()
        }
        assertTrue(error.message.orEmpty().contains("address is required"))
    }

    @Test
    fun connectStartsNotifyOnTxCharacteristic() = runBlocking {
        val ble = MockBleOperations()
        val client = NusClient(ble = ble, address = "AA:BB:CC:DD:EE:FF")
        client.connect()

        assertTrue(client.isConnected)
        assertTrue(ble.isConnected)
    }

    @Test
    fun writeSplitsIntoTwentyByteChunks() = runBlocking {
        val ble = MockBleOperations()
        val client = NusClient(ble = ble, address = "AA:BB:CC:DD:EE:FF")
        client.connect()

        val data = ByteArray(41) { it.toByte() }
        client.write(data)

        assertEquals(listOf(20, 20, 1), ble.writes.map { it.size })
        // Reassembled data matches original.
        val reassembled = ble.writes.fold(ByteArray(0)) { acc, chunk -> acc + chunk }
        assertEquals(data.toList(), reassembled.toList())
    }

    @Test
    fun writeEmptyDataDoesNothing() = runBlocking {
        val ble = MockBleOperations()
        val client = NusClient(ble = ble, address = "AA:BB:CC:DD:EE:FF")
        client.connect()

        client.write(ByteArray(0))

        assertTrue(ble.writes.isEmpty())
    }

    @Test
    fun writeFailsWhenDisconnected() = runBlocking {
        val ble = MockBleOperations()
        val client = NusClient(ble = ble, address = "AA:BB:CC:DD:EE:FF")

        assertFailsWith<RingSoundError.Transport> {
            client.write(byteArrayOf(1, 2, 3))
        }
    }

    @Test
    fun receiveHandlerForwardsNotifyData() = runBlocking {
        val ble = MockBleOperations()
        val client = NusClient(ble = ble, address = "AA:BB:CC:DD:EE:FF")
        client.connect()

        var received: ByteArray? = null
        client.setReceiveHandler { received = it }

        ble.pushNotify(byteArrayOf(0x3F, 0x00, 0x04))

        assertEquals(listOf(0x3F.toByte(), 0x00.toByte(), 0x04.toByte()), received?.toList())
    }

    @Test
    fun disconnectHandlerInvokedOnBleDrop() = runBlocking {
        val ble = MockBleOperations()
        val client = NusClient(ble = ble, address = "AA:BB:CC:DD:EE:FF")
        client.connect()

        var disconnected = false
        client.setDisconnectHandler { disconnected = true }

        ble.dropConnection()

        assertTrue(disconnected)
        assertFalse(client.isConnected)
    }

    @Test
    fun disconnectStopsNotifyAndDisconnects() = runBlocking {
        val ble = MockBleOperations()
        val client = NusClient(ble = ble, address = "AA:BB:CC:DD:EE:FF")
        client.connect()

        client.disconnect()

        assertFalse(ble.isConnected)
    }

    @Test
    fun discoverFiltersDevicesByAddress() = runBlocking {
        val devices = listOf(
            BLEDeviceInfo(name = "Ring1", address = "AA:BB:CC:DD:EE:FF", rssi = -50),
            BLEDeviceInfo(name = "Ring2", address = "11:22:33:44:55:66", rssi = -60)
        )
        val discovery = MockBleDiscovery(devices)

        val all = NusClient.discover(discovery)
        assertEquals(2, all.size)

        val filtered = NusClient.discover(discovery, address = "AA:BB:CC:DD:EE:FF")
        assertEquals(1, filtered.size)
        assertEquals("Ring1", filtered[0].name)
    }

    @Test
    fun scanRingsDelegatesToNusClientDiscover() = runBlocking {
        val devices = listOf(
            BLEDeviceInfo(name = "Ring", address = "AA:BB:CC:DD:EE:FF")
        )
        val discovery = MockBleDiscovery(devices)

        val result = scanRings(discovery)

        assertEquals(1, result.size)
        assertEquals("Ring", result[0].name)
    }

    @Test
    fun connectRingCreatesAndConnectsClient() = runBlocking {
        val ble = MockBleOperations()
        val client = connectRing(ble = ble, address = "AA:BB:CC:DD:EE:FF")

        assertTrue(client.isConnected)
    }

    @Test
    fun connectRingWithTransportCreatesAndConnectsClient() = runBlocking {
        val transport = MockTransport()
        val client = connectRing(transport = transport)

        assertTrue(client.isConnected)
    }
}
