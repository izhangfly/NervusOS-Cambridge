package com.nervus.zring.sdk

import java.io.File

data class BLEDeviceInfo(
    val name: String?,
    /** On Android this is the hardware MAC address; on Apple platforms a peripheral UUID. */
    val address: String,
    val rssi: Int? = null
)

/**
 * A decoded protocol packet.
 *
 * Note: [body] is a [ByteArray]; structural equality uses [ByteArray.contentEquals]
 * semantics only via [equals] implemented below.
 */
data class Packet(
    val command: Int,
    val body: ByteArray,
    val version: Int = RingSoundPacketConstants.PROTOCOL_VERSION,
    val bodyCRC: Int = 0
) {
    override fun equals(other: Any?): Boolean {
        if (this === other) return true
        if (other !is Packet) return false
        return command == other.command &&
            body.contentEquals(other.body) &&
            version == other.version &&
            bodyCRC == other.bodyCRC
    }

    override fun hashCode(): Int {
        var result = command
        result = 31 * result + body.contentHashCode()
        result = 31 * result + version
        result = 31 * result + bodyCRC
        return result
    }
}

data class SystemInfo(
    val firmwareVersion: String,
    val systemTime: UInt,
    val audioStorageTotal: UInt,
    val audioStorageAvailable: UInt,
    val batteryPercent: Int,
    val batteryCharging: Boolean,
    val serialNumber: String,
    val cpuID: String,
    val model: String
)

data class LogStorageInfo(
    val pageSize: UInt,
    val totalLength: UInt
)

data class AudioFileInfo(
    val fileIndex: UInt,
    val recordTime: UInt,
    val dataSize: UInt
)

data class AudioDataFrame(
    val fileIndex: UInt,
    val frameOffset: UInt,
    val frameSize: UInt,
    val isEnd: Boolean,
    val data: ByteArray
) {
    override fun equals(other: Any?): Boolean {
        if (this === other) return true
        if (other !is AudioDataFrame) return false
        return fileIndex == other.fileIndex &&
            frameOffset == other.frameOffset &&
            frameSize == other.frameSize &&
            isEnd == other.isEnd &&
            data.contentEquals(other.data)
    }

    override fun hashCode(): Int {
        var result = fileIndex.hashCode()
        result = 31 * result + frameOffset.hashCode()
        result = 31 * result + frameSize.hashCode()
        result = 31 * result + isEnd.hashCode()
        result = 31 * result + data.contentHashCode()
        return result
    }
}

class PCMConfig(
    sampleRate: Int = RingSoundDefaults.SAMPLE_RATE,
    channels: Int = RingSoundDefaults.CHANNELS,
    bitDepth: Int = RingSoundDefaults.BIT_DEPTH
) {
    val sampleRate: Int = maxOf(1, sampleRate)
    val channels: Int = if (channels > 1) 2 else 1
    val bitDepth: Int = if (bitDepth == 8) 8 else 16

    override fun equals(other: Any?): Boolean {
        if (this === other) return true
        if (other !is PCMConfig) return false
        return sampleRate == other.sampleRate &&
            channels == other.channels &&
            bitDepth == other.bitDepth
    }

    override fun hashCode(): Int {
        var result = sampleRate
        result = 31 * result + channels
        result = 31 * result + bitDepth
        return result
    }

    override fun toString(): String = "${sampleRate}Hz / ${channels}ch / ${bitDepth}bit"
}

data class SpeexDecodeOptions(
    val pcmConfig: PCMConfig,
    val quality: Int,
    val bitsSize: Int?,
    val allowFramedBlocks: Boolean,
    val ffmpegPath: String
)

data class SpeexDecodeResult(
    val pcmData: ByteArray,
    val pcmConfig: PCMConfig,
    val sourceType: String = "speex",
    val sourceExtension: String = "spx",
    val packetCount: Int = 0
) {
    override fun equals(other: Any?): Boolean {
        if (this === other) return true
        if (other !is SpeexDecodeResult) return false
        return pcmData.contentEquals(other.pcmData) &&
            pcmConfig == other.pcmConfig &&
            sourceType == other.sourceType &&
            sourceExtension == other.sourceExtension &&
            packetCount == other.packetCount
    }

    override fun hashCode(): Int {
        var result = pcmData.contentHashCode()
        result = 31 * result + pcmConfig.hashCode()
        result = 31 * result + sourceType.hashCode()
        result = 31 * result + sourceExtension.hashCode()
        result = 31 * result + packetCount
        return result
    }
}

