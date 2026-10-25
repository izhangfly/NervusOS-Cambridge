package com.nervus.handteleop.control

import com.sun.net.httpserver.HttpServer
import kotlinx.serialization.json.Json
import kotlinx.serialization.json.JsonArray
import kotlinx.serialization.json.JsonObject
import kotlinx.serialization.json.JsonPrimitive
import kotlinx.serialization.json.boolean
import kotlinx.serialization.json.double
import kotlinx.serialization.json.int
import kotlinx.serialization.json.jsonPrimitive
import java.net.InetSocketAddress
import java.util.Collections

/**
 * A stand-in for the PiPER driver (hardware/piper/piper_webctl.py) on an ephemeral loopback port, with
 * joints that move: in joint mode, while the motors are on and a heartbeat (or joint command) is under
 * 1.5 s old, each state read moves every joint [stepDeg] toward its target, and pose_current is the
 * forward kinematics of the joints. Never the real driver (port 8770), never the arm.
 */
class FakePiper(start: List<Double>) : AutoCloseable {
    class Call(val path: String, val body: JsonObject, val atMs: Long)

    val calls: MutableList<Call> = Collections.synchronizedList(ArrayList())
    @Volatile var stepDeg = 3.0
    @Volatile var enabled = false
    @Volatile var speed = 15
    @Volatile var mode = "joint"
    @Volatile var owner: String? = null
    val joints = start.toMutableList()
    private val targets = start.toMutableList()
    private var lastBeatMs = 0L
    private val lock = Any()
    private val server: HttpServer = HttpServer.create(InetSocketAddress("127.0.0.1", 0), 0)

    val url get() = "http://127.0.0.1:${server.address.port}"

    init {
        server.createContext("/") { ex ->
            val path = ex.requestURI.path
            val text = ex.requestBody.readBytes().decodeToString()
            val body = if (text.isBlank()) JsonObject(emptyMap()) else Json.parseToJsonElement(text) as JsonObject
            calls += Call(path, body, System.currentTimeMillis())
            val (code, reply) = synchronized(lock) { handle(path, body) }
            val bytes = reply.toByteArray()
            ex.sendResponseHeaders(code, bytes.size.toLong())
            ex.responseBody.use { it.write(bytes) }
        }
        server.start()
    }

    private fun handle(path: String, body: JsonObject): Pair<Int, String> {
        val now = System.currentTimeMillis()
        val client = (body["client"] as? JsonPrimitive)?.content
        return when (path) {
            "/api/state" -> {
                if (enabled && mode == "joint" && now - lastBeatMs < 1500) {
                    for (i in 0..5) joints[i] += (targets[i] - joints[i]).coerceIn(-stepDeg, stepDeg)
                }
                200 to state()
            }
            "/api/enable" -> { enabled = body["on"]!!.jsonPrimitive.boolean; targets.indices.forEach { targets[it] = joints[it] }; 200 to OK }
            "/api/speed" -> { speed = body["speed"]!!.jsonPrimitive.int; 200 to OK }
            "/api/joint" -> {
                if (mode != "joint") { mode = "joint"; targets.indices.forEach { targets[it] = joints[it] } }
                targets[body["joint"]!!.jsonPrimitive.int - 1] = body["deg"]!!.jsonPrimitive.double
                owner = client
                lastBeatMs = now
                200 to OK
            }
            "/api/heartbeat" -> { lastBeatMs = now; 200 to OK }
            "/api/stop", "/api/release" -> {
                mode = "joint"; targets.indices.forEach { targets[it] = joints[it] }; owner = null; lastBeatMs = 0
                200 to OK
            }
            "/api/servo" -> if (!enabled) 409 to """{"error":"motors are not enabled"}""" else { mode = "servo"; owner = client; 200 to OK }
            else -> 200 to OK
        }
    }

    private fun state(): String {
        val pose = PiperKinematics.endPose(joints)
        return """{"enabled":$enabled,"feedback_fresh":true,"estopped":false,"teaching":false,"soft_fault":false,
            "arm_status":"NORMAL","speed":$speed,"mode":"$mode","current":${JsonArray(joints.map { JsonPrimitive(it) })},
            "pose_current":${JsonArray(pose.map { JsonPrimitive(it) })},"gripper_range":[0,95],
            "owner":${owner?.let { """{"client":"$it","kind":"panel"}""" } ?: "null"}}"""
    }

    fun paths() = synchronized(calls) { calls.map { it.path } }
    fun bodies(path: String) = synchronized(calls) { calls.filter { it.path == path }.map { it.body } }
    fun count(path: String) = synchronized(calls) { calls.count { it.path == path } }

    override fun close() = server.stop(0)

    private companion object {
        const val OK = """{"ok":true}"""
    }
}
