package com.nervus.zring.sdk

import java.io.ByteArrayOutputStream
import java.io.File
import java.io.IOException
import java.nio.file.Files
import java.nio.file.Path
import java.time.Instant
import java.time.ZoneId
import java.time.ZonedDateTime

/** WAV/Ogg/Speex audio format helpers, mirroring the Swift `AudioCodec`. */
object AudioCodec {
    private val bitsByQuality: Map<Int, Int> = mapOf(
        1 to 10,
        2 to 15,
        3 to 20,
        4 to 20,
        5 to 28,
        6 to 28,
        7 to 38,
        8 to 38,
        9 to 46,
        10 to 46
    )

    fun buildWAV(pcm: ByteArray, config: PCMConfig = PCMConfig()): ByteArray {
        val blockAlign = config.channels * config.bitDepth / 8
        val byteRate = config.sampleRate * blockAlign
        val result = ByteArrayOutputStream(44 + pcm.size)
        result.writeASCII("RIFF")
        result.writeLittleEndianUInt32((36 + pcm.size).toUInt())
        result.writeASCII("WAVE")
        result.writeASCII("fmt ")
        result.writeLittleEndianUInt32(16u)
        result.writeLittleEndianUInt16(1)
        result.writeLittleEndianUInt16(config.channels)
        result.writeLittleEndianUInt32(config.sampleRate.toUInt())
        result.writeLittleEndianUInt32(byteRate.toUInt())
        result.writeLittleEndianUInt16(blockAlign)
        result.writeLittleEndianUInt16(config.bitDepth)
        result.writeASCII("data")
        result.writeLittleEndianUInt32(pcm.size.toUInt())
        result.write(pcm, 0, pcm.size)
        return result.toByteArray()
    }

    fun isWAV(data: ByteArray): Boolean {
        if (data.size < 12) return false
        return data.copyOf(4).contentEquals("RIFF".encodeToByteArray()) &&
            data.copyOfRange(8, 12).contentEquals("WAVE".encodeToByteArray())
    }

    fun isOggSpeex(data: ByteArray): Boolean {
        if (data.size < 36 || !data.copyOf(4).contentEquals("OggS".encodeToByteArray())) {
            return false
        }
        val marker = "Speex   ".encodeToByteArray()
        if (data.copyOfRange(28, 36).contentEquals(marker)) return true
        val head = data.copyOf(minOf(128, data.size))
        return head.indexOf(marker) >= 0
    }

    fun speexMode(sampleRate: Int): Int = when {
        sampleRate <= 8_000 -> 0
        sampleRate <= 16_000 -> 1
        else -> 2
    }

    fun frameSize(sampleRate: Int): Int = when {
        sampleRate <= 8_000 -> 160
        sampleRate <= 16_000 -> 320
        else -> 640
    }

    fun bitsSize(
        quality: Int = RingSoundDefaults.SPEEX_QUALITY,
        explicitBitsSize: Int? = null
    ): Int {
        if (explicitBitsSize != null && explicitBitsSize > 0) {
            return explicitBitsSize
        }
        return bitsByQuality[quality] ?: 20
    }

    /**
     * Parses the device's length-prefixed Speex stream
     * (`uint16 little-endian length + payload`), optionally accepting the
     * legacy 1026-byte outer framed blocks.
     */
    fun parsePacketizedSpeex(
        data: ByteArray,
        maxPacketSize: Int = RingSoundDefaults.SPEEX_MAXIMUM_PACKET_SIZE,
        allowFramedBlocks: Boolean = true,
        framedBlockSize: Int = RingSoundDefaults.LEGACY_OUTER_FRAME_SIZE
    ): List<ByteArray>? {
        val maximum = maxOf(1, maxPacketSize)

        fun parseBlock(start: Int, end: Int): List<ByteArray>? {
            val packets = mutableListOf<ByteArray>()
            var offset = start
            while (offset + 2 <= end) {
                val length = (data[offset].toInt() and 0xFF) or
                    ((data[offset + 1].toInt() and 0xFF) shl 8)
                if (length == 0) break
                if (length > maximum || offset + 2 + length > end) return null
                packets.add(data.copyOfRange(offset + 2, offset + 2 + length))
                offset += 2 + length
            }
            if (packets.isEmpty()) return null
            for (i in offset until end) {
                val byte = data[i]
                if (byte != 0.toByte() && byte != 0xFF.toByte()) return null
            }
            return packets
        }

        parseBlock(0, data.size)?.let { return it }
        if (!allowFramedBlocks) return null

        val blockSize = maxOf(1, framedBlockSize)
        if (data.size <= blockSize) return null
        val packets = mutableListOf<ByteArray>()
        var start = 0
        while (start < data.size) {
            val block = parseBlock(start, minOf(data.size, start + blockSize)) ?: return null
            packets.addAll(block)
            start += blockSize
        }
        return packets
    }

