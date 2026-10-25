package com.nervus.handteleop.control

import com.nervus.handteleop.math.Box
import com.nervus.handteleop.math.Quat
import io.github.nervusos.iface.manipulator.v1.ArmReadiness
import io.github.nervusos.iface.manipulator.v1.ArmState
import io.github.nervusos.iface.manipulator.v1.JointState
import io.github.nervusos.iface.manipulator.v1.Pose
import kotlinx.serialization.json.Json
import kotlinx.serialization.json.JsonArray
import kotlinx.serialization.json.JsonElement
import kotlinx.serialization.json.JsonNull
import kotlinx.serialization.json.JsonObject
import kotlinx.serialization.json.JsonPrimitive
import kotlinx.serialization.json.booleanOrNull
import kotlinx.serialization.json.buildJsonObject
import kotlinx.serialization.json.doubleOrNull
import kotlinx.serialization.json.intOrNull
import kotlinx.serialization.json.jsonPrimitive
import kotlinx.serialization.json.put
import kotlinx.serialization.json.putJsonArray
import java.net.URI
import java.net.http.HttpClient
import java.net.http.HttpRequest
import java.net.http.HttpResponse
import java.time.Duration
import java.util.logging.Logger
import kotlin.math.atan2
import kotlin.math.cos
import kotlin.math.hypot
import kotlin.math.round
import kotlin.math.sin
import kotlin.math.sqrt

/**
 * The PiPER's own pose format, [x, y, z mm, rx, ry, rz deg], against Manipulator@1's metres and
 * quaternion. The angles are static-XYZ Euler ("sxyz"): R = Rz(rz) * Ry(ry) * Rx(rx), as in
 * piperd's pose.go and piper_sdk. The wrong convention would still hold a pose steady, but would
 * turn the wrist the wrong way as soon as roll is applied.
 */
object PiperPose {
    const val FRAME = "base_link"

    /** sxyz Euler angles (radians) to a unit quaternion. */
    fun eulerToQuat(rx: Double, ry: Double, rz: Double): Quat {
        val cr = cos(rx / 2); val sr = sin(rx / 2)
        val cp = cos(ry / 2); val sp = sin(ry / 2)
        val cy = cos(rz / 2); val sy = sin(rz / 2)
        return Quat(
            sr * cp * cy - cr * sp * sy,
            cr * sp * cy + sr * cp * sy,
            cr * cp * sy - sr * sp * cy,
            cr * cp * cy + sr * sp * sy,
        )
    }

    /** A quaternion to sxyz Euler angles (radians). Near gimbal lock (pitch +-90 deg) yaw folds into roll, as piper_sdk does. */
    fun quatToEuler(q: Quat): DoubleArray {
        val n = sqrt(q.x * q.x + q.y * q.y + q.z * q.z + q.w * q.w)
        val x = q.x / n; val y = q.y / n; val z = q.z / n; val w = q.w / n
        val r00 = 1 - 2 * (y * y + z * z)
        val r10 = 2 * (x * y + z * w)
        val r20 = 2 * (x * z - y * w)
        val r21 = 2 * (y * z + x * w)
        val r22 = 1 - 2 * (x * x + y * y)
        val c = hypot(r00, r10)
        if (c > 1e-9) return doubleArrayOf(atan2(r21, r22), atan2(-r20, c), atan2(r10, r00))
        val r11 = 1 - 2 * (x * x + z * z)
        val r12 = 2 * (y * z - x * w)
        return doubleArrayOf(atan2(-r12, r11), atan2(-r20, c), 0.0)
    }

    /** The driver's [x, y, z mm, rx, ry, rz deg] as a Manipulator pose. */
    fun toPose(p: List<Double>): Pose? {
        if (p.size < 6 || p.any { !it.isFinite() }) return null
        val q = eulerToQuat(Math.toRadians(p[3]), Math.toRadians(p[4]), Math.toRadians(p[5]))
        return Pose.newBuilder().setFrame(FRAME)
            .setX(p[0] / 1000).setY(p[1] / 1000).setZ(p[2] / 1000)
            .setQx(q.x).setQy(q.y).setQz(q.z).setQw(q.w)
            .build()
    }

