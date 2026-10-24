package com.nervus.llmarm.ui

import androidx.compose.foundation.Canvas
import androidx.compose.foundation.background
import androidx.compose.foundation.border
import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.aspectRatio
import androidx.compose.foundation.layout.fillMaxHeight
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.foundation.lazy.rememberLazyListState
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.clip
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.graphics.ImageBitmap
import androidx.compose.ui.input.key.Key
import androidx.compose.ui.input.key.KeyEventType
import androidx.compose.ui.input.key.key
import androidx.compose.ui.input.key.onPreviewKeyEvent
import androidx.compose.ui.input.key.type
import androidx.compose.ui.unit.IntOffset
import androidx.compose.ui.unit.IntSize
import androidx.compose.ui.unit.dp
import com.nervus.llmarm.engine.LlmArm
import com.nervus.llmarm.engine.arr
import com.nervus.llmarm.engine.bool
import com.nervus.llmarm.engine.dbl
import com.nervus.llmarm.engine.get
import com.nervus.llmarm.engine.obj
import com.nervus.llmarm.engine.str
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.delay
import kotlinx.coroutines.isActive
import kotlinx.coroutines.withContext

@Composable
fun LlmArmScreen(m: LlmArm) {
    val s = m.status
    Column(Modifier.fillMaxSize().background(Sci.bg).padding(16.dp)) {
        TopBar(m)
        Spacer(Modifier.height(12.dp))
        if (!m.connected) {
            Panel("SERVICE OFFLINE") {
                Text("The LLM Arm service isn't answering on 127.0.0.1:8796. It starts with NervusOS " +
                    "(nervus-llm-arm.service); check it with: systemctl status nervus-llm-arm", style = Sci.body)
            }
            return@Column
        }
        Row(Modifier.fillMaxSize(), horizontalArrangement = Arrangement.spacedBy(12.dp)) {
            Column(Modifier.weight(0.56f).fillMaxHeight(), verticalArrangement = Arrangement.spacedBy(12.dp)) {
                Row(horizontalArrangement = Arrangement.spacedBy(12.dp)) {
                    CameraTile(m, "scene", "SCENE CAMERA", Modifier.weight(1f))
                    CameraTile(m, "wrist", "WRIST CAMERA", Modifier.weight(1f))
                }
                Panel("ARM", Modifier.fillMaxWidth()) {
                    val arm = s["arm"]
                    val pose = arm["pose"].arr.mapNotNull { it.dbl }
                    Facts(
                        "Driver" to if (arm["reachable"].bool) (arm["status"].str ?: "ok") else (arm["error"].str ?: "unreachable"),
                        "Motors" to if (arm["enabled"].bool) "enabled" else "off",
                        "Gripper" to (pose.takeIf { it.size >= 3 }?.let { "x %.0f  y %.0f  z %.0f mm".format(it[0], it[1], it[2]) } ?: "-"),
                        "Controlled by" to (arm["owner"].str ?: "nobody"),
                        "Calibration" to if (s["calibration"]["ready"].bool) "scene camera calibrated" else "not calibrated (pick and place are off)",
                    )
                }
                SettingsPanel(m)
            }
            ChatPanel(m, Modifier.weight(0.44f).fillMaxHeight())
        }
    }
}

@Composable
private fun TopBar(m: LlmArm) {
    val s = m.status
    Row(verticalAlignment = Alignment.CenterVertically, horizontalArrangement = Arrangement.spacedBy(10.dp)) {
        Text("LLM ARM", style = Sci.title.copy(color = Sci.accent))
        if (s["sim"].bool) Pill("SIMULATED ARM", Sci.violet)
        Spacer(Modifier.width(12.dp))
        // The model switch: one pill per provider, the active one lit.
        for ((id, p) in s["providers"].obj) {
            val on = s["provider"].str == id
            val keyOk = p["key"]["state"].str == "ready"
            Box(
                Modifier.clip(RoundedCornerShape(16.dp))
                    .background(if (on) Sci.accentDim else Sci.panel)
                    .border(1.dp, if (on) Sci.accent else Sci.line, RoundedCornerShape(16.dp))
                    .clickable(!m.running) { m.settings("provider" to id) }
                    .padding(horizontal = 14.dp, vertical = 6.dp),
            ) {
                Text("${p["label"].str}  ·  ${p["model"].str}${if (keyOk) "" else "  (no key)"}",
                    style = Sci.small.copy(color = if (on) Sci.text else Sci.dim))
            }
        }
        Spacer(Modifier.weight(1f))
        SciButton("STOP", { m.now("/api/stop") }, Kind.Danger)
        SciButton("E-STOP", { m.now("/api/estop") }, Kind.Danger)
    }
    m.notice?.let { (text, err) ->
        Spacer(Modifier.height(6.dp))
        Text(text, style = Sci.small.copy(color = if (err) Sci.bad else Sci.ok))
    }
}

