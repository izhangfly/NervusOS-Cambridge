package com.nervus.zring.sdk

/** Errors thrown by the Ring Sound SDK, mirroring the Swift `RingSoundError` enum. */
sealed class RingSoundError(message: String) : Exception(message) {

    class Transport(val transportMessage: String) : RingSoundError(transportMessage)

    class Protocol(val protocolMessage: String) : RingSoundError(protocolMessage)

    class Timeout(val command: Int) : RingSoundError(
        "Timed out waiting for command 0x%04X".format(command)
    )

    class Device(val code: Int, val deviceMessage: String) : RingSoundError(deviceMessage)

    class AudioDecode(val decodeMessage: String) : RingSoundError(decodeMessage)

    class SpeexDecoderUnavailable(val reason: String) : RingSoundError(reason)
}

/** Throws [RingSoundError.Device] unless [code] is [ErrorCode.SUCCESS]. */
internal fun ensureSuccess(code: Int) {
    if (code == ErrorCode.SUCCESS.value) return
    val message = ErrorCode.fromValue(code)?.message ?: "device error $code"
    throw RingSoundError.Device(code, message)
}