    /** Splits raw (prefix-less) Speex into fixed-size packets as a compatibility fallback. */
    fun splitRawSpeexPackets(
        data: ByteArray,
        quality: Int = RingSoundDefaults.SPEEX_QUALITY,
        bitsSize: Int? = null
    ): List<ByteArray>? {
        val size = bitsSize(quality = quality, explicitBitsSize = bitsSize)
        if (data.size < size || data.size % size != 0) return null
        val packets = mutableListOf<ByteArray>()
        var offset = 0
        while (offset < data.size) {
            packets.add(data.copyOfRange(offset, offset + size))
            offset += size
        }
        return packets
    }

    fun buildOggSpeex(
        packets: List<ByteArray>,
        config: PCMConfig = PCMConfig(),
        serial: UInt = 0x564F_5247u
    ): ByteArray {
        val selectedFrameSize = frameSize(config.sampleRate)
        val mode = speexMode(config.sampleRate)

        val speexHeader = ByteArrayOutputStream(80)
        speexHeader.writeASCII("Speex   ")
        val version = "speex-1.2.1".encodeToByteArray()
        speexHeader.write(version, 0, version.size)
        speexHeader.write(ByteArray(20 - version.size), 0, 20 - version.size)
        for (value in intArrayOf(
            1,
            80,
            config.sampleRate,
            mode,
            4,
            1,
            -1,
            selectedFrameSize,
            0,
            1,
            0,
            0,
            0
        )) {
            speexHeader.writeLittleEndianUInt32(value.toUInt())
        }

        val vendor = "ring-sound-python".encodeToByteArray()
        val comments = ByteArrayOutputStream()
        comments.writeLittleEndianUInt32(vendor.size.toUInt())
        comments.write(vendor, 0, vendor.size)
        comments.writeLittleEndianUInt32(0u)

        val result = ByteArrayOutputStream()
        result.write(
            buildOggPage(
                packet = speexHeader.toByteArray(),
                headerType = 2,
                granulePosition = 0u,
                serial = serial,
                sequence = 0u
            )
        )
        result.write(
            buildOggPage(
                packet = comments.toByteArray(),
                headerType = 0,
                granulePosition = 0u,
                serial = serial,
                sequence = 1u
            )
        )

        var granule = 0uL
        packets.forEachIndexed { index, packet ->
            granule += selectedFrameSize.toULong()
            result.write(
                buildOggPage(
                    packet = packet,
                    headerType = if (index == packets.size - 1) 4 else 0,
                    granulePosition = granule,
                    serial = serial,
                    sequence = (index + 2).toUInt()
                )
            )
        }
        return result.toByteArray()
    }

    /**
     * Trims per-packet padding some decoders produce so each Speex packet maps
     * to exactly one PCM frame.
     */
    fun normalizeDecodedPCM(
        pcmData: ByteArray,
        packetCount: Int,
        config: PCMConfig = PCMConfig()
    ): ByteArray {
        val count = maxOf(0, packetCount)
        if (pcmData.isEmpty() || count <= 0) return pcmData

        val bytesPerSample = maxOf(1, config.bitDepth / 8)
        val frameBytes = frameSize(config.sampleRate) * config.channels * bytesPerSample
        val expectedBytes = frameBytes * count
        if (frameBytes <= 0 || pcmData.size <= expectedBytes) return pcmData

        val decodedBytesPerPacket = pcmData.size / count
        if (pcmData.size % count != 0 || decodedBytesPerPacket % frameBytes != 0) {
            return pcmData
        }

        val result = ByteArrayOutputStream(expectedBytes)
        var offset = 0
        while (offset < pcmData.size) {
            result.write(pcmData, offset, frameBytes)
            offset += decodedBytesPerPacket
        }
        return result.toByteArray()
    }

