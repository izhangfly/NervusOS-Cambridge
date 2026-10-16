package com.nervus.zring.sdk

import kotlin.time.Duration
import kotlin.time.Duration.Companion.milliseconds
import kotlin.time.Duration.Companion.nanoseconds

suspend fun RingSoundClient.getSystemInfo(timeout: Duration? = null): SystemInfo {
    val packet = request(
        command = SystemCommand.GET_INFO,
        responseCommand = SystemCommand.INFO_RESPONSE,
        timeout = timeout
    )
    return RingSoundParsers.systemInfo(packet.body)
}

suspend fun RingSoundClient.sendTimeResponse(
    requestTime: UInt,
    responseTime: UInt? = null,
    sendTime: UInt? = null
) {
    val now = (System.currentTimeMillis() / 1_000).toUInt()
    val body = BinaryWriter()
    body.writeUInt32(requestTime)
    body.writeUInt32(responseTime ?: now)
    body.writeUInt32(sendTime ?: now)
    sendCommand(TimeCommand.RESPONSE, body.data)
}

fun RingSoundClient.enableTimeSync(): PacketHandlerToken =
    addPacketHandler(TimeCommand.REQUEST) { packet ->
        val reader = BinaryReader(packet.body)
        val requestTime = runCatching { reader.readUInt32() }.getOrNull() ?: return@addPacketHandler
        runCatching { sendTimeResponse(requestTime = requestTime) }
    }

suspend fun RingSoundClient.getLogStorage(timeout: Duration? = null): LogStorageInfo {
    val packet = request(
        command = LogCommand.GET_STORAGE,
        responseCommand = LogCommand.STORAGE_RESPONSE,
        timeout = timeout
    )
    return RingSoundParsers.logStorageInfo(packet.body)
}

suspend fun RingSoundClient.readLogChunk(
    index: UInt,
    offset: UInt,
    size: UInt,
    timeout: Duration? = null
): ByteArray {
    val body = BinaryWriter()
    body.writeUInt32(index)
    body.writeUInt32(offset)
    body.writeUInt32(size)
    val packet = request(
        command = LogCommand.GET_LOG,
        responseCommand = LogCommand.LOG_RESPONSE,
        body = body.data,
        timeout = timeout
    )
    val reader = BinaryReader(packet.body)
    ensureSuccess(reader.readUInt16())
    val dataLength = reader.readUInt32().toInt()
    return reader.read(minOf(dataLength, reader.remaining))
}

suspend fun RingSoundClient.getAudioFileCount(timeout: Duration? = null): UInt {
    val packet = request(
        command = AudioCommand.GET_LIST,
        responseCommand = AudioCommand.LIST_RESPONSE,
        timeout = timeout
    )
    return RingSoundParsers.audioFileCount(packet.body)
}

suspend fun RingSoundClient.getAudioFileInfo(
    fileIndex: UInt,
    timeout: Duration? = null
): AudioFileInfo {
    val body = BinaryWriter()
    body.writeUInt16(0)
    body.writeUInt32(fileIndex)
    val packet = request(
        command = AudioCommand.START_EXTRACT,
        responseCommand = AudioCommand.FILE_INFO_RESPONSE,
        body = body.data,
        timeout = timeout
    )
    return RingSoundParsers.audioFileInfo(packet.body)
}

suspend fun RingSoundClient.readAudioFrame(
    fileIndex: UInt,
    frameOffset: UInt,
    timeout: Duration? = null
): AudioDataFrame {
    val packet = request(
        command = AudioCommand.NEXT_FRAME,
        responseCommand = AudioCommand.DATA_FRAME,
        body = audioFrameRequestBody(fileIndex, frameOffset),
        timeout = timeout
    )
    return RingSoundParsers.audioDataFrame(packet.body)
}

suspend fun RingSoundClient.endAudioExtract(
    fileIndex: UInt,
    timeout: Duration? = null,
    ignoreTimeout: Boolean = true
) {
    val body = BinaryWriter()
    body.writeUInt16(0)
    body.writeUInt32(fileIndex)
    try {
        val packet = request(
            command = AudioCommand.END_EXTRACT,
            responseCommand = AudioCommand.EXTRACT_DONE,
            body = body.data,
            timeout = timeout
        )
        val reader = BinaryReader(packet.body)
        ensureSuccess(reader.readUInt16())
    } catch (error: RingSoundError.Timeout) {
        if (!ignoreTimeout) throw error
    }
}

