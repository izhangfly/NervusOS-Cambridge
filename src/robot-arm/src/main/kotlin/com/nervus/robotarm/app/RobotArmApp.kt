package com.nervus.robotarm.app

import androidx.compose.foundation.background
import androidx.compose.foundation.gestures.awaitEachGesture
import androidx.compose.foundation.gestures.awaitFirstDown
import androidx.compose.foundation.gestures.waitForUpOrCancellation
import androidx.compose.foundation.layout.*
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.material3.*
import androidx.compose.runtime.*
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.input.pointer.pointerInput
import androidx.compose.ui.text.font.FontFamily
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.style.TextAlign
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import com.nervus.robotarm.ArmApi
import com.nervus.robotarm.ArmState
import com.nervus.robotarm.asStatusText
import com.nervus.robotarm.gripperRange
import com.nervus.robotarm.speedRange
import com.nervus.sdk.component.ComponentConfig
import com.nervus.sdk.component.NervusApp
import com.nervus.sdk.ui.attachComposeDesktop
import kotlinx.coroutines.delay
import kotlinx.coroutines.launch
import kotlin.math.roundToInt

fun main() {
    val app = RobotArmApp()
    Runtime.getRuntime().addShutdownHook(Thread { app.close() })
    app.start()
}

class RobotArmApp : NervusApp(
    config = ComponentConfig(
        componentId = "main",
        socketPath = "/run/nervus/nervud.sock",
    ),
) {
    override val requiredInterfaces = emptyList<com.nervus.sdk.component.InterfaceRequirement>()

    override fun onReady() {
        logger.info("RobotArmApp ready, launching UI...")
        attachComposeDesktop(
            title = "Robot Arm",
            width = 600.dp,
            height = 820.dp,
        ) {
            RobotArmScreen(ArmApi())
        }
    }
}

private data class JogDef(
    val label: String,
    val axis: Int,
    val dir: Int,
    val color: Color = Color(0xFF4FC3F7),
)

private val translateJogs = listOf(
    JogDef("FWD", 0, 1, Color(0xFF66BB6A)),
    JogDef("BCK", 0, -1, Color(0xFF66BB6A)),
    JogDef("LFT", 1, 1, Color(0xFF66BB6A)),
    JogDef("RGT", 1, -1, Color(0xFF66BB6A)),
    JogDef("UP", 2, 1, Color(0xFF66BB6A)),
    JogDef("DN", 2, -1, Color(0xFF66BB6A)),
)

private val rotateJogs = listOf(
    JogDef("R+", 3, 1, Color(0xFFFF8A65)),
    JogDef("R-", 3, -1, Color(0xFFFF8A65)),
    JogDef("P+", 4, 1, Color(0xFFFF8A65)),
    JogDef("P-", 4, -1, Color(0xFFFF8A65)),
)

