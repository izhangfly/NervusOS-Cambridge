package com.nervus.handteleop.control

import com.nervus.handteleop.math.Vec3
import java.io.BufferedWriter
import java.io.File
import java.time.LocalDateTime
import java.time.format.DateTimeFormatter
import java.util.Locale
import java.util.logging.Logger

/**
 * A CSV line per control tick while a hand is in view or the app has control, so a session can be
 * replayed and debugged afterwards: what the hand did, what the app asked for (before and after
 * clamping), where the arm actually was, and the phase. One file per run in ~/.cache/hand-teleop/;
 * the newest [KEEP] are kept. Writing never stops the control loop: a failure turns the log off.
 */
class TickLog(private val dir: File?, private val log: Logger) : AutoCloseable {
    private var out: BufferedWriter? = null
    private var failed = dir == null
    private var lastFlush = 0L
    var file: File? = null
        private set

    /**
     * One tick: [hand] the operator's reach in user axes (forward, side, up; metres) in mirror mode, or
     * the hand's movement since the anchor in clutch mode; [goalRaw] / [goal] the target before and
     * after the workspace clamp; [tcp] the arm's measured TCP. All in metres; nulls are empty fields.
     */
    fun write(nowNanos: Long, phase: String, mode: String, hand: Vec3?, goalRaw: Vec3?, goal: Vec3?, tcp: Vec3?, note: String) {
        if (failed) return
        try {
            val w = out ?: open() ?: return
            w.write(String.format(Locale.ROOT, "%.3f,%s,%s,%s,%s,%s,%s,%s\n",
                nowNanos / 1e9, phase, mode, v(hand), v(goalRaw), v(goal), v(tcp), note.replace(',', ';').replace('\n', ' ')))
            if (nowNanos - lastFlush > 1_000_000_000L) {
                w.flush()
                lastFlush = nowNanos
            }
        } catch (e: Exception) {
            failed = true
            log.warning("tick log off: ${e.message}")
        }
    }

    private fun v(p: Vec3?) = if (p == null) ",," else String.format(Locale.ROOT, "%.4f,%.4f,%.4f", p.x, p.y, p.z)

    private fun open(): BufferedWriter? {
        val d = dir ?: return null
        d.mkdirs()
        // Old sessions go first, so the folder doesn't grow forever.
        d.listFiles { f -> f.name.startsWith("session-") && f.name.endsWith(".csv") }?.sortedBy { it.name }
            ?.dropLast(KEEP - 1)?.forEach { it.delete() }
        val f = File(d, "session-${LocalDateTime.now().format(DateTimeFormatter.ofPattern("yyyyMMdd-HHmmss"))}.csv")
        file = f
        log.info("tick log: $f")
        return f.bufferedWriter().also {
            it.write(HEADER + "\n")
            out = it
        }
    }

    override fun close() {
        runCatching { out?.close() }
        out = null
    }

    companion object {
        const val KEEP = 20
        const val HEADER = "t_s,phase,mode,hand_fwd_m,hand_side_m,hand_up_m,goal_raw_x,goal_raw_y,goal_raw_z,goal_x,goal_y,goal_z,tcp_x,tcp_y,tcp_z,note"

        fun defaultDir() = File(System.getProperty("user.home"), ".cache/hand-teleop")
    }
}