/**
 * Receives the audio file the firmware pushes automatically after a recording
 * is saved (a stream of `0x0505` frames without a preceding `0x0504`).
 */
suspend fun RingSoundClient.receiveAutoAudioFile(
    timeout: Duration? = null
): AutoReceivedAudioFile {
    if (!isConnected) {
        throw RingSoundError.Transport("BLE client is not connected")
    }
    val firstPacket = waitForCommand(AudioCommand.DATA_FRAME, timeout)
    var frame = RingSoundParsers.audioDataFrame(firstPacket.body)
    val assembler = AutoAudioAssembler(fileIndex = frame.fileIndex)
    var recoveryAttempts = 0

    while (true) {
        var grew = assembler.add(frame)
        if (assembler.appendPending()) {
            grew = true
        }
        if (grew) {
            recoveryAttempts = 0
        }
        assembler.completeData?.let { complete ->
            return AutoReceivedAudioFile(fileIndex = assembler.fileIndex, data = complete)
        }

        if (assembler.hasGap) {
            recoveryAttempts += 1
            if (recoveryAttempts > 3) {
                throw RingSoundError.Protocol(
                    "Audio frame gap: unable to recover offset ${assembler.received.size}"
                )
            }
            if (assembler.expectedSize == null) {
                val info = getAudioFileInfo(fileIndex = assembler.fileIndex, timeout = timeout)
                assembler.apply(info)
            }
            assembler.completeData?.let { complete ->
                return AutoReceivedAudioFile(fileIndex = assembler.fileIndex, data = complete)
            }
            requestAudioRetry(
                fileIndex = assembler.fileIndex,
                frameOffset = assembler.received.size.toUInt()
            )
        }

        while (true) {
            try {
                frame = waitAudioDataFrame(fileIndex = assembler.fileIndex, timeout = timeout)
                break
            } catch (error: RingSoundError) {
                when (error) {
                    is RingSoundError.Timeout, is RingSoundError.Protocol -> {
                        recoveryAttempts += 1
                        if (recoveryAttempts > 3) {
                            throw RingSoundError.Protocol(
                                "Audio stream stalled: unable to recover offset ${assembler.received.size}"
                            )
                        }
                        if (assembler.expectedSize == null) {
                            val info = getAudioFileInfo(
                                fileIndex = assembler.fileIndex,
                                timeout = timeout
                            )
                            assembler.apply(info)
                        }
                        assembler.completeData?.let { complete ->
                            return AutoReceivedAudioFile(
                                fileIndex = assembler.fileIndex,
                                data = complete
                            )
                        }
                        requestAudioRetry(
                            fileIndex = assembler.fileIndex,
                            frameOffset = assembler.received.size.toUInt()
                        )
                    }
                    else -> throw error
                }
            }
        }
    }
}

suspend fun RingSoundClient.clearAudioFiles(timeout: Duration? = null) {
    val packet = request(
        command = AudioCommand.CLEAR_ALL,
        responseCommand = AudioCommand.CLEAR_ALL_RESPONSE,
        timeout = timeout
    )
    val reader = BinaryReader(packet.body)
    ensureSuccess(reader.readUInt16())
}

