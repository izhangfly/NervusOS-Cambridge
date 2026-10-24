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
import androidx.compose.ui.focus.onFocusChanged
import androidx.compose.ui.graphics.SolidColor
import androidx.compose.foundation.text.BasicTextField
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
import androidx.compose.foundation.gestures.detectTapGestures
import androidx.compose.ui.geometry.Offset
import androidx.compose.ui.input.pointer.pointerInput
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
                var tab by remember { mutableStateOf("ARM") }
                var pick by remember { mutableStateOf<Pair<Float, Float>?>(null) }
                Row(horizontalArrangement = Arrangement.spacedBy(12.dp)) {
                    CameraTile(m, "scene", if (tab == "CALIBRATE") "SCENE CAMERA · CLICK THE GRIPPER TIP" else "SCENE CAMERA",
                        Modifier.weight(1f), onPick = if (tab == "CALIBRATE") { u, v -> pick = u to v } else null,
                        mark = if (tab == "CALIBRATE") pick else null)
                    CameraTile(m, "wrist", "WRIST CAMERA", Modifier.weight(1f))
                }
                Row(horizontalArrangement = Arrangement.spacedBy(8.dp)) {
                    for (t in listOf("ARM", "SETTINGS", "CONTEXT", "CALIBRATE")) {
                        SciButton(t, { tab = t }, if (tab == t) Kind.Primary else Kind.Quiet)
                    }
                }
                when (tab) {
                    "SETTINGS" -> SettingsPanel(m)
                    "CONTEXT" -> ContextPanel(m, Modifier.weight(1f))
                    "CALIBRATE" -> CalibratePanel(m, pick) { pick = null }
                    else -> ArmPanel(m)
                }
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
private fun CameraTile(
    m: LlmArm, role: String, title: String, modifier: Modifier,
    onPick: ((Float, Float) -> Unit)? = null, mark: Pair<Float, Float>? = null,
) {
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
                // Clicks map back to the camera's own pixels, which is what the calibration pairs with arm positions.
                val tap = if (onPick == null) Modifier else Modifier.pointerInput(img) {
                    detectTapGestures { o ->
                        val k = minOf(size.width.toFloat() / img.width, size.height.toFloat() / img.height)
                        val u = (o.x - (size.width - img.width * k) / 2) / k
                        val v = (o.y - (size.height - img.height * k) / 2) / k
                        if (u in 0f..img.width.toFloat() && v in 0f..img.height.toFloat()) onPick(u, v)
                    }
                }
                Canvas(Modifier.fillMaxSize().then(tap)) {
                    val k = minOf(size.width / img.width, size.height / img.height)
                    val w = img.width * k
                    val h = img.height * k
                    val ox = (size.width - w) / 2
                    val oy = (size.height - h) / 2
                    drawImage(img, IntOffset.Zero, IntSize(img.width, img.height),
                        IntOffset(ox.toInt(), oy.toInt()), IntSize(w.toInt(), h.toInt()))
                    mark?.let { (u, v) ->
                        val c = Offset(ox + u * k, oy + v * k)
                        drawLine(Sci.warn, c - Offset(14f, 0f), c + Offset(14f, 0f), 2f)
                        drawLine(Sci.warn, c - Offset(0f, 14f), c + Offset(0f, 14f), 2f)
                    }
                }
            } else {
                Text(cam["error"].str ?: "waiting for frames…", style = Sci.small,
                    modifier = Modifier.align(Alignment.Center).padding(16.dp))
            }
        }
    }
}