    /** A Manipulator pose in the driver's units, or null if it isn't a usable base_link pose. */
    fun fromPose(p: Pose): List<Double>? {
        val v = listOf(p.x, p.y, p.z, p.qx, p.qy, p.qz, p.qw)
        if (p.frame != FRAME || v.any { !it.isFinite() }) return null
        // A quaternion far from unit length isn't a rotation that slipped a little; it's garbage.
        val n = sqrt(p.qx * p.qx + p.qy * p.qy + p.qz * p.qz + p.qw * p.qw)
        if (n < 0.5 || n > 1.5) return null
        val e = quatToEuler(Quat(p.qx, p.qy, p.qz, p.qw))
        return listOf(p.x * 1000, p.y * 1000, p.z * 1000, Math.toDegrees(e[0]), Math.toDegrees(e[1]), Math.toDegrees(e[2]))
    }
}

/**
 * The PiPER driver's HTTP API (hardware/piper/piper_webctl.py, loopback port 8770), for running
 * as a plain desktop app without NervusOS. It does what piperd does for the NervusOS path: servo
 * targets go to /api/servo at the control rate (their deadman is 0.4 s), the motors are enabled on
 * first use, and the driver's one-client-at-a-time ownership stands in for the control lease. A
 * refusal (HTTP 409: another client, motors off, stale feedback, the IK guard) becomes an
 * [ArmRefusedException] carrying the driver's own words, and the control loop holds on it.
 */
