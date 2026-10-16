package com.nervus.zring.sdk

/**
 * Prints download progress to stdout, mirroring the Python `ProgressPrinter`.
 *
 * Usage:
 * ```
 * val progress = ProgressPrinter("download")
 * client.downloadAudioFile(fileIndex = 0u, progress = progress::onProgress)
 * ```
 */
class ProgressPrinter(private val prefix: String = "progress") {
    private var lastPercent = -1

    fun onProgress(current: Int, total: Int) {
        val percent = if (total > 0) current * 100 / total else 0
        if (percent == lastPercent) return
        lastPercent = percent
        println("$prefix: $current/$total bytes ($percent%)")
    }

    /** Adapts this printer to the [AudioProgressHandler] functional shape. */
    fun asHandler(): AudioProgressHandler = { current, total -> onProgress(current, total) }
}