suspend fun RingSoundClient.downloadAudioFile(
    fileIndex: UInt,
    progress: AudioProgressHandler? = null,
    timeout: Duration? = null,
    quick: Boolean = true
): DownloadedAudioFile {
    if (quick) {
        return downloadAudioFileQuick(
            fileIndex = fileIndex,
            progress = progress,
            timeout = timeout
        )
    }

    val info = getAudioFileInfo(fileIndex = fileIndex, timeout = timeout)
    if (info.fileIndex != fileIndex) {
        throw RingSoundError.Protocol(
            "Audio metadata index mismatch: expected $fileIndex, got ${info.fileIndex}"
        )
    }
    var received = ByteArray(0)

    while (true) {
        val frame = readAudioFrame(
            fileIndex = fileIndex,
            frameOffset = received.size.toUInt(),
            timeout = timeout
        )
        val merge = mergeAudioFrame(
            frame = frame,
            received = received,
            expectedSize = info.dataSize.toInt()
        )
        received = merge.received
        progress?.invoke(received.size, info.dataSize.toInt())
        if (info.dataSize > 0u && received.size >= info.dataSize.toInt()) {
            break
        }
        if (frame.isEnd) {
            throw RingSoundError.Protocol(
                "Audio stream ended at ${received.size} of ${info.dataSize} bytes"
            )
        }
        if (merge.added == 0) {
            throw RingSoundError.Protocol(
                "Audio transfer made no progress at offset ${received.size}"
            )
        }
    }

    endAudioExtract(fileIndex = fileIndex, timeout = timeout, ignoreTimeout = false)
    return DownloadedAudioFile(
        info = info,
        data = received.copyOf(info.dataSize.toInt())
    )
}

suspend fun RingSoundClient.startSensorReport(timeout: Duration? = null): SensorStartInfo {
    val packet = request(
        command = SensorCommand.START_REPORT,
        responseCommand = SensorCommand.START_REPORT_RESPONSE,
        timeout = timeout
    )
    return RingSoundParsers.sensorStartInfo(packet.body)
}

suspend fun RingSoundClient.stopSensorReport(timeout: Duration? = null): SensorStopInfo {
    val packet = request(
        command = SensorCommand.STOP_REPORT,
        responseCommand = SensorCommand.STOP_REPORT_RESPONSE,
        timeout = timeout
    )
    return RingSoundParsers.sensorStopInfo(packet.body)
}

suspend fun RingSoundClient.waitForSensorData(timeout: Duration? = null): SensorDataBatch {
    val packet = waitForCommand(SensorCommand.DATA_FRAME, timeout)
    return RingSoundParsers.sensorDataBatch(packet.body)
}

suspend fun RingSoundClient.waitForDoubleTapEvent(
    timeout: Duration? = null
): SensorDoubleTapEvent {
    val packet = waitForCommand(SensorCommand.DOUBLE_TAP, timeout)
    return RingSoundParsers.doubleTapEvent(packet.body)
}

suspend fun RingSoundClient.waitForGestureEvent(timeout: Duration? = null): SensorGestureEvent {
    val packet = waitForCommand(SensorCommand.GESTURE, timeout)
    return RingSoundParsers.gestureEvent(packet.body)
}

suspend fun RingSoundClient.waitForKeyDoublePressEvent(
    timeout: Duration? = null
): SensorKeyDoublePressEvent {
    val packet = waitForCommand(SensorCommand.KEY_DOUBLE_PRESS, timeout)
    return RingSoundParsers.keyDoublePressEvent(packet.body)
}

suspend fun RingSoundClient.waitForKeySinglePressEvent(
    timeout: Duration? = null
): SensorKeySinglePressEvent {
    val packet = waitForCommand(SensorCommand.KEY_SINGLE_PRESS, timeout)
    return RingSoundParsers.keySinglePressEvent(packet.body)
}

// MARK: - Private helpers