    fun decodeSpeexToPCM(
        data: ByteArray,
        config: PCMConfig = PCMConfig(),
        quality: Int = RingSoundDefaults.SPEEX_QUALITY,
        bitsSize: Int? = null,
        allowFramedBlocks: Boolean = false,
        ffmpegPath: String = "ffmpeg",
        decoder: SpeexDecoder? = null
    ): SpeexDecodeResult {
        if (data.isEmpty()) {
            throw RingSoundError.AudioDecode("Speex data is empty")
        }
        val options = SpeexDecodeOptions(
            pcmConfig = config,
            quality = quality,
            bitsSize = bitsSize,
            allowFramedBlocks = allowFramedBlocks,
            ffmpegPath = ffmpegPath
        )
        if (decoder != null) {
            return decoder(data, options)
        }

        if (isOggSpeex(data)) {
            return SpeexDecodeResult(
                pcmData = decodeOggSpeexWithFFmpeg(data, config = config, ffmpegPath = ffmpegPath),
                pcmConfig = config,
                sourceType = "ogg-speex",
                sourceExtension = "spx"
            )
        }

        var packets = parsePacketizedSpeex(data, allowFramedBlocks = allowFramedBlocks)
        var sourceType = "packet-speex"
        if (packets == null) {
            packets = splitRawSpeexPackets(data, quality = quality, bitsSize = bitsSize)
            sourceType = "raw-speex"
        }
        if (packets == null) {
            throw RingSoundError.AudioDecode(
                "bin data does not look like packetized or raw Speex"
            )
        }

        val ogg = buildOggSpeex(packets = packets, config = config)
        val decoded = decodeOggSpeexWithFFmpeg(ogg, config = config, ffmpegPath = ffmpegPath)
        return SpeexDecodeResult(
            pcmData = normalizeDecodedPCM(decoded, packetCount = packets.size, config = config),
            pcmConfig = config,
            sourceType = sourceType,
            sourceExtension = "spx",
            packetCount = packets.size
        )
    }

    fun buildPlayableAudio(
        data: ByteArray,
        config: PCMConfig = PCMConfig(),
        quality: Int = RingSoundDefaults.SPEEX_QUALITY,
        bitsSize: Int? = null,
        allowFramedBlocks: Boolean = false,
        ffmpegPath: String = "ffmpeg",
        decoder: SpeexDecoder? = null
    ): PlayableAudio {
        if (isWAV(data)) {
            return PlayableAudio(
                data = data,
                fileExtension = "wav",
                mimeType = "audio/wav",
                playMode = "direct",
                label = "WAV",
                sourceType = "wav",
                sourceExtension = "wav",
                sourceMIMEType = "audio/wav",
                description = "Detected WAV audio; saved directly."
            )
        }

        val decoded = decodeSpeexToPCM(
            data,
            config = config,
            quality = quality,
            bitsSize = bitsSize,
            allowFramedBlocks = allowFramedBlocks,
            ffmpegPath = ffmpegPath,
            decoder = decoder
        )
        return PlayableAudio(
            data = buildWAV(pcm = decoded.pcmData, config = decoded.pcmConfig),
            fileExtension = "wav",
            mimeType = "audio/wav",
            playMode = "speex-decode",
            label = "WAV",
            pcmConfig = decoded.pcmConfig,
            sourceType = decoded.sourceType,
            sourceExtension = decoded.sourceExtension,
            sourceMIMEType = "audio/x-speex",
            description = "Decoded Speex and wrapped as WAV. ${decoded.pcmConfig}"
        )
    }

    fun decodeAudioToWAV(
        data: ByteArray,
        config: PCMConfig = PCMConfig(),
        quality: Int = RingSoundDefaults.SPEEX_QUALITY,
        bitsSize: Int? = null,
        allowFramedBlocks: Boolean = false,
        ffmpegPath: String = "ffmpeg",
        decoder: SpeexDecoder? = null
    ): ByteArray = buildPlayableAudio(
        data = data,
        config = config,
        quality = quality,
        bitsSize = bitsSize,
        allowFramedBlocks = allowFramedBlocks,
        ffmpegPath = ffmpegPath,
        decoder = decoder
    ).data