@Composable
fun RobotArmScreen(api: ArmApi) {
    val scope = rememberCoroutineScope()
    var state by remember { mutableStateOf(ArmState()) }
    var connected by remember { mutableStateOf(false) }
    var error by remember { mutableStateOf<String?>(null) }
    var gripperLocal by remember { mutableStateOf(0f) }
    var speedLocal by remember { mutableStateOf(15f) }

    LaunchedEffect(Unit) {
        while (true) {
            try {
                api.heartbeat()
                val s = api.getState()
                state = s
                connected = true
                error = null
            } catch (e: Exception) {
                connected = false
                error = e.message ?: "disconnected"
            }
            delay(200)
        }
    }

    DisposableEffect(Unit) {
        onDispose {
            scope.launch { runCatching { api.jogStop() } }
        }
    }

    MaterialTheme(
        colorScheme = darkColorScheme(
            primary = Color(0xFF4FC3F7),
            error = Color(0xFFEF5350),
            surface = Color(0xFF1E1E2E),
            background = Color(0xFF12121A),
        ),
    ) {
        Scaffold(
            topBar = {
                TopAppBar(
                    title = { Text("Robot Arm", fontWeight = FontWeight.SemiBold) },
                    actions = {
                        StatusChip(connected, state)
                        Spacer(Modifier.width(12.dp))
                    },
                    colors = TopAppBarDefaults.topAppBarColors(
                        containerColor = Color(0xFF1A1A28),
                    ),
                )
            },
        ) { padding ->
            Column(
                Modifier.fillMaxSize().padding(padding).background(MaterialTheme.colorScheme.background).padding(12.dp),
                verticalArrangement = Arrangement.spacedBy(12.dp),
                horizontalAlignment = Alignment.CenterHorizontally,
            ) {
                if (error != null) {
                    Card(colors = CardDefaults.cardColors(containerColor = Color(0xFF3E1F1F)), modifier = Modifier.fillMaxWidth()) {
                        Text("API :8770 — $error", color = Color(0xFFFFCDD2), modifier = Modifier.padding(12.dp), fontSize = 13.sp)
                    }
                }

                ControlRow(state = state, api = api, scope = scope)
                SpeedGripperRow(speedLocal = speedLocal, gripperLocal = gripperLocal, state = state, api = api, scope = scope, onSpeed = { speedLocal = it }, onGripper = { gripperLocal = it })

                Card(Modifier.fillMaxWidth()) {
                    Column(Modifier.padding(16.dp), horizontalAlignment = Alignment.CenterHorizontally, verticalArrangement = Arrangement.spacedBy(12.dp)) {
                        Text("Translate", fontWeight = FontWeight.Bold, fontSize = 14.sp, color = Color(0xFF90A4AE))
                        JogGrid(jogs = translateJogs, api = api, scope = scope)
                    }
                }

                Card(Modifier.fillMaxWidth()) {
                    Column(Modifier.padding(16.dp), horizontalAlignment = Alignment.CenterHorizontally, verticalArrangement = Arrangement.spacedBy(12.dp)) {
                        Text("Rotate", fontWeight = FontWeight.Bold, fontSize = 14.sp, color = Color(0xFF90A4AE))
                        JogGrid(jogs = rotateJogs, api = api, scope = scope)
                    }
                }

                TelemetryPanel(state)
            }
        }
    }
}

@Composable
private fun ControlRow(state: ArmState, api: ArmApi, scope: kotlinx.coroutines.CoroutineScope) {
    Row(Modifier.fillMaxWidth(), horizontalArrangement = Arrangement.spacedBy(8.dp)) {
        Button(
            onClick = { scope.launch { runCatching { api.setEnable(!state.enabled) } } },
            colors = ButtonDefaults.buttonColors(containerColor = if (state.enabled) Color(0xFF455A64) else Color(0xFF2E7D32)),
            modifier = Modifier.weight(1f),
        ) { Text(if (state.enabled) "Disable" else "Enable", fontWeight = FontWeight.Bold) }
        Button(
            onClick = { scope.launch { runCatching { api.home() } } },
            modifier = Modifier.weight(1f),
        ) { Text("Home", fontWeight = FontWeight.Bold) }
        Button(
            onClick = { scope.launch { runCatching { api.recover() } } },
            modifier = Modifier.weight(1f),
        ) { Text("Recover", fontWeight = FontWeight.Bold) }
        Button(
            onClick = { scope.launch { runCatching { api.estop() } } },
            colors = ButtonDefaults.buttonColors(containerColor = Color(0xFFC62828)),
            modifier = Modifier.weight(1f),
        ) { Text("E-STOP", fontWeight = FontWeight.Bold, fontSize = 13.sp) }
    }
}

@Composable
private fun SpeedGripperRow(speedLocal: Float, gripperLocal: Float, state: ArmState, api: ArmApi, scope: kotlinx.coroutines.CoroutineScope, onSpeed: (Float) -> Unit, onGripper: (Float) -> Unit) {
    Card(Modifier.fillMaxWidth()) {
        Column(Modifier.padding(16.dp), verticalArrangement = Arrangement.spacedBy(8.dp)) {
            Row(verticalAlignment = Alignment.CenterVertically) {
                Text("Speed  ${speedLocal.roundToInt()}%", fontWeight = FontWeight.Medium, modifier = Modifier.width(100.dp))
                val sr = state.speedRange()
                Slider(value = speedLocal.coerceIn(sr.first.toFloat(), sr.last.toFloat()), onValueChange = onSpeed, onValueChangeFinished = { scope.launch { runCatching { api.setSpeed(speedLocal.roundToInt()) } } }, valueRange = sr.first.toFloat()..sr.last.toFloat(), modifier = Modifier.weight(1f))
            }
            Row(verticalAlignment = Alignment.CenterVertically) {
                Text("Gripper  ${"%.0f".format(gripperLocal)} mm", fontWeight = FontWeight.Medium, modifier = Modifier.width(100.dp))
                val gr = state.gripperRange()
                Slider(value = gripperLocal.coerceIn(gr.first.toFloat(), gr.second.toFloat()), onValueChange = onGripper, onValueChangeFinished = { scope.launch { runCatching { api.setGripper(gripperLocal.toDouble()) } } }, valueRange = gr.first.toFloat()..gr.second.toFloat(), modifier = Modifier.weight(1f))
            }
        }
    }
}

