package com.nervus.llmarm.engine

import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.setValue
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.cancel
import kotlinx.coroutines.delay
import kotlinx.coroutines.isActive
import kotlinx.coroutines.launch
import kotlinx.serialization.json.JsonElement

/**
 * Everything the screen shows, from the LLM Arm service (nervus-llm-arm, 127.0.0.1:8796).
 *
 * Status is fetched every 0.7 s while a request runs and every 2 s otherwise, and only replaces the state when it
 * changed, so an idle window doesn't redraw (the Dell and the Surface render in software). The heartbeat goes every
 * 0.5 s: the service stops the arm if the app goes quiet for 2 s during a request.
 */
class LlmArm(val api: EngineClient = EngineClient(System.getenv("LLM_ARM") ?: "http://127.0.0.1:8796")) {
    private val scope = CoroutineScope(SupervisorJob() + Dispatchers.IO)

    var status by mutableStateOf<JsonElement?>(null)
        private set
    var connected by mutableStateOf(false)
        private set
    var notice by mutableStateOf<Pair<String, Boolean>?>(null)   // text, is an error
    var busy by mutableStateOf(false)
        private set

    val job get() = status["job"]
    val running get() = job["state"].str in setOf("running", "waiting")

    fun start() {
        scope.launch {
            var last = ""
            while (isActive) {
                try {
                    val s = api.get("/api/status", 3_000)
                    val raw = s.toString()
                    if (raw != last) {
                        last = raw
                        status = s
                    }
                    connected = true
                } catch (e: EngineException) {
                    connected = false
                }
                delay(if (running) 700 else 2_000)
            }
        }
        scope.launch {
            while (isActive) {
                try {
                    api.post("/api/heartbeat", timeoutMs = 1_500)
                } catch (_: EngineException) {
                }
                delay(500)
            }
        }
    }

    private fun refresh() {
        try {
            status = api.get("/api/status", 3_000)
        } catch (_: EngineException) {
        }
    }

    /** One action at a time, off the UI thread; the service's message (or error) becomes the notice. */
    fun act(label: String, path: String, body: Map<String, Any?> = emptyMap()) {
        if (busy) return
        busy = true
        scope.launch {
            try {
                val r = api.post(path, body)
                r["message"].str?.let { notice = it to false }
            } catch (e: EngineException) {
                notice = "$label: ${e.message}" to true
            } finally {
                busy = false
                refresh()
            }
        }
    }

    /** STOP and E-STOP skip the one-at-a-time rule. */
    fun now(path: String) {
        scope.launch {
            try {
                val r = api.post(path, timeoutMs = 3_000)
                notice = (r["message"].str ?: "done") to false
            } catch (e: EngineException) {
                notice = "${e.message}" to true
            }
            refresh()
        }
    }

    /** Fetch something for one panel, off the UI thread. */
    fun <T> load(block: EngineClient.() -> T, done: (T) -> Unit) {
        scope.launch {
            try {
                done(api.block())
            } catch (e: EngineException) {
                notice = (e.message ?: "request failed") to true
            }
        }
    }

    fun send(text: String) = act("Send", "/api/chat", mapOf("text" to text))
    fun approve(ok: Boolean) = act(if (ok) "Approve" else "Reject", "/api/approve", mapOf("ok" to ok))
    fun settings(vararg kv: Pair<String, Any?>) = act("Settings", "/api/settings", mapOf(*kv))

    fun close() = scope.cancel()
}