    fun buildBaseName(
        fileIndex: UInt,
        recordTime: UInt? = null,
        nowMillis: Long = System.currentTimeMillis(),
        zone: ZoneId = ZoneId.systemDefault()
    ): String {
        val recordingMillis = if (recordTime != null && recordTime != 0u) {
            recordTime.toLong() * 1_000
        } else {
            nowMillis
        }
        val date = ZonedDateTime.ofInstant(Instant.ofEpochMilli(recordingMillis), zone)
        return "ring-sound-%03d-%04d%02d%02d-%02d%02d%02d-%d".format(
            fileIndex.toInt(),
            date.year,
            date.monthValue,
            date.dayOfMonth,
            date.hour,
            date.minute,
            date.second,
            nowMillis
        )
    }

    fun buildBundleFiles(
        fileIndex: UInt,
        recordTime: UInt? = null,
        outputFile: File? = null,
        outputDirectory: File? = null,
        nowMillis: Long = System.currentTimeMillis()
    ): Pair<File, File> {
        if (outputFile != null) {
            val basePath = outputFile.path.substringBeforeLast('.')
            return Pair(File("$basePath.bin"), File("$basePath.wav"))
        }
        val directory = outputDirectory ?: File(".")
        val name = buildBaseName(
            fileIndex = fileIndex,
            recordTime = recordTime,
            nowMillis = nowMillis
        )
        return Pair(
            File(directory, "$name.bin"),
            File(directory, "$name.wav")
        )
    }

    fun saveAudioBundle(
        fileIndex: UInt,
        data: ByteArray,
        recordTime: UInt? = null,
        outputFile: File? = null,
        outputDirectory: File? = null,
        config: PCMConfig = PCMConfig(),
        quality: Int = RingSoundDefaults.SPEEX_QUALITY,
        bitsSize: Int? = null,
        allowFramedBlocks: Boolean = false,
        ffmpegPath: String = "ffmpeg",
        decoder: SpeexDecoder? = null
    ): AudioBundle {
        val (raw, playableFile) = buildBundleFiles(
            fileIndex = fileIndex,
            recordTime = recordTime,
            outputFile = outputFile,
            outputDirectory = outputDirectory
        )
        raw.parentFile?.mkdirs()
        playableFile.parentFile?.mkdirs()
        Files.write(raw.toPath(), data)
        val playable = buildPlayableAudio(
            data = data,
            config = config,
            quality = quality,
            bitsSize = bitsSize,
            allowFramedBlocks = allowFramedBlocks,
            ffmpegPath = ffmpegPath,
            decoder = decoder
        )
        Files.write(playableFile.toPath(), playable.data)
        return AudioBundle(
            rawFile = raw,
            rawFileName = raw.name,
            playableFile = playableFile,
            playableFileName = playableFile.name,
            playMode = playable.playMode,
            formatLabel = playable.label,
            playDescription = playable.description,
            pcmSummary = (playable.pcmConfig ?: config).toString(),
            rawSize = data.size,
            playableSize = playable.data.size,
            sourceType = playable.sourceType,
            sourceExtension = playable.sourceExtension
        )
    }