@Composable
private fun CameraTile(m: LlmArm, role: String, title: String, modifier: Modifier) {
    var image by remember(role) { mutableStateOf<ImageBitmap?>(null) }
    // Frames change every couple of seconds; fetching once a second keeps the tile current without redrawing much.
    LaunchedEffect(role) {
        while (isActive) {
            image = withContext(Dispatchers.IO) { m.api.bytes("/api/camera/$role.jpg") }?.toImageBitmap()
            delay(1_000)
        }
    }
    val cam = m.status["cameras"][role]
    Panel(title, modifier, trailing = {
        Text(cam["age_s"].dbl?.let { "%.0f s old".format(it) } ?: "", style = Sci.small)
    }) {
        Box(Modifier.fillMaxWidth().aspectRatio(4f / 3f).clip(RoundedCornerShape(8.dp)).background(Color.Black)) {
            val img = image
            if (img != null && cam["fresh"].bool) {
                Canvas(Modifier.fillMaxSize()) {
                    val k = minOf(size.width / img.width, size.height / img.height)
                    val w = img.width * k
                    val h = img.height * k
                    drawImage(img, IntOffset.Zero, IntSize(img.width, img.height),
                        IntOffset(((size.width - w) / 2).toInt(), ((size.height - h) / 2).toInt()), IntSize(w.toInt(), h.toInt()))
                }
            } else {
                Text(cam["error"].str ?: "waiting for frames…", style = Sci.small,
                    modifier = Modifier.align(Alignment.Center).padding(16.dp))
            }
        }
    }
}

@Composable
private fun SettingsPanel(m: LlmArm) {
    val set = m.status["settings"]
    Panel("SETTINGS", Modifier.fillMaxWidth()) {
        Row(verticalAlignment = Alignment.CenterVertically, horizontalArrangement = Arrangement.spacedBy(16.dp)) {
            Check("Approve each move", set["confirm_moves"]["value"].bool, { m.settings("confirm_moves" to it) })
            Check("Send camera images", set["images"]["value"].bool, { m.settings("images" to it) })
            Spacer(Modifier.weight(1f))
            SciButton("Reload keys", { m.act("Reload keys", "/api/keys/reload") }, Kind.Quiet)
            SciButton("New conversation", { m.act("Clear", "/api/clear") }, Kind.Quiet)
        }
        val saved = (set["speed_mm_s"]["value"].dbl ?: 50.0).toFloat()
        var speed by remember(saved) { mutableStateOf(saved) }
        LabeledSlider("Top speed", speed, 5f..100f, { speed = it }, { "%.0f mm/s".format(it) },
            onDone = { m.settings("speed_mm_s" to speed.toDouble()) })
        val p = m.status["provider"].str
        val key = m.status["providers"][p ?: ""]["key"]
        if (key["state"].str != "ready") Text(key["message"].str ?: "", style = Sci.small.copy(color = Sci.warn))
    }
}

@Composable
private fun ChatPanel(m: LlmArm, modifier: Modifier) {
    val job = m.job
    val log = job["log"].arr
    val list = rememberLazyListState()
    LaunchedEffect(log.size) { if (log.isNotEmpty()) list.animateScrollToItem(log.size - 1) }
    var text by remember { mutableStateOf("") }
    val send = {
        if (text.isNotBlank() && !m.running) {
            m.send(text.trim())
            text = ""
        }
    }
    Panel("CONVERSATION", modifier, trailing = { job["state"].str?.let { Pill(it.uppercase(), stateColor(it)) } }) {
        LazyColumn(Modifier.weight(1f).fillMaxWidth(), state = list, verticalArrangement = Arrangement.spacedBy(6.dp)) {
            if (log.isEmpty()) item {
                Text("Say what the arm should do, for example: \"put the red block on the blue one\". " +
                    "The model sees both cameras and plans; you approve each move.", style = Sci.small)
            }
            items(log) { e ->
                val kind = e["kind"].str
                val (who, color) = when (kind) {
                    "user" -> "YOU" to Sci.accent
                    "assistant" -> "MODEL" to Sci.ok
                    "tool" -> "STEP" to Sci.violet
                    "result" -> "RESULT" to Sci.dim
                    "error" -> "ERROR" to Sci.bad
                    else -> "INFO" to Sci.warn
                }
                val body = if (kind == "tool") "${e["text"].str}(${e["data"].obj.entries.joinToString { "${it.key}=${it.value}" }})"
                else e["text"].str ?: ""
                Row {
                    Text(who, style = Sci.small.copy(color = color), modifier = Modifier.width(64.dp))
                    Text(body, style = if (kind == "result") Sci.small else Sci.body)
                }
            }
        }
        job["pending"].takeIf { it.obj.isNotEmpty() }?.let { p ->
            Spacer(Modifier.height(8.dp))
            Column(Modifier.fillMaxWidth().clip(RoundedCornerShape(8.dp)).background(Sci.panelHi)
                .border(1.dp, Sci.warn, RoundedCornerShape(8.dp)).padding(12.dp)) {
                Text("The model wants to: ${p["tool"].str}  ${p["args"].obj.entries.joinToString { "${it.key}=${it.value}" }}",
                    style = Sci.body)
                Spacer(Modifier.height(8.dp))
                Row(horizontalArrangement = Arrangement.spacedBy(10.dp)) {
                    SciButton("Approve", { m.approve(true) }, Kind.Primary)
                    SciButton("Reject", { m.approve(false) })
                }
            }
        }
        Spacer(Modifier.height(8.dp))
        Row(verticalAlignment = Alignment.Bottom, horizontalArrangement = Arrangement.spacedBy(10.dp)) {
            Field("Tell the arm", text, { text = it }, Modifier.weight(1f).onPreviewKeyEvent {
                if (it.key == Key.Enter && it.type == KeyEventType.KeyDown) { send(); true } else false
            }, enabled = !m.running)
            SciButton("Send", { send() }, Kind.Primary, enabled = !m.running && text.isNotBlank())
        }
    }
}

private fun stateColor(s: String) = when (s) {
    "running" -> Sci.accent
    "waiting" -> Sci.warn
    "done" -> Sci.ok
    else -> Sci.bad
}
