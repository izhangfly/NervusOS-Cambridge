package com.nervus.zring.sdk

import kotlin.time.Duration

/**
 * Top-level SDK entry points, mirroring Python `scan_rings()` / `connect_ring()`.
 */

/** Scan for nearby BLE ring devices, optionally filtered by MAC [address]. */
suspend fun scanRings(
    bleDiscovery: BleDiscovery,
    address: String? = null,
    timeout: Duration = RingSoundDefaults.SCAN_TIMEOUT
): List<BLEDeviceInfo> = NusClient.discover(
    bleDiscovery = bleDiscovery,
    address = address,
    timeout = timeout
)

/**
 * Create and connect a [RingSoundClient] over the given [ble] transport,
 * optionally enabling automatic time-sync.
 */
suspend fun connectRing(
    ble: BleOperations,
    address: String? = null,
    commandTimeout: Duration = RingSoundDefaults.COMMAND_TIMEOUT,
    autoTimeSync: Boolean = false,
    discovery: BleDiscovery? = null,
    writeWithResponse: Boolean = false
): RingSoundClient {
    val transport = NusClient(
        ble = ble,
        discovery = discovery,
        address = address,
        writeWithResponse = writeWithResponse
    )
    val client = RingSoundClient(
        transport = transport,
        commandTimeout = commandTimeout
    )
    client.connect()
    if (autoTimeSync) {
        client.enableTimeSync()
    }
    return client
}

/**
 * Create and connect a [RingSoundClient] using a custom [transport],
 * optionally enabling automatic time-sync.
 */
suspend fun connectRing(
    transport: RingSoundTransport,
    commandTimeout: Duration = RingSoundDefaults.COMMAND_TIMEOUT,
    autoTimeSync: Boolean = false
): RingSoundClient {
    val client = RingSoundClient(
        transport = transport,
        commandTimeout = commandTimeout
    )
    client.connect()
    if (autoTimeSync) {
        client.enableTimeSync()
    }
    return client
}

// MARK: - PCM config utilities, mirroring Python normalize_pcm_config / format_pcm_config

/** Returns a [PCMConfig], using defaults for any null parameter. */
fun normalizePcmConfig(
    sampleRate: Int? = null,
    channels: Int? = null,
    bitDepth: Int? = null
): PCMConfig = PCMConfig(
    sampleRate = sampleRate ?: RingSoundDefaults.SAMPLE_RATE,
    channels = channels ?: RingSoundDefaults.CHANNELS,
    bitDepth = bitDepth ?: RingSoundDefaults.BIT_DEPTH
)

/** Formats a [PCMConfig] as a human-readable string, e.g. `"16000Hz / 1ch / 16bit"`. */
fun formatPcmConfig(config: PCMConfig = PCMConfig()): String = config.toString()
