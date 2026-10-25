package com.nervus.handteleop.control

import com.nervus.handteleop.math.Vec3
import kotlinx.serialization.json.JsonArray
import kotlinx.serialization.json.JsonPrimitive
import kotlinx.serialization.json.doubleOrNull
import java.io.File
import java.util.logging.Logger
import kotlin.math.PI
import kotlin.math.atan2
import kotlin.math.cos
import kotlin.math.sin
import kotlin.math.sqrt

/**
 * The PiPER's forward kinematics: joint angles to the end pose the arm itself reports, so the app can
 * tell whether a joint pose is inside the teleop workspace without moving there. Checked against the
 * arm: READY (0, 45, -60, 0, 45, 0) deg reads [138, 0, 418, 180, 65, 180] and joint zero
 * [56, 0, 213, 0, 85, 0] (legacy/piper-act/docs/HARDWARE.md).
 */
object PiperKinematics {
    // Adapted from piper_sdk/kinematics/piper_fk.py (C_PiperForwardKinematics, dh_is_offset = 0x01, the
    // firmware with the 2 degree j2/j3 offset): standard DH parameters, mm and radians.
    private val A = doubleArrayOf(0.0, 0.0, 285.03, -21.98, 0.0, 0.0)
    private val ALPHA = doubleArrayOf(0.0, -PI / 2, 0.0, PI / 2, -PI / 2, PI / 2)
    private val THETA = doubleArrayOf(0.0, -PI * 172.22 / 180, -102.78 / 180 * PI, 0.0, 0.0, 0.0)
    private val D = doubleArrayOf(123.0, 0.0, 0.0, 250.75, 0.0, 91.0)

    /** The end pose for joints in degrees: [x, y, z mm, rx, ry, rz deg], in the driver's convention. */
    fun endPose(jointsDeg: List<Double>): List<Double> {
        require(jointsDeg.size == 6) { "6 joints" }
        var t = identity()
        for (i in 0 until 6) t = mul(t, link(ALPHA[i], A[i], Math.toRadians(jointsDeg[i]) + THETA[i], D[i]))
        return toEuler(t)
    }

    /** The TCP position for joints in degrees, metres. */
    fun tcp(jointsDeg: List<Double>): Vec3 = endPose(jointsDeg).let { Vec3(it[0] / 1000, it[1] / 1000, it[2] / 1000) }

    // Adapted from piper_fk.py __LinkTransformtion: one link's 4x4 transform, row-major.
    private fun link(alpha: Double, a: Double, theta: Double, d: Double): DoubleArray {
        val ca = cos(alpha); val sa = sin(alpha)
        val ct = cos(theta); val st = sin(theta)
        return doubleArrayOf(
            ct, -st, 0.0, a,
            st * ca, ct * ca, -sa, -sa * d,
            st * sa, ct * sa, ca, ca * d,
            0.0, 0.0, 0.0, 1.0,
        )
    }

    // Adapted from piper_fk.py __MatrixToeula: position, then roll/pitch/yaw with the same gimbal-lock folding.
    private fun toEuler(t: DoubleArray): List<Double> {
        val r = 180 / PI
        val (rx, ry, rz) = when {
            t[8] < -1 + 0.0001 -> Triple(atan2(t[1], t[5]) * r, 90.0, 0.0)
            t[8] > 1 - 0.0001 -> Triple(-atan2(t[1], t[5]) * r, -90.0, 0.0)
            else -> {
                val bt = atan2(-t[8], sqrt(t[0] * t[0] + t[4] * t[4]))
                Triple(atan2(t[9] / cos(bt), t[10] / cos(bt)) * r, bt * r, atan2(t[4] / cos(bt), t[0] / cos(bt)) * r)
            }
        }
        return listOf(t[3], t[7], t[11], rx, ry, rz)
    }

    private fun identity() = DoubleArray(16) { if (it % 5 == 0) 1.0 else 0.0 }

    private fun mul(a: DoubleArray, b: DoubleArray) = DoubleArray(16) {
        val i = it / 4
        val j = it % 4
        (0 until 4).sumOf { k -> a[4 * i + k] * b[4 * k + j] }
    }

    /** The driver's READY pose (piper_webctl.py READY_POSE): elbow bent, wrist clear of the Euler singularity. */
    val READY_POSE = listOf(0.0, 45.0, -60.0, 0.0, 45.0, 0.0)

    /** Joint limits in degrees, from the AgileX manual as the driver clamps them (piper_webctl.py JOINT_LIMITS). */
    val JOINT_LIMITS = listOf(-154.0..154.0, 0.0..195.0, -175.0..0.0, -106.0..106.0, -75.0..75.0, -100.0..100.0)
}

/**
 * The joint pose "Go to start pose" moves to: the driver's READY pose unless the operator saved
 * another with "Set start pose here". Kept in the settings file (~/.config/nervus/hand-teleop.json), so
 * it survives a restart. Every pose is checked before use: six finite joints inside the limits whose TCP
 * is inside the teleop workspace; a saved one that isn't is ignored.
 */
class StartPoseStore(private val settings: SettingsFile, private val log: Logger) {
    constructor(file: File?, log: Logger) : this(SettingsFile(file), log)

    @Volatile
    var joints: List<Double> = PiperKinematics.READY_POSE
        private set

    val isDefault: Boolean get() = joints == PiperKinematics.READY_POSE

    init {
        load()
    }

    @Synchronized
    private fun load() {
        val saved = (settings[KEY] as? JsonArray)?.map { (it as? JsonPrimitive)?.doubleOrNull ?: Double.NaN } ?: return
        val why = problem(saved)
        if (why != null) {
            log.warning("ignoring the start pose in ${settings.file}: $why")
            return
        }
        joints = saved
    }

    /** Saves [j] (degrees) as the start pose. Null when done, else why not. */
    @Synchronized
    fun save(j: List<Double>): String? {
        problem(j)?.let { return it }
        val rounded = j.map { Math.round(it * 100) / 100.0 }
        settings.put(KEY, JsonArray(rounded.map { JsonPrimitive(it) }))?.let { return it }
        joints = rounded
        return null
    }

    /** Back to the READY pose. Null when done, else why the file couldn't be written. */
    @Synchronized
    fun reset(): String? {
        joints = PiperKinematics.READY_POSE
        return settings.put(KEY, null)
    }

    companion object {
        const val KEY = "start_pose_deg"

        /** Why [j] can't be a start pose, or null if it can. */
        fun problem(j: List<Double>): String? {
            if (j.size != 6 || j.any { !it.isFinite() }) return "a start pose is 6 joint angles"
            j.forEachIndexed { i, v ->
                if (v !in PiperKinematics.JOINT_LIMITS[i]) return "J${i + 1} = ${"%.1f".format(v)} deg is outside its limits"
            }
            val tcp = PiperKinematics.tcp(j)
            if (tcp !in Teleop.WORKSPACE) {
                return "its TCP (${mm(tcp)}) is outside the teleop workspace"
            }
            return null
        }

        fun mm(p: Vec3) = "%.0f, %.0f, %.0f mm".format(p.x * 1000, p.y * 1000, p.z * 1000)
    }
}