@Composable
private fun JogGrid(jogs: List<JogDef>, api: ArmApi, scope: kotlinx.coroutines.CoroutineScope) {
    Row(Modifier.fillMaxWidth(), horizontalArrangement = Arrangement.SpaceEvenly) {
        jogs.chunked(2).forEach { pair ->
            Column(horizontalAlignment = Alignment.CenterHorizontally, verticalArrangement = Arrangement.spacedBy(8.dp)) {
                pair.forEach { def ->
                    JogButton(def, api, scope)
                }
            }
        }
    }
}

@Composable
private fun JogButton(def: JogDef, api: ArmApi, scope: kotlinx.coroutines.CoroutineScope) {
    var pressed by remember { mutableStateOf(false) }
    OutlinedButton(
        onClick = {},
        modifier = Modifier.size(80.dp, 56.dp).pointerInput(Unit) {
            awaitEachGesture {
                awaitFirstDown(requireUnconsumed = false)
                pressed = true
                scope.launch { runCatching { api.jog(def.axis, def.dir.toDouble(), true) } }
                waitForUpOrCancellation()
                pressed = false
                scope.launch { runCatching { api.jog(def.axis, def.dir.toDouble(), false) } }
            }
        },
        colors = ButtonDefaults.outlinedButtonColors(
            containerColor = if (pressed) def.color.copy(alpha = 0.3f) else Color.Transparent,
        ),
        border = ButtonDefaults.outlinedButtonBorder(enabled = true).copy(
            brush = androidx.compose.ui.graphics.SolidColor(if (pressed) def.color else Color(0xFF374151)),
        ),
    ) {
        Text(def.label, fontSize = 14.sp, fontWeight = FontWeight.Bold, color = if (pressed) def.color else Color(0xFFE0E0E0), textAlign = TextAlign.Center)
    }
}

@Composable
private fun TelemetryPanel(state: ArmState) {
    Card(Modifier.fillMaxWidth()) {
        Column(Modifier.padding(12.dp), verticalArrangement = Arrangement.spacedBy(4.dp)) {
            Text("Telemetry", fontWeight = FontWeight.SemiBold, fontSize = 13.sp)
            Row {
                Column(Modifier.weight(1f)) {
                    val labels = listOf("J1", "J2", "J3", "J4", "J5", "J6")
                    labels.forEachIndexed { i, n ->
                        val cur = state.current.getOrElse(i) { 0.0 }
                        Text("$n  ${"%.1f°".format(cur)}", fontFamily = FontFamily.Monospace, fontSize = 11.sp, color = Color(0xFFB0BEC5))
                    }
                }
                Column(Modifier.weight(1f)) {
                    val poseLabel = listOf("X", "Y", "Z", "Rx", "Ry", "Rz")
                    poseLabel.forEachIndexed { i, n ->
                        val v = state.pose_current.getOrElse(i) { 0.0 }
                        Text("$n  ${"%.1f".format(v)}", fontFamily = FontFamily.Monospace, fontSize = 11.sp, color = Color(0xFF90A4AE))
                    }
                }
            }
            Text(state.arm_status?.asStatusText() ?: "—", fontSize = 11.sp, color = Color(0xFF78909C))
        }
    }
}

@Composable
private fun StatusChip(connected: Boolean, state: ArmState) {
    val color = when {
        state.estopped -> Color(0xFFEF5350)
        !connected -> Color(0xFF78909C)
        state.enabled -> Color(0xFF66BB6A)
        else -> Color(0xFFFFA726)
    }
    val label = when {
        state.estopped -> "E-STOP"
        !connected -> "OFFLINE"
        state.enabled -> "ENABLED"
        else -> "DISABLED"
    }
    Surface(color = color.copy(alpha = 0.2f), shape = RoundedCornerShape(16.dp)) {
        Row(Modifier.padding(horizontal = 12.dp, vertical = 6.dp), verticalAlignment = Alignment.CenterVertically, horizontalArrangement = Arrangement.spacedBy(8.dp)) {
            Box(Modifier.size(8.dp).background(color, RoundedCornerShape(50)))
            Text(label, color = color, fontSize = 12.sp, fontWeight = FontWeight.Bold)
        }
    }
}
