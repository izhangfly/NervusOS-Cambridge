package com.nervus.robotarm

import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.withContext
import kotlinx.serialization.Serializable
import kotlinx.serialization.json.Json
import kotlinx.serialization.json.JsonElement
import kotlinx.serialization.json.JsonObject
import kotlinx.serialization.json.JsonPrimitive
import kotlinx.serialization.json.buildJsonObject
import kotlinx.serialization.json.put
import java.net.URI
import java.net.http.HttpClient
import java.net.http.HttpRequest
import java.net.http.HttpResponse
import java.time.Duration

@Serializable
data class ArmState(
    val current: List<Double> = List(6) { 0.0 },
    val targets: List<Double> = List(6) { 0.0 },
    val mode: String = "joint",
    val pose: List<Double> = List(6) { 0.0 },
    val pose_current: List<Double> = List(6) { 0.0 },
    val arm_status: JsonElement? = null,
    val gripper_mm: Double = 0.0,
    val enabled: Boolean = false,
    val estopped: Boolean = false,
    val speed: Int = 15,
    val limits: Map<String, List<Double>> = emptyMap(),
    val gripper_range: List<Double> = listOf(0.0, 100.0),
    val speed_range: List<Int> = listOf(1, 100),
    val log: List<JsonElement> = emptyList(),
)

class ArmApi(
    private val baseUrl: String = "http://127.0.0.1:8770",
) {
    private val client = HttpClient.newBuilder()
        .connectTimeout(Duration.ofSeconds(2))
        .build()

    private val json = Json {
        ignoreUnknownKeys = true
        isLenient = true
    }

    suspend fun getState(): ArmState = withContext(Dispatchers.IO) {
        val body = get("/api/state")
        json.decodeFromString(ArmState.serializer(), body)
    }

    suspend fun heartbeat() = post("/api/heartbeat", buildJsonObject {})

    suspend fun setJoint(joint: Int, deg: Double) = post(
        "/api/joint",
        buildJsonObject {
            put("joint", joint)
            put("deg", deg)
        },
    )

    suspend fun setGripper(mm: Double) = post(
        "/api/gripper",
        buildJsonObject { put("mm", mm) },
    )

    suspend fun setEnable(on: Boolean) = post(
        "/api/enable",
        buildJsonObject { put("on", on) },
    )

    suspend fun setSpeed(speed: Int) = post(
        "/api/speed",
        buildJsonObject { put("speed", speed) },
    )

    suspend fun estop() = post("/api/estop", buildJsonObject {})

    suspend fun recover() = post("/api/recover", buildJsonObject {})

    suspend fun setMode(mode: String) = post(
        "/api/mode",
        buildJsonObject { put("mode", mode) },
    )

    /** Cartesian jog: axis 0-5, dir +1/-1, on true/false */
    suspend fun jog(axis: Int, dir: Double, on: Boolean) = post(
        "/api/jog",
        buildJsonObject {
            put("axis", axis)
            put("dir", dir)
            put("on", on)
        },
    )

    /** Joint jog: joint 0-5, dir +1/-1, on true/false */
    suspend fun jointJog(joint: Int, dir: Double, on: Boolean) = post(
        "/api/jointjog",
        buildJsonObject {
            put("joint", joint)
            put("dir", dir)
            put("on", on)
        },
    )

    suspend fun jogStop() = post("/api/jogstop", buildJsonObject {})

    suspend fun home() = post("/api/home", buildJsonObject {})

    private fun get(path: String): String {
        val req = HttpRequest.newBuilder()
            .uri(URI.create("$baseUrl$path"))
            .timeout(Duration.ofSeconds(3))
            .GET()
            .build()
        val resp = client.send(req, HttpResponse.BodyHandlers.ofString())
        if (resp.statusCode() !in 200..299) {
            throw RuntimeException("GET $path -> ${resp.statusCode()}: ${resp.body()}")
        }
        return resp.body()
    }

    private suspend fun post(path: String, body: JsonObject): String = withContext(Dispatchers.IO) {
        val req = HttpRequest.newBuilder()
            .uri(URI.create("$baseUrl$path"))
            .timeout(Duration.ofSeconds(3))
            .header("Content-Type", "application/json")
            .POST(HttpRequest.BodyPublishers.ofString(body.toString()))
            .build()
        val resp = client.send(req, HttpResponse.BodyHandlers.ofString())
        if (resp.statusCode() !in 200..299) {
            throw RuntimeException("POST $path -> ${resp.statusCode()}: ${resp.body()}")
        }
        resp.body()
    }
}

fun JsonElement?.asStatusText(): String {
    if (this == null) return "—"
    return when (this) {
        is JsonPrimitive -> content
        is JsonObject -> this["status"]?.let { (it as? JsonPrimitive)?.content }
            ?: this["state"]?.let { (it as? JsonPrimitive)?.content }
            ?: toString()
        else -> toString()
    }
}

fun ArmState.jointLimit(joint1Based: Int): Pair<Double, Double> {
    val key = joint1Based.toString()
    val lim = limits[key]
    if (lim != null && lim.size >= 2) return lim[0] to lim[1]
    return -180.0 to 180.0
}

fun ArmState.gripperRange(): Pair<Double, Double> {
    if (gripper_range.size >= 2) return gripper_range[0] to gripper_range[1]
    return 0.0 to 100.0
}

fun ArmState.speedRange(): IntRange {
    if (speed_range.size >= 2) return speed_range[0]..speed_range[1]
    return 1..100
}
