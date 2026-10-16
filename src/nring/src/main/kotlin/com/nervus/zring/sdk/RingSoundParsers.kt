package com.nervus.zring.sdk

/** Parsers for response and event packet bodies. */
object RingSoundParsers {

    fun systemInfo(body: ByteArray): SystemInfo {
        val reader = BinaryReader(body)
        ensureSuccess(reader.readUInt16())
        return SystemInfo(
            firmwareVersion = reader.readStringUInt16(),
            systemTime = reader.readUInt32(),
            audioStorageTotal = reader.readUInt32(),
            audioStorageAvailable = reader.readUInt32(),
            batteryPercent = reader.readUInt16(),
            batteryCharging = reader.readUInt8() != 0,
            serialNumber = reader.readStringUInt16(),
            cpuID = reader.readStringUInt16(),
            model = reader.readStringUInt16()
        )
    }

    fun logStorageInfo(body: ByteArray): LogStorageInfo {
        val reader = BinaryReader(body)
        ensureSuccess(reader.readUInt16())
        return LogStorageInfo(
            pageSize = reader.readUInt32(),
            totalLength = reader.readUInt32()
        )
    }

    fun audioFileCount(body: ByteArray): UInt {
        val reader = BinaryReader(body)
        ensureSuccess(reader.readUInt16())
        return reader.readUInt32()
    }

    fun audioFileInfo(body: ByteArray): AudioFileInfo {
        val reader = BinaryReader(body)
        ensureSuccess(reader.readUInt16())
        val info = AudioFileInfo(
            fileIndex = reader.readUInt32(),
            recordTime = reader.readUInt32(),
            dataSize = reader.readUInt32()
        )
        if (reader.remaining != 0) {
            throw RingSoundError.Protocol(
                "Unexpected trailing audio file info bytes: ${reader.remaining}"
            )
        }
        return info
    }

    fun audioDataFrame(body: ByteArray): AudioDataFrame {
        val reader = BinaryReader(body)
        ensureSuccess(reader.readUInt16())
        val fileIndex = reader.readUInt32()
        val frameOffset = reader.readUInt32()
        val frameSize = reader.readUInt32()
        val isEnd = reader.readUInt8() != 0
        if (reader.remaining != frameSize.toInt()) {
            throw RingSoundError.Protocol(
                "Audio frame size mismatch: declared $frameSize, got ${reader.remaining}"
            )
        }
        return AudioDataFrame(
            fileIndex = fileIndex,
            frameOffset = frameOffset,
            frameSize = frameSize,
            isEnd = isEnd,
            data = reader.read(frameSize.toInt())
        )
    }

    fun sensorStartInfo(body: ByteArray): SensorStartInfo {
        val reader = BinaryReader(body)
        ensureSuccess(reader.readUInt16())
        return SensorStartInfo(
            sampleRateHz = reader.readUInt16(),
            accelerationRangeG = reader.readUInt16(),
            gyroscopeRangeDPS = reader.readUInt16()
        )
    }

    fun sensorStopInfo(body: ByteArray): SensorStopInfo {
        val reader = BinaryReader(body)
        ensureSuccess(reader.readUInt16())
        return SensorStopInfo()
    }

    fun sensorDataBatch(body: ByteArray): SensorDataBatch {
        val reader = BinaryReader(body)
        ensureSuccess(reader.readUInt16())
        val sequenceStart = reader.readUInt32()
        val frameCount = reader.readUInt16()
        val sampleSize = reader.readUInt16()
        if (sampleSize != 16) {
            throw RingSoundError.Protocol("Unsupported sensor sample size: $sampleSize")
        }
        val expectedRemaining = frameCount * sampleSize
        if (reader.remaining != expectedRemaining) {
            throw RingSoundError.Protocol(
                "Sensor batch length mismatch: expected $expectedRemaining sample bytes, got ${reader.remaining}"
            )
        }

        val samples = ArrayList<SensorDataSample>(frameCount)
        repeat(frameCount) {
            samples.add(
                SensorDataSample(
                    timestampMilliseconds = reader.readUInt32(),
                    accelerationX = reader.readInt16(),
                    accelerationY = reader.readInt16(),
                    accelerationZ = reader.readInt16(),
                    gyroscopeX = reader.readInt16(),
                    gyroscopeY = reader.readInt16(),
                    gyroscopeZ = reader.readInt16()
                )
            )
        }
        return SensorDataBatch(
            sequenceStart = sequenceStart,
            frameCount = frameCount,
            sampleSize = sampleSize,
            samples = samples
        )
    }

    fun doubleTapEvent(body: ByteArray): SensorDoubleTapEvent {
        val reader = BinaryReader(body)
        val event = SensorDoubleTapEvent(timestampMilliseconds = reader.readUInt32())
        rejectTrailing(reader.remaining, "double-tap")
        return event
    }

    fun gestureEvent(body: ByteArray): SensorGestureEvent {
        val reader = BinaryReader(body)
        val event = SensorGestureEvent(
            timestampMilliseconds = reader.readUInt32(),
            gestureID = reader.readUInt8()
        )
        rejectTrailing(reader.remaining, "gesture")
        return event
    }

    fun keyDoublePressEvent(body: ByteArray): SensorKeyDoublePressEvent {
        val reader = BinaryReader(body)
        val event = SensorKeyDoublePressEvent(timestampMilliseconds = reader.readUInt32())
        rejectTrailing(reader.remaining, "key double-press")
        return event
    }

    fun keySinglePressEvent(body: ByteArray): SensorKeySinglePressEvent {
        val reader = BinaryReader(body)
        val event = SensorKeySinglePressEvent(timestampMilliseconds = reader.readUInt32())
        rejectTrailing(reader.remaining, "key single-press")
        return event
    }

    fun gestureName(gestureID: Int): String = when (SensorGestureID.fromValue(gestureID)) {
        SensorGestureID.IDLE -> "idle"
        SensorGestureID.ROTATE_BACK -> "rotate_back"
        SensorGestureID.ROTATE_FRONT -> "rotate_front"
        SensorGestureID.WAVE -> "wave"
        null -> "unknown($gestureID)"
    }

    private fun rejectTrailing(remaining: Int, label: String) {
        if (remaining != 0) {
            throw RingSoundError.Protocol("Unexpected trailing $label event bytes: $remaining")
        }
    }
}