@Composable
private fun ArmPanel(m: LlmArm) {
    val s = m.status
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
    s["task"].takeIf { it.obj.isNotEmpty() }?.let { task ->
        Facts(
            "Object" to "${task["labels"]["target"].str} at ${task["target_xy_mm"].arr.joinToString { it.toString() }} mm",
            "Place" to "${task["labels"]["place"].str} (${task["place_kind"].str}) at ${task["place_xy_mm"].arr.joinToString { it.toString() }} mm",
        )
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
        if (m.status["provider"].str == "openai") {
            val effort = set["openai_effort"]["value"].str
            Row(verticalAlignment = Alignment.CenterVertically, horizontalArrangement = Arrangement.spacedBy(8.dp)) {
                Text("GPT reasoning effort", style = Sci.body, modifier = Modifier.weight(1f))
                for (e in listOf("none", "low", "medium", "high")) {
                    SciButton(e, { m.settings("openai_effort" to e) }, if (effort == e) Kind.Primary else Kind.Quiet)
                }
            }
        }
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
                    "ground" -> "SEES" to Sci.ok
                    "decision" -> "SKILL" to Sci.violet
                    "refused" -> "REFUSED" to Sci.warn
                    "error" -> "ERROR" to Sci.bad
                    else -> "INFO" to Sci.dim
                }
                Row {
                    Text(who, style = Sci.small.copy(color = color), modifier = Modifier.width(72.dp))
                    Column {
                        Text(e["text"].str ?: "", style = if (kind == "decision") Sci.num.copy(color = Sci.violet) else Sci.body)
                        e["data"]["note"].str?.takeIf { kind == "decision" && it.isNotBlank() }?.let { Text(it, style = Sci.small) }
                    }
                }
            }
        }
        job["pending"].takeIf { it.obj.isNotEmpty() }?.let { p ->
            Spacer(Modifier.height(8.dp))
            Column(Modifier.fillMaxWidth().clip(RoundedCornerShape(8.dp)).background(Sci.panelHi)
                .border(1.dp, Sci.warn, RoundedCornerShape(8.dp)).padding(12.dp)) {
                Text("Next skill: ${p["skill"].str}", style = Sci.num.copy(color = Sci.warn))
                p["note"].str?.let { Text(it, style = Sci.small) }
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

@Composable
private fun ContextPanel(m: LlmArm, modifier: Modifier) {
    // What the model is told about the arm before every decision; {task} becomes the request.
    var text by remember { mutableStateOf<String?>(null) }
    LaunchedEffect(Unit) { m.load({ get("/api/context")["text"].str ?: "" }) { text = it } }
    Panel("ARM CONTEXT", modifier.fillMaxWidth(), trailing = {
        SciButton("Reset", { m.act("Reset", "/api/context", mapOf("reset" to true)); text = null
            m.load({ get("/api/context")["text"].str ?: "" }) { text = it } }, Kind.Quiet)
        SciButton("Save", { text?.let { m.act("Save", "/api/context", mapOf("text" to it)) } }, Kind.Primary)
    }) {
        Text("Markdown. Keep {task}: it becomes what you type.", style = Sci.small)
        Spacer(Modifier.height(6.dp))
        val t = text
        if (t == null) Text("loading…", style = Sci.small)
        else BasicTextField(
            t, { text = it }, Modifier.fillMaxSize().clip(RoundedCornerShape(6.dp)).background(Sci.panelHi)
                .padding(10.dp).onFocusChanged { Typing.active = it.isFocused },
            textStyle = Sci.body.copy(fontFamily = Sci.mono), cursorBrush = SolidColor(Sci.accent),
        )
    }
}

@Composable
private fun CalibratePanel(m: LlmArm, pick: Pair<Float, Float>?, clearPick: () -> Unit) {
    // Four or more pairs of (pixel in the scene image, arm x/y at that spot), plus the table height.
    var points by remember { mutableStateOf(listOf<List<Double>>()) }
    var lowest by remember { mutableStateOf<Double?>(null) }
    val pose = m.status["arm"]["pose"].arr.mapNotNull { it.dbl }
    Panel("CALIBRATE THE SCENE CAMERA", Modifier.fillMaxWidth()) {
        Text("1. Jog the gripper tip down onto a spot on the table (Robot Arm app). 2. Click that tip in the scene " +
            "image. 3. Add point. Repeat at 4 or more spots spread over the table, then Save.", style = Sci.small)
        Spacer(Modifier.height(8.dp))
        Facts(
            "Clicked pixel" to (pick?.let { "u %.0f  v %.0f".format(it.first, it.second) } ?: "click the scene image"),
            "Arm tip now" to (pose.takeIf { it.size >= 3 }?.let { "x %.0f  y %.0f  z %.0f mm".format(it[0], it[1], it[2]) } ?: "-"),
            "Points" to "${points.size}",
            "Table height (lowest tip z)" to (lowest?.let { "%.0f mm".format(it) } ?: "-"),
            "Now" to if (m.status["calibration"]["ready"].bool) "calibrated" else "not calibrated",
        )
        Spacer(Modifier.height(8.dp))
        Row(horizontalArrangement = Arrangement.spacedBy(10.dp)) {
            SciButton("Add point", {
                if (pick != null && pose.size >= 3) {
                    points = points + listOf(listOf(pick.first.toDouble(), pick.second.toDouble(), pose[0], pose[1]))
                    lowest = minOf(lowest ?: pose[2], pose[2])
                    clearPick()
                }
            }, enabled = pick != null && pose.size >= 3)
            SciButton("Clear", { points = emptyList(); lowest = null }, Kind.Quiet)
            SciButton("Save", {
                m.act("Calibrate", "/api/calibrate", mapOf("points" to points, "table_z_mm" to lowest))
            }, Kind.Primary, enabled = points.size >= 4 && lowest != null)
        }
    }
}