private suspend fun RingSoundClient.downloadAudioFileQuick(
    fileIndex: UInt,
    progress: AudioProgressHandler?,
    timeout: Duration?
): DownloadedAudioFile {
    drainCommandQueue(AudioCommand.FILE_INFO_RESPONSE.value)
    drainCommandQueue(AudioCommand.DATA_FRAME.value)
    val requestBody = BinaryWriter()
    requestBody.writeUInt16(0)
    requestBody.writeUInt32(fileIndex)
    sendCommand(AudioCommand.START_EXTRACT_QUICK, requestBody.data)

    val infoPacket = waitForCommand(AudioCommand.FILE_INFO_RESPONSE, timeout)
    val info = RingSoundParsers.audioFileInfo(infoPacket.body)
    if (info.fileIndex != fileIndex) {
        throw RingSoundError.Protocol(
            "Audio metadata index mismatch: expected $fileIndex, got ${info.fileIndex}"
        )
    }

    var received = ByteArray(0)
    var gapRetries = 0
    while (true) {
        val frame: AudioDataFrame = try {
            waitAudioDataFrame(fileIndex = fileIndex, timeout = timeout)
        } catch (error: RingSoundError.Timeout) {
            gapRetries += 1
            if (gapRetries > 3) {
                throw RingSoundError.Protocol("Audio stream stalled at offset ${received.size}")
            }
            requestAudioRetry(
                fileIndex = fileIndex,
                frameOffset = received.size.toUInt()
            )
            waitAudioDataFrame(fileIndex = fileIndex, timeout = timeout)
        }

        if (frame.frameOffset.toInt() > received.size) {
            gapRetries += 1
            if (gapRetries > 3) {
                throw RingSoundError.Protocol(
                    "Audio frame gap: expected offset ${received.size}, got ${frame.frameOffset}"
                )
            }
            requestAudioRetry(
                fileIndex = fileIndex,
                frameOffset = received.size.toUInt()
            )
            continue
        }

        val merge = mergeAudioFrame(
            frame = frame,
            received = received,
            expectedSize = info.dataSize.toInt()
        )
        received = merge.received
        if (merge.added > 0) {
            gapRetries = 0
        }
        progress?.invoke(received.size, info.dataSize.toInt())
        if (info.dataSize > 0u && received.size >= info.dataSize.toInt()) {
            break
        }
        if (frame.isEnd) {
            gapRetries += 1
            if (gapRetries > 3) {
                throw RingSoundError.Protocol(
                    "Audio stream ended at ${received.size} of ${info.dataSize} bytes"
                )
            }
            requestAudioRetry(
                fileIndex = fileIndex,
                frameOffset = received.size.toUInt()
            )
        } else if (frame.data.isEmpty()) {
            throw RingSoundError.Protocol(
                "Empty audio frame at offset ${frame.frameOffset} before end"
            )
        }
    }

    return DownloadedAudioFile(
        info = info,
        data = received.copyOf(info.dataSize.toInt())
    )
}

private suspend fun RingSoundClient.waitAudioDataFrame(
    fileIndex: UInt,
    timeout: Duration?
): AudioDataFrame {
    val interval = maxOf(1.milliseconds, timeout ?: commandTimeout)
    val deadline = System.nanoTime() + interval.inWholeNanoseconds
    while (true) {
        val remaining = deadline - System.nanoTime()
        if (remaining <= 0) {
            throw RingSoundError.Timeout(AudioCommand.DATA_FRAME.value)
        }
        val packet = waitForCommand(AudioCommand.DATA_FRAME, remaining.nanoseconds)
        val frame = RingSoundParsers.audioDataFrame(packet.body)
        if (frame.fileIndex == fileIndex) {
            return frame
        }
    }
}

private suspend fun RingSoundClient.requestAudioRetry(fileIndex: UInt, frameOffset: UInt) {
    sendCommand(
        AudioCommand.NEXT_FRAME,
        audioFrameRequestBody(fileIndex, frameOffset)
    )
}

private fun audioFrameRequestBody(fileIndex: UInt, frameOffset: UInt): ByteArray {
    val body = BinaryWriter()
    body.writeUInt16(0)
    body.writeUInt32(fileIndex)
    body.writeUInt32(frameOffset)
    // Current firmware validates a 12-byte struct but parses only 10 bytes.
    body.writeUInt16(0)
    return body.data
}

private class MergeResult(val received: ByteArray, val added: Int)

private fun mergeAudioFrame(
    frame: AudioDataFrame,
    received: ByteArray,
    expectedSize: Int
): MergeResult {
    val offset = frame.frameOffset.toInt()
    if (offset > received.size) {
        throw RingSoundError.Protocol(
            "Audio frame gap: expected offset ${received.size}, got ${frame.frameOffset}"
        )
    }
    val overlap = received.size - offset
    val overlapSize = minOf(overlap, frame.data.size)
    if (overlapSize > 0) {
        val existing = received.copyOfRange(offset, offset + overlapSize)
        if (!existing.contentEquals(frame.data.copyOf(overlapSize))) {
            throw RingSoundError.Protocol(
                "Conflicting audio overlap at offset ${frame.frameOffset}"
            )
        }
    }
    if (overlap >= frame.data.size) {
        return MergeResult(received, 0)
    }

    var newData = frame.data.copyOfRange(overlap, frame.data.size)
    if (expectedSize > 0) {
        newData = newData.copyOf(maxOf(0, expectedSize - received.size))
    }
    return MergeResult(received + newData, newData.size)
}