    /** Decodes Ogg Speex to raw PCM (s16le/u8) through an ffmpeg subprocess. */
    fun decodeOggSpeexWithFFmpeg(
        oggData: ByteArray,
        config: PCMConfig = PCMConfig(),
        ffmpegPath: String = "ffmpeg"
    ): ByteArray {
        val temporaryDirectory = Files.createTempDirectory("ring-sound-")
        try {
            val input = temporaryDirectory.resolve("input.ogg")
            val output = temporaryDirectory.resolve("output.pcm")
            val errorFile = temporaryDirectory.resolve("ffmpeg-error.log")
            Files.write(input, oggData)

            if (ffmpegPath.contains("/") && !Files.isExecutable(Path.of(ffmpegPath))) {
                throw RingSoundError.SpeexDecoderUnavailable(
                    "ffmpeg is required to decode Speex. Install ffmpeg or pass its path."
                )
            }

            val processBuilder = ProcessBuilder(
                ffmpegPath,
                "-hide_banner",
                "-loglevel", "error",
                "-f", "ogg",
                "-i", input.toString(),
                "-f", if (config.bitDepth == 16) "s16le" else "u8",
                "-ac", config.channels.toString(),
                "-ar", config.sampleRate.toString(),
                "-y",
                output.toString()
            )
            processBuilder.redirectError(errorFile.toFile())
            val process = try {
                processBuilder.start()
            } catch (error: IOException) {
                throw RingSoundError.SpeexDecoderUnavailable(
                    "ffmpeg is required to decode Speex. Install ffmpeg or pass its path."
                )
            }
            val exitCode = process.waitFor()
            if (exitCode != 0) {
                val message = runCatching {
                    Files.readAllBytes(errorFile).decodeToString().take(500)
                }.getOrDefault("")
                throw RingSoundError.AudioDecode("ffmpeg Speex decode failed: $message")
            }
            return Files.readAllBytes(output)
        } finally {
            temporaryDirectory.toFile().deleteRecursively()
        }
    }

    private fun buildOggPage(
        packet: ByteArray,
        headerType: Int,
        granulePosition: ULong,
        serial: UInt,
        sequence: UInt
    ): ByteArray {
        var remaining = packet.size
        val lacing = mutableListOf<Int>()
        while (remaining >= 255) {
            lacing.add(255)
            remaining -= 255
        }
        lacing.add(remaining)
        if (lacing.size > 255) {
            throw RingSoundError.AudioDecode("single Ogg page cannot contain this packet")
        }

        val page = ByteArrayOutputStream(27 + lacing.size + packet.size)
        page.writeASCII("OggS")
        page.write(0)
        page.write(headerType and 0xFF)
        page.writeLittleEndianUInt64(granulePosition)
        page.writeLittleEndianUInt32(serial)
        page.writeLittleEndianUInt32(sequence)
        page.writeLittleEndianUInt32(0u)
        page.write(lacing.size)
        lacing.forEach { page.write(it) }
        page.write(packet, 0, packet.size)

        val bytes = page.toByteArray()
        val checksum = oggCRC(bytes)
        bytes[22] = (checksum and 0xFFu).toByte()
        bytes[23] = ((checksum shr 8) and 0xFFu).toByte()
        bytes[24] = ((checksum shr 16) and 0xFFu).toByte()
        bytes[25] = ((checksum shr 24) and 0xFFu).toByte()
        return bytes
    }

    private fun oggCRC(data: ByteArray): UInt {
        var crc = 0u
        for (value in data) {
            crc = crc xor ((value.toUInt() and 0xFFu) shl 24)
            repeat(8) {
                crc = if (crc and 0x8000_0000u != 0u) {
                    (crc shl 1) xor 0x04C1_1DB7u
                } else {
                    crc shl 1
                }
            }
        }
        return crc
    }
}

private fun ByteArrayOutputStream.writeASCII(string: String) {
    val bytes = string.encodeToByteArray()
    write(bytes, 0, bytes.size)
}

private fun ByteArrayOutputStream.writeLittleEndianUInt16(value: Int) {
    write(value and 0xFF)
    write(value ushr 8 and 0xFF)
}

private fun ByteArrayOutputStream.writeLittleEndianUInt32(value: UInt) {
    write(value.toInt() and 0xFF)
    write((value shr 8).toInt() and 0xFF)
    write((value shr 16).toInt() and 0xFF)
    write((value shr 24).toInt() and 0xFF)
}

private fun ByteArrayOutputStream.writeLittleEndianUInt64(value: ULong) {
    for (shift in 0 until 64 step 8) {
        write((value shr shift).toInt() and 0xFF)
    }
}

private fun ByteArray.indexOf(needle: ByteArray): Int {
    if (needle.isEmpty() || size < needle.size) return -1
    for (i in 0..size - needle.size) {
        var match = true
        for (j in needle.indices) {
            if (this[i + j] != needle[j]) {
                match = false
                break
            }
        }
        if (match) return i
    }
    return -1
}
