package com.nervus.zring.sdk

import kotlin.time.Duration
import kotlin.time.Duration.Companion.seconds

object RingSoundSDK {
    const val VERSION = "0.4.1"
}

object RingSoundUUID {
    const val SERVICE = "6E400001-B5A3-F393-E0A9-E50E24DCCA9E"

    /** TX characteristic: ring notifies the SDK. */
    const val TRANSMIT = "6E400003-B5A3-F393-E0A9-E50E24DCCA9E"

    /** RX characteristic: SDK writes to the ring. */
    const val RECEIVE = "6E400002-B5A3-F393-E0A9-E50E24DCCA9E"
}

object RingSoundDefaults {
    val SCAN_TIMEOUT: Duration = 25.seconds
    val COMMAND_TIMEOUT: Duration = 10.seconds
    const val NUS_WRITE_CHUNK_SIZE = 20
    const val SAMPLE_RATE = 16_000
    const val CHANNELS = 1
    const val BIT_DEPTH = 16
    const val SPEEX_FRAME_SIZE = 320
    const val SPEEX_MAXIMUM_PACKET_SIZE = 540
    const val SPEEX_QUALITY = 3
    const val LEGACY_OUTER_FRAME_SIZE = 1_026
}

object RingSoundPacketConstants {
    const val MAGIC: Int = 0x3F
    const val PROTOCOL_VERSION: Int = 4
    const val HEADER_SIZE = 11
    const val MAXIMUM_BODY_LENGTH = 5_120
}

/** Common interface implemented by all command enums so APIs can accept them type-safely. */
interface RingCommand {
    val value: Int
}

enum class ErrorCode(val value: Int, val message: String) {
    SUCCESS(0, "success"),
    UNKNOWN(1, "unknown error"),
    DEVICE_BUSY(2, "device busy"),
    FILE_NOT_EXIST(3, "file not exist"),
    COMMAND_GROUP_NOT_EXIST(4, "command group not exist"),
    COMMAND_NOT_EXIST(5, "command not exist"),
    TIMEOUT(6, "operation timeout"),
    INVALID_PARAMETER(7, "invalid parameter"),
    COMMUNICATION(8, "communication error");

    companion object {
        fun fromValue(value: Int): ErrorCode? = entries.firstOrNull { it.value == value }
    }
}

enum class SystemCommand(override val value: Int) : RingCommand {
    GET_INFO(0x0101),
    INFO_RESPONSE(0x0102)
}

enum class LogCommand(override val value: Int) : RingCommand {
    GET_STORAGE(0x0301),
    STORAGE_RESPONSE(0x0302),
    GET_LOG(0x0303),
    LOG_RESPONSE(0x0304)
}

enum class TimeCommand(override val value: Int) : RingCommand {
    REQUEST(0x0401),
    RESPONSE(0x0402)
}

enum class AudioCommand(override val value: Int) : RingCommand {
    GET_LIST(0x0501),
    LIST_RESPONSE(0x0502),
    START_EXTRACT(0x0503),
    FILE_INFO_RESPONSE(0x0504),
    DATA_FRAME(0x0505),
    NEXT_FRAME(0x0506),
    END_EXTRACT(0x0507),
    EXTRACT_DONE(0x0508),
    START_EXTRACT_QUICK(0x0509),
    CLEAR_ALL(0x050B),
    CLEAR_ALL_RESPONSE(0x050C)
}

enum class SensorCommand(override val value: Int) : RingCommand {
    START_REPORT(0x0601),
    START_REPORT_RESPONSE(0x0602),
    STOP_REPORT(0x0603),
    STOP_REPORT_RESPONSE(0x0604),
    DATA_FRAME(0x0605),
    DOUBLE_TAP(0x0701),
    GESTURE(0x0702),
    KEY_DOUBLE_PRESS(0x0703),
    KEY_SINGLE_PRESS(0x0704)
}

enum class SensorGestureID(val value: Int) {
    IDLE(0),
    ROTATE_BACK(1),
    ROTATE_FRONT(2),
    WAVE(3);

    companion object {
        fun fromValue(value: Int): SensorGestureID? = entries.firstOrNull { it.value == value }
    }
}
