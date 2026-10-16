package com.nervus.sysui

import java.util.concurrent.TimeUnit
import java.util.logging.Logger

/**
 * Narrow adapter around the X11 session tools installed by Nervus.
 *
 * Window ownership remains with Openbox. Applications only request one of the
 * three session operations needed by the system UI: activate a named window,
 * deliver Back to the focused window, or hide the current root window.
 */
object X11WindowControl {
    private val log = Logger.getLogger(X11WindowControl::class.java.name)

    /**
     * Activates the exact window title, retrying while a newly launched Compose
     * process is still creating its X11 window.
     */
    fun activateWindow(
        title: String,
        attempts: Int = 50,
        retryDelayMs: Long = 100,
    ): Boolean {
        require(attempts > 0) { "attempts must be positive" }
        repeat(attempts) { attempt ->
            if (run("/usr/bin/wmctrl", "-F", "-a", title)) {
                return true
            }
            if (attempt + 1 < attempts) {
                try {
                    Thread.sleep(retryDelayMs)
                } catch (_: InterruptedException) {
                    Thread.currentThread().interrupt()
                    return false
                }
            }
        }
        log.warning("cannot activate X11 window with exact title: $title")
        return false
    }

    /** Delivers the Nervus Back key to the currently focused application. */
    fun sendBack(): Boolean =
        run("/usr/bin/xdotool", "key", "--clearmodifiers", "Escape")

    /**
     * Hides the active application so the window below becomes visible.
     *
     * Do not synthesize Alt+Tab here: Openbox intentionally presents its window
     * switcher for that shortcut, which is not part of the system Back UI.
     */
    fun hideActiveWindow(): Boolean =
        run("/usr/bin/xdotool", "getactivewindow", "windowminimize")

    private fun run(vararg command: String): Boolean {
        return try {
            val process = ProcessBuilder(*command)
                .redirectOutput(ProcessBuilder.Redirect.DISCARD)
                .redirectError(ProcessBuilder.Redirect.DISCARD)
                .start()
            if (!process.waitFor(COMMAND_TIMEOUT_SECONDS, TimeUnit.SECONDS)) {
                process.destroyForcibly()
                log.warning("X11 command timed out: ${command.firstOrNull()}")
                false
            } else {
                process.exitValue() == 0
            }
        } catch (_: InterruptedException) {
            Thread.currentThread().interrupt()
            false
        } catch (e: Exception) {
            log.warning("X11 command failed (${command.firstOrNull()}): ${e.message}")
            false
        }
    }

    private const val COMMAND_TIMEOUT_SECONDS = 2L
}