class DriverArm(
    private val base: String = DEFAULT_URL,
    private val client: String = CLIENT,
    private val log: Logger,
    /** How long the motors get to settle after enabling (piperd waits the same). */
    private val enableSettleMs: Long = 1300,
) : Arm {
    private val http = HttpClient.newBuilder().connectTimeout(Duration.ofMillis(500)).build()

    @Volatile
    private var last: JsonObject? = null

    override val label = "PiPER driver · ${base.removePrefix("http://")}"

    @Volatile
    override var words: String = "-"
        private set

    override val jointMoveUnavailable: String? = null

    @Volatile
    override var servoNote: String? = null
        private set

    /** The box the driver clamps servo goals to (its --workspace, reported in /api/state). */
    override val workspace: Box?
        get() = last?.let { st ->
            val w = st["workspace_mm"] as? JsonObject ?: return@let null
            fun r(k: String) = (w[k] as? JsonArray)?.map { it.jsonPrimitive.doubleOrNull ?: Double.NaN }
                ?.takeIf { it.size == 2 && it.all(Double::isFinite) }?.let { it[0] / 1000..it[1] / 1000 }
            val x = r("x"); val y = r("y"); val z = r("z")
            if (x == null || y == null || z == null) null else Box(x, y, z)
        }
    override val confirmsMoves = false
    private var lastBeat = 0L
    private var speedBefore: Int? = null

    override fun state(): ArmState {
        val st = try {
            fetchState()
        } catch (e: Exception) {
            words = "PiPER driver not reachable"
            throw e
        }
        words = describe(st)
        val b = ArmState.newBuilder().setReadiness(readiness(st))
        if (st.bool("feedback_fresh")) {
            st.doubles("pose_current")?.let { p -> PiperPose.toPose(p)?.let { b.setTcpPose(it) } }
            st.doubles("current")?.let { j ->
                b.setJoints(JointState.newBuilder().addAllPositionRad(j.map { Math.toRadians(it) }))
            }
        }
        return b.build()
    }

    override fun servo(target: Pose, velScale: Double) {
        val pose = PiperPose.fromPose(target) ?: throw ArmRefusedException("invalid target pose")
        val body = buildJsonObject {
            putJsonArray("pose") { pose.forEach { add(JsonPrimitive(it)) } }
            // max_vel_scale 0..1 to the driver's speed percentage, as piperd maps it.
            if (velScale > 0) put("speed", round(5 + 95 * velScale.coerceAtMost(1.0)).toInt())
        }
        withEnable {
            // In smooth mode the driver plans every new goal and says how (line, joint, via) or why not
            // (partial, unreachable, blocked): that's what tells the operator why the arm stopped short.
            val text = post("/api/servo", body) // a refusal (409) propagates; only the reply's parsing may fail quietly
            val reply = runCatching { Json.parseToJsonElement(text) as JsonObject }.getOrNull()
            val kind = reply?.str("plan")?.ifEmpty { null }
            val reason = reply?.str("reason")?.ifEmpty { null }
            servoNote = when {
                kind == null -> if (reply?.bool("clamped") == true) "clamped to the driver's box" else null
                kind in setOf("partial", "unreachable", "blocked") -> "driver: $kind${reason?.let { " ($it)" } ?: ""}"
                else -> null
            }
        }
    }

    override fun gripper(opening: Double, effortNm: Double) {
        val range = last?.doubles("gripper_range")
        val maxMm = range?.getOrNull(1)?.takeIf { it > 0 && it < MAX_GRIP_MM } ?: MAX_GRIP_MM
        val body = buildJsonObject {
            put("mm", opening.coerceIn(0.0, 1.0) * maxMm)
            // N*m to the driver's 0.001 N*m, 0..5000.
            if (effortNm > 0) put("effort", minOf(5000.0, round(effortNm * 1000)).toInt())
        }
        withEnable { post("/api/gripper", body) }
    }

    override fun stop() {
        post("/api/stop", JsonObject(emptyMap()))
    }

    override fun acquire(): ArmControl {
        val st = try {
            fetchState()
        } catch (e: Exception) {
            throw ArmRefusedException("the PiPER driver is not reachable at $base (${e.message})")
        }
        ownerElse(st)?.let { throw ArmRefusedException("another app is controlling the arm ($it)") }
        ensureEnabled(st)
        return object : ArmControl {
            override val description = "driver client \"$client\" at $base"
            override val epoch = 1L
            // Servo calls refresh ownership themselves; the heartbeat covers the moments between them.
            override fun renew(): Boolean {
                post("/api/heartbeat", JsonObject(emptyMap()))
                return true
            }
            override fun release() {
                post("/api/release", JsonObject(emptyMap()))
            }
        }
    }

    /**
     * The start-pose move, in the driver's joint mode (MOVE J: no IK, so no singularity faults): the
     * motors on if they are off, the speed down to [MOVE_SPEED] % for the move, then each joint's
     * target. Joint targets are streamed by the driver only while a heartbeat is fresh (1.5 s), which
     * [keepJointMove] sends every 0.5 s; /api/stop (STOP) holds the arm wherever it got to.
     */
    override fun startJointMove(jointsDeg: List<Double>, nowNanos: Long) {
        require(jointsDeg.size == 6) { "6 joints" }
        val st = fetchState()
        ownerElse(st)?.let { throw ArmRefusedException("another app is controlling the arm ($it)") }
        ensureEnabled(st)
        speedBefore = (st["speed"] as? JsonPrimitive)?.intOrNull
        post("/api/speed", buildJsonObject { put("speed", MOVE_SPEED) })
        jointsDeg.forEachIndexed { i, deg ->
            post("/api/joint", buildJsonObject { put("joint", i + 1); put("deg", deg) })
        }
        lastBeat = nowNanos
    }

    override fun keepJointMove(nowNanos: Long) {
        if (nowNanos - lastBeat >= 500_000_000L) {
            post("/api/heartbeat", JsonObject(emptyMap()))
            lastBeat = nowNanos
        }
    }

    override fun endJointMove() {
        val s = speedBefore ?: return
        speedBefore = null
        runCatching { post("/api/speed", buildJsonObject { put("speed", s) }) }
    }

    /** Runs [block]; if the driver says the motors are off, enables them (when it's safe) and tries once more. */
    private fun withEnable(block: () -> Unit) {
        try {
            block()
        } catch (e: ArmRefusedException) {
            if (e.message != NOT_ENABLED) throw e
            ensureEnabled(fetchState())
            block()
        }
    }

    /**
     * Turns the motors on if they're off. Enabling holds the measured pose, so it moves nothing by
     * itself. Refused while the arm is drag-taught, e-stopped or faulted, or its feedback is stale:
     * those need a person (Robot Arm's Recover), not an app.
     */
    private fun ensureEnabled(st: JsonObject) {
        when {
            !st.bool("feedback_fresh") -> throw ArmRefusedException("arm feedback is stale (is the PiPER powered?)")
            st.bool("teaching") -> throw ArmRefusedException("the arm is in drag-teach mode")
            st.bool("estopped") -> throw ArmRefusedException("the arm is e-stopped: press Recover in Robot Arm")
            st.str("arm_status") != "NORMAL" && !st.bool("soft_fault") ->
                throw ArmRefusedException("arm fault ${st.str("arm_status")}: press Recover in Robot Arm")
            st.bool("enabled") -> return
        }
        log.info("enabling the PiPER motors")
        post("/api/enable", buildJsonObject { put("on", true) }, timeoutMs = 3000)
        Thread.sleep(enableSettleMs)
    }

    /** The client holding the arm, if it isn't us. */
    private fun ownerElse(st: JsonObject): String? {
        val o = st["owner"] as? JsonObject ?: return null
        val c = (o["client"] as? JsonPrimitive)?.content ?: return null
        return c.takeIf { it != client }
    }

    /** The driver's view in the operator's words. */
    private fun describe(st: JsonObject): String = when {
        !st.bool("feedback_fresh") -> "not powered (no feedback from the arm)"
        st.bool("estopped") -> "e-stopped: press Recover in Robot Arm or the driver panel"
        st.str("arm_status") != "NORMAL" && !st.bool("soft_fault") -> "fault ${st.str("arm_status")}: press Recover"
        st.bool("teaching") -> "teach mode (being drag-taught)"
        ownerElse(st) != null -> "controlled by another app (${ownerElse(st)})"
        !st.bool("enabled") -> "motors off (engaging turns them on)"
        else -> "ready"
    }

    private fun readiness(st: JsonObject): ArmReadiness = when {
        !st.bool("feedback_fresh") -> ArmReadiness.ARM_READINESS_NOT_READY
        st.bool("estopped") || (st.str("arm_status") != "NORMAL" && !st.bool("soft_fault")) -> ArmReadiness.ARM_READINESS_FAULT
        // Motors that are off aren't "not ready": engaging turns them on (ensureEnabled), holding the pose.
        st.bool("teaching") -> ArmReadiness.ARM_READINESS_NOT_READY
        ownerElse(st) != null -> ArmReadiness.ARM_READINESS_NOT_READY
        else -> ArmReadiness.ARM_READINESS_READY
    }

    private fun fetchState(): JsonObject {
        val req = HttpRequest.newBuilder(URI.create("$base/api/state?lite=1")).timeout(Duration.ofMillis(800)).GET().build()
        val resp = http.send(req, HttpResponse.BodyHandlers.ofString())
        if (resp.statusCode() != 200) throw IllegalStateException("driver state: HTTP ${resp.statusCode()}")
        val st = Json.parseToJsonElement(resp.body()) as JsonObject
        last = st
        return st
    }

    private fun post(path: String, body: JsonObject, timeoutMs: Long = 500): String {
        val full = JsonObject(body + ("client" to JsonPrimitive(client)))
        val req = HttpRequest.newBuilder(URI.create(base + path))
            .timeout(Duration.ofMillis(timeoutMs))
            .header("Content-Type", "application/json")
            .POST(HttpRequest.BodyPublishers.ofString(full.toString()))
            .build()
        val resp = http.send(req, HttpResponse.BodyHandlers.ofString())
        if (resp.statusCode() == 409) throw ArmRefusedException(refusal(resp.body()))
        if (resp.statusCode() != 200) {
            throw IllegalStateException("driver $path: HTTP ${resp.statusCode()}: ${resp.body().take(200)}")
        }
        return resp.body()
    }

    companion object {
        const val DEFAULT_URL = "http://127.0.0.1:8770"
        const val CLIENT = "handteleop"
        /** The gripper's opening is mapped onto 0..70 mm, as piperd does (the driver allows up to 95). */
        const val MAX_GRIP_MM = 70.0
        const val NOT_ENABLED = "motors are not enabled"
        /** Driver speed (% of rated) for the start-pose move: slow enough to watch and stop. */
        const val MOVE_SPEED = 12

        /** The driver's 409 body, in words for the operator. */
        fun refusal(body: String): String {
            val o = runCatching { Json.parseToJsonElement(body) as JsonObject }.getOrNull() ?: return "refused by the driver"
            val error = (o["error"] as? JsonPrimitive)?.content ?: "refused by the driver"
            val owner = (o["owner"] as? JsonPrimitive)?.content
            return if (error == "busy" && owner != null) "busy: \"$owner\" is controlling the arm" else error
        }
    }
}

private fun JsonObject.bool(k: String) = (this[k] as? JsonPrimitive)?.booleanOrNull ?: false
private fun JsonObject.str(k: String) = (this[k] as? JsonPrimitive)?.takeIf { it !is JsonNull }?.content ?: ""
private fun JsonObject.doubles(k: String): List<Double>? =
    (this[k] as? JsonArray)?.map { e: JsonElement -> e.jsonPrimitive.doubleOrNull ?: Double.NaN }