/** Pluggable Speex decoder; receives the source bytes and decode options. */
typealias SpeexDecoder = (data: ByteArray, options: SpeexDecodeOptions) -> SpeexDecodeResult

data class PlayableAudio(
    val data: ByteArray,
    val fileExtension: String,
    val mimeType: String,
    val playMode: String,
    val label: String,
    val pcmConfig: PCMConfig? = null,
    val sourceType: String = "raw",
    val sourceExtension: String = "bin",
    val sourceMIMEType: String = "application/octet-stream",
    val description: String = ""
) {
    override fun equals(other: Any?): Boolean {
        if (this === other) return true
        if (other !is PlayableAudio) return false
        return data.contentEquals(other.data) &&
            fileExtension == other.fileExtension &&
            mimeType == other.mimeType &&
            playMode == other.playMode &&
            label == other.label &&
            pcmConfig == other.pcmConfig &&
            sourceType == other.sourceType &&
            sourceExtension == other.sourceExtension &&
            sourceMIMEType == other.sourceMIMEType &&
            description == other.description
    }

    override fun hashCode(): Int {
        var result = data.contentHashCode()
        result = 31 * result + fileExtension.hashCode()
        result = 31 * result + mimeType.hashCode()
        result = 31 * result + playMode.hashCode()
        result = 31 * result + label.hashCode()
        return result
    }
}

data class AudioBundle(
    val rawFile: File,
    val rawFileName: String,
    val playableFile: File,
    val playableFileName: String,
    val playMode: String,
    val formatLabel: String,
    val playDescription: String,
    val pcmSummary: String,
    val rawSize: Int,
    val playableSize: Int,
    val sourceType: String,
    val sourceExtension: String
)

data class SensorStartInfo(
    val sampleRateHz: Int,
    val accelerationRangeG: Int,
    val gyroscopeRangeDPS: Int
)

class SensorStopInfo {
    override fun equals(other: Any?): Boolean = other is SensorStopInfo
    override fun hashCode(): Int = 0
    override fun toString(): String = "SensorStopInfo()"
}

data class SensorDataSample(
    val timestampMilliseconds: UInt,
    val accelerationX: Short,
    val accelerationY: Short,
    val accelerationZ: Short,
    val gyroscopeX: Short,
    val gyroscopeY: Short,
    val gyroscopeZ: Short
)

data class SensorDataBatch(
    val sequenceStart: UInt,
    val frameCount: Int,
    val sampleSize: Int,
    val samples: List<SensorDataSample>
)

data class SensorDoubleTapEvent(
    val timestampMilliseconds: UInt
)

data class SensorGestureEvent(
    val timestampMilliseconds: UInt,
    val gestureID: Int
)

data class SensorKeyDoublePressEvent(
    val timestampMilliseconds: UInt
)

data class SensorKeySinglePressEvent(
    val timestampMilliseconds: UInt
)

data class DownloadedAudioFile(
    val info: AudioFileInfo,
    val data: ByteArray
) {
    override fun equals(other: Any?): Boolean {
        if (this === other) return true
        if (other !is DownloadedAudioFile) return false
        return info == other.info && data.contentEquals(other.data)
    }

    override fun hashCode(): Int = 31 * info.hashCode() + data.contentHashCode()
}

data class AutoReceivedAudioFile(
    val fileIndex: UInt,
    val data: ByteArray
) {
    override fun equals(other: Any?): Boolean {
        if (this === other) return true
        if (other !is AutoReceivedAudioFile) return false
        return fileIndex == other.fileIndex && data.contentEquals(other.data)
    }

    override fun hashCode(): Int = 31 * fileIndex.hashCode() + data.contentHashCode()
}