/** Reassembles the firmware's automatic post-recording `0x0505` stream. */
private class AutoAudioAssembler(
    val fileIndex: UInt
) {
    var received = ByteArray(0)
        private set
    private var pending = sortedMapOf<UInt, AudioDataFrame>()
    private var endOffset: Int? = null
    var expectedSize: Int? = null
        private set

    val hasGap: Boolean
        get() {
            val first = pending.keys.firstOrNull() ?: return false
            return first.toInt() > received.size
        }

    val completeData: ByteArray?
        get() {
            val end = endOffset ?: return null
            if (received.size < end) return null
            return received.copyOf(end)
        }

    fun apply(info: AudioFileInfo) {
        if (info.fileIndex != fileIndex) {
            throw RingSoundError.Protocol(
                "Audio metadata index mismatch: expected $fileIndex, got ${info.fileIndex}"
            )
        }
        if (info.dataSize == 0u) {
            throw RingSoundError.Protocol(
                "Audio metadata has invalid data_size=${info.dataSize}"
            )
        }
        expectedSize = info.dataSize.toInt()
        endOffset = info.dataSize.toInt()
        if (received.size > info.dataSize.toInt()) {
            received = received.copyOf(info.dataSize.toInt())
        }
        pending = pending.filterKeys { it.toInt() < info.dataSize.toInt() }
            .toSortedMap()
    }

    /** Returns true when the buffer grew. */
    fun add(frame: AudioDataFrame): Boolean {
        if (frame.fileIndex != fileIndex) {
            throw RingSoundError.Protocol(
                "Audio file index mismatch: expected $fileIndex, got ${frame.fileIndex}"
            )
        }
        val offset = frame.frameOffset.toInt()
        expectedSize?.let { expected ->
            if (offset > expected) {
                throw RingSoundError.Protocol(
                    "Audio frame offset $offset exceeds file size $expected"
                )
            }
        }

        var frameData = frame.data
        expectedSize?.let { expected ->
            frameData = frameData.copyOf(maxOf(0, expected - offset))
        }
        val frameEnd = offset + frameData.size
        if (frame.isEnd && expectedSize == null) {
            endOffset?.let { existing ->
                if (existing != frameEnd) {
                    throw RingSoundError.Protocol(
                        "Conflicting audio end offsets: $existing and $frameEnd"
                    )
                }
            }
            endOffset = frameEnd
        }

        val previousSize = received.size
        if (frameEnd <= previousSize) {
            val existing = received.copyOfRange(offset, frameEnd)
            if (!existing.contentEquals(frameData)) {
                throw RingSoundError.Protocol("Conflicting audio data at offset $offset")
            }
            return false
        }
        if (offset <= previousSize) {
            val overlap = previousSize - offset
            val existing = received.copyOfRange(offset, previousSize)
            if (!existing.contentEquals(frameData.copyOf(overlap))) {
                throw RingSoundError.Protocol("Conflicting audio overlap at offset $offset")
            }
            received += frameData.copyOfRange(overlap, frameData.size)
            return received.size > previousSize
        }

        val pendingSize = pending[frame.frameOffset]?.data?.size ?: -1
        if (pendingSize < frame.data.size) {
            pending[frame.frameOffset] = frame
        }
        return false
    }

    /** Returns true when the buffer grew. */
    fun appendPending(): Boolean {
        var grew = false
        while (true) {
            val offset = pending.keys.firstOrNull() ?: break
            if (offset.toInt() > received.size) break
            val frame = pending.remove(offset) ?: break
            if (add(frame)) {
                grew = true
            }
        }
        return grew
    }
}
