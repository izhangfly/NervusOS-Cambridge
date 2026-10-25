package com.nervus.handteleop.ui

import androidx.compose.foundation.Canvas
import androidx.compose.foundation.background
import androidx.compose.foundation.border
import androidx.compose.foundation.focusable
import androidx.compose.foundation.layout.*
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.foundation.verticalScroll
import androidx.compose.material3.Text
import androidx.compose.runtime.*
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.focus.FocusRequester
import androidx.compose.ui.focus.focusRequester
import androidx.compose.ui.geometry.Offset
import androidx.compose.ui.geometry.Size
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.graphics.ImageBitmap
import androidx.compose.ui.graphics.StrokeCap
import androidx.compose.ui.graphics.drawscope.DrawScope
import androidx.compose.ui.graphics.drawscope.Stroke
import androidx.compose.ui.graphics.drawscope.withTransform
import androidx.compose.ui.graphics.toComposeImageBitmap
import androidx.compose.ui.input.key.Key
import androidx.compose.ui.input.key.KeyEventType
import androidx.compose.ui.input.key.key
import androidx.compose.ui.input.key.onPreviewKeyEvent
import androidx.compose.ui.input.key.type
import androidx.compose.ui.unit.IntOffset
import androidx.compose.ui.unit.IntSize
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import com.nervus.handteleop.app.Pipeline
import com.nervus.handteleop.control.ControlMode
import com.nervus.handteleop.control.PinchMode
import com.nervus.handteleop.control.PiperKinematics
import com.nervus.handteleop.control.StartPoseStore
import com.nervus.handteleop.control.RollMode
import com.nervus.handteleop.control.TeleopSettings
import com.nervus.handteleop.control.TeleopStatus
import com.nervus.handteleop.vision.BodyPose
import com.nervus.handteleop.vision.HandLandmarks
import com.nervus.handteleop.vision.HandMetrics
import com.nervus.handteleop.vision.Observation
import com.nervus.handteleop.vision.RgbFrame
import kotlinx.coroutines.delay
import org.jetbrains.skia.ColorAlphaType
import org.jetbrains.skia.ColorType
import org.jetbrains.skia.ImageInfo
import java.nio.ByteBuffer
import java.nio.ByteOrder

// Per-finger colours, thumb to little finger, and per-side arm colours.
private val FINGER_COLORS = listOf(Color(0xFFFF5370), Color(0xFFFFB547), Color(0xFFFFE66D), Color(0xFF45F5A1), Color(0xFF39E1FF))
private val LEFT_ARM = Color(0xFF39E1FF)
private val RIGHT_ARM = Color(0xFFFF4FD8)
private val TORSO = Color(0xFFFFB547)

@OptIn(androidx.compose.foundation.layout.ExperimentalLayoutApi::class)
@Composable
fun TeleopScreen(p: Pipeline) {
    var obs by remember { mutableStateOf<Observation?>(null) }
    var image by remember { mutableStateOf<ImageBitmap?>(null) }
    var status by remember { mutableStateOf(TeleopStatus()) }
    // Plain fields on the pipeline don't trigger recomposition, so the numbers are copied into state here.
    var camFps by remember { mutableStateOf(0f) }
    var visionFps by remember { mutableStateOf(0f) }
    var camFormat by remember { mutableStateOf("-") }
    var camError by remember { mutableStateOf<String?>(null) }
    var visionError by remember { mutableStateOf<String?>(null) }
    val focus = remember { FocusRequester() }

    // Pull the newest frame and status ~30 times a second. The pipeline runs on its own threads;
    // the UI only ever shows what is already there.
    LaunchedEffect(Unit) {
        var lastSeq = -1L
        while (true) {
            val o = p.observation()
            val f = o?.frame ?: p.camera.latest()
            if (f != null && f.sequence != lastSeq) {
                lastSeq = f.sequence
                image = f.toImageBitmap()
                obs = o?.takeIf { it.frame.sequence == f.sequence } ?: o
            } else if (f == null && p.camera.error != null) {
                // The camera went away: say so rather than freeze on its last frame.
                image = null
                obs = null
            }
            status = p.teleop.status
            camFps = p.camera.fps
            visionFps = p.visionFps
            camFormat = p.camera.format
            camError = p.camera.error
            visionError = p.visionError
            delay(33)
        }
    }
    LaunchedEffect(Unit) { runCatching { focus.requestFocus() } }

    val engaged = status.phase == TeleopStatus.Phase.ENGAGED || status.phase == TeleopStatus.Phase.HOLD ||
        status.phase == TeleopStatus.Phase.APPROACH ||
        (status.phase == TeleopStatus.Phase.ERROR && status.leaseEpoch != 0L && status.target != null)
    val moving = status.phase == TeleopStatus.Phase.MOVING
    val moveUnavailable = p.teleop.jointMoveUnavailable
    var startPoseNote by remember { mutableStateOf<String?>(null) }

    Box(
        Modifier.fillMaxSize().background(Hud.bg)
            .focusRequester(focus).focusable()
            .onPreviewKeyEvent { e ->
                if (e.type != KeyEventType.KeyDown) return@onPreviewKeyEvent false
                when (e.key) {
                    Key.Spacebar -> {
                        when {
                            moving -> p.teleop.stopNow()
                            engaged -> p.teleop.disengage()
                            else -> p.teleop.engage()
                        }
                        true
                    }
                    Key.Escape -> { p.teleop.stopNow(); true }
                    else -> false
                }
            },
    ) {
        Row(Modifier.fillMaxSize().padding(18.dp), horizontalArrangement = Arrangement.spacedBy(18.dp)) {
            // ---------------------------------------------------------------- video
            Box(Modifier.weight(1f).fillMaxHeight()) {
                val frameColor = when (status.phase) {
                    TeleopStatus.Phase.ENGAGED, TeleopStatus.Phase.APPROACH -> Hud.cyan
                    TeleopStatus.Phase.CALIBRATING -> Hud.magenta
                    TeleopStatus.Phase.HOLD, TeleopStatus.Phase.MOVING, TeleopStatus.Phase.OUTSIDE -> Hud.amber
                    TeleopStatus.Phase.ERROR -> Hud.red
                    TeleopStatus.Phase.IDLE -> Hud.line
                }
                HudPanel(Modifier.fillMaxSize(), accent = frameColor, padding = 8.dp) {
                    Box(Modifier.fillMaxSize()) {
                        Canvas(Modifier.fillMaxSize()) {
                            val img = image
                            if (img == null) {
                                return@Canvas
                            }
                            val fit = fitRect(img.width.toFloat(), img.height.toFloat(), size, p.settings.mirrorImage)
                            // Mirrored, the operator sees themselves as in a mirror. Only the picture flips:
                            // tracking always works on the frame as the camera saw it.
                            withTransform({ if (fit.mirror) scale(-1f, 1f, pivot = Offset(fit.left + fit.width / 2, fit.top + fit.height / 2)) }) {
                                drawImage(img, srcOffset = IntOffset.Zero, srcSize = IntSize(img.width, img.height),
                                    dstOffset = IntOffset(fit.left.toInt(), fit.top.toInt()), dstSize = IntSize(fit.width.toInt(), fit.height.toInt()))
                            }
                            obs?.let {
                                drawSkeleton(it, fit, p.settings)
                                if (status.mode == ControlMode.MIRROR) drawShoulderToHand(it, fit, p.settings.hand)
                                if (status.gestureProgress > 0) drawFistRing(it, fit, p.settings.hand, status.gestureProgress, status.gestureEngages)
                            }
                            drawScanFrame(fit, frameColor)
                        }
                        if (image == null) {
                            Column(Modifier.align(Alignment.Center), horizontalAlignment = Alignment.CenterHorizontally) {
                                Text("NO SIGNAL", style = Hud.title.copy(color = Hud.dim, fontSize = 28.sp))
                                Spacer(Modifier.height(8.dp))
                                Text(camError ?: p.camera.waiting, style = Hud.value.copy(color = Hud.dim))
                            }
                        }
                        // corner telemetry
                        Column(Modifier.align(Alignment.TopStart).padding(14.dp)) {
                            Text("CAM · FRONT · $camFormat", style = Hud.label.copy(color = Hud.cyan))
                            Text("${"%.0f".format(camFps)} FPS IN · " + if (p.visionIdle) "STANDBY: NOBODY IN VIEW" else "${"%.0f".format(visionFps)} FPS TRACKED", style = Hud.label)
                            Text(
                                "HAND ${status.handDistance?.let { "%.2f M".format(it) } ?: "-"} · ROLL " +
                                    if (status.rollMode == RollMode.OFF) "OFF" else "${status.handRollDeg?.let { "%+.0f°".format(it) } ?: "-"} (${status.rollMode.label.uppercase()})",
                                style = Hud.label.copy(color = Hud.magenta),
                            )
                        }
                        Column(Modifier.align(Alignment.BottomStart).padding(14.dp)) {
                            val o = obs
                            Text("YOLO26N-POSE ${o?.let { "%.1f".format(it.poseMs) } ?: "-"} MS · HAND LANDMARKS ${o?.let { "%.1f".format(it.handMs) } ?: "-"} MS", style = Hud.label)
                            visionError?.let { Text(it, style = Hud.label.copy(color = Hud.red)) }
                            camError?.let { Text(it, style = Hud.label.copy(color = Hud.red)) }
                        }
                        Text(
                            when (status.phase) {
                                TeleopStatus.Phase.ENGAGED -> "● LIVE CONTROL"
                                TeleopStatus.Phase.HOLD -> "■ HOLD"
                                TeleopStatus.Phase.ERROR -> "▲ ATTENTION"
                                TeleopStatus.Phase.IDLE -> "○ STANDBY"
                                TeleopStatus.Phase.OUTSIDE -> "▲ OUTSIDE THE WORKSPACE"
                                TeleopStatus.Phase.MOVING -> "▶ MOVING TO THE START POSE"
                                TeleopStatus.Phase.APPROACH -> "▶ MOVING TO YOUR HAND"
                                TeleopStatus.Phase.CALIBRATING -> "◎ CALIBRATING"
                            },
                            style = Hud.title.copy(fontSize = 15.sp, color = frameColor),
                            modifier = Modifier.align(Alignment.TopEnd).padding(14.dp),
                        )
                        if (status.phase == TeleopStatus.Phase.OUTSIDE) {
                            // Engage was asked for with the arm outside the workspace: say so, big, and offer the way out.
                            HudPanel(Modifier.align(Alignment.Center).width(520.dp), accent = Hud.amber, padding = 20.dp) {
                                Text("THE ARM IS OUTSIDE THE WORKSPACE", style = Hud.title.copy(color = Hud.amber, fontSize = 18.sp))
                                Spacer(Modifier.height(10.dp))
                                Text(
                                    if (moveUnavailable == null) "Move it to the start pose and engage? It goes slowly, joint by joint; STOP or Esc halts it."
                                    else status.message,
                                    style = Hud.value.copy(fontSize = 14.sp),
                                )
                                Spacer(Modifier.height(16.dp))
                                Row(horizontalArrangement = Arrangement.spacedBy(10.dp)) {
                                    HudButton("MOVE TO START POSE", Hud.amber, Modifier.weight(1.6f), filled = true, enabled = moveUnavailable == null) {
                                        p.teleop.goToStartPose(engageAfter = true)
                                        runCatching { focus.requestFocus() }
                                    }
                                    HudButton("CANCEL", Hud.dim, Modifier.weight(1f)) {
                                        p.teleop.cancelPrompt()
                                        runCatching { focus.requestFocus() }
                                    }
                                }
                            }
                        }
                        Diagnostics(status, Modifier.align(Alignment.BottomEnd).padding(14.dp))
                        if (status.phase == TeleopStatus.Phase.CALIBRATING) {
                            Column(Modifier.align(Alignment.BottomCenter).padding(bottom = 48.dp).width(420.dp)) {
                                Text("CALIBRATING · ARM STRAIGHT OUT SIDEWAYS, LEVEL", style = Hud.label.copy(color = Hud.magenta))
                                Spacer(Modifier.height(6.dp))
                                Gauge((status.calibration ?: 0.0).toFloat(), Hud.magenta)
                            }
                        }
                        if (moving) {
                            Column(Modifier.align(Alignment.BottomCenter).padding(bottom = 48.dp).width(420.dp)) {
                                Text("MOVING TO THE START POSE · ESC STOPS", style = Hud.label.copy(color = Hud.amber))
                                Spacer(Modifier.height(6.dp))
                                Gauge((status.moveProgress ?: 0.0).toFloat(), Hud.amber)
                            }
                        }
                    }
                }
            }

            // ---------------------------------------------------------------- side panel
            Column(
                Modifier.width(420.dp).fillMaxHeight().verticalScroll(rememberScrollState()),
                verticalArrangement = Arrangement.spacedBy(14.dp),
            ) {
                Column {
                    Text("HAND TELEOP", style = Hud.title)
                    Text("${if (p.standalone) "Standalone" else "NervusOS"} · ${p.armLabel} · AgileX PiPER", style = Hud.label)
                }
                FlowRow(horizontalArrangement = Arrangement.spacedBy(8.dp), verticalArrangement = Arrangement.spacedBy(8.dp)) {
                    StatusChip("cam", "%.0f fps".format(camFps), camFps > 10f)
                    StatusChip("arm", status.armReadiness.lowercase(), when (status.armReadiness) { "READY" -> true; "-" -> null; else -> false })
                    StatusChip("lease", if (status.leaseEpoch != 0L && engaged) "epoch ${status.leaseEpoch}" else "none", if (engaged) true else null)
                    StatusChip("servo", "%.0f Hz".format(status.servoHz), if (status.phase == TeleopStatus.Phase.ENGAGED) status.servoHz > 12f else null)
                }
                // The arm's state in words: what the chip's one word can't say (motors off, teach mode, who has it).
                Text("ARM: ${status.armWords.uppercase()}", style = Hud.label.copy(color = if (status.armWords == "ready") Hud.green else Hud.amber))

                HudPanel(accent = if (engaged) Hud.cyan else Hud.dim) {
                    Row(horizontalArrangement = Arrangement.spacedBy(10.dp)) {
                        HudButton(if (engaged) "DISENGAGE" else "ENGAGE", Hud.cyan, Modifier.weight(1.4f), filled = engaged, enabled = !moving) {
                            if (engaged) p.teleop.disengage() else p.teleop.engage()
                            runCatching { focus.requestFocus() }
                        }
                        HudButton("STOP", Hud.red, Modifier.weight(1f), filled = true) {
                            p.teleop.stopNow()
                            runCatching { focus.requestFocus() }
                        }
                    }
                    Spacer(Modifier.height(10.dp))
                    Text(status.message, style = Hud.value.copy(fontSize = 13.sp, color = if (status.phase == TeleopStatus.Phase.ERROR) Hud.red else Hud.text))
                    Spacer(Modifier.height(4.dp))
                    Text("SPACE  engage / disengage     ESC  stop", style = Hud.label)
                    Spacer(Modifier.height(10.dp))
                    var gesture by remember { mutableStateOf(p.settings.fistGesture) }
                    Segmented(listOf(true to "FIST GESTURE ON", false to "FIST GESTURE OFF"), gesture) { gesture = it; p.settings.fistGesture = it }
                    Text("A fist held 1.5 s engages or disengages; while engaged a fist holds the arm (clutch).", style = Hud.label)
                }

                HudPanel(accent = Hud.amber) {
                    HudLabel("start pose (joint move, no IK)")
                    Spacer(Modifier.height(6.dp))
                    val sp = p.teleop.startPose.joints
                    val tcp = PiperKinematics.tcp(sp)
                    Text(
                        "J " + sp.joinToString("  ") { "%.0f".format(it) } + " deg" + if (p.teleop.startPose.isDefault) "  (READY)" else "  (saved)",
                        style = Hud.value.copy(fontSize = 12.sp),
                    )
                    Text("TCP ${StartPoseStore.mm(tcp)}", style = Hud.label)
                    Spacer(Modifier.height(8.dp))
                    if (moving) {
                        Gauge((status.moveProgress ?: 0.0).toFloat(), Hud.amber)
                        Spacer(Modifier.height(8.dp))
                    }
                    Hint(moveUnavailable?.let { "Not available: $it" }) {
                        HudButton("GO TO START POSE", Hud.amber, Modifier.fillMaxWidth(), enabled = moveUnavailable == null && !moving && !engaged) {
                            p.teleop.goToStartPose()
                            runCatching { focus.requestFocus() }
                        }
                    }
                    Spacer(Modifier.height(8.dp))
                    Row(horizontalArrangement = Arrangement.spacedBy(10.dp)) {
                        HudButton("SET START POSE HERE", Hud.dim, Modifier.weight(1.4f), enabled = !moving) {
                            startPoseNote = p.teleop.setStartPoseHere()
                        }
                        HudButton("RESET", Hud.dim, Modifier.weight(1f), enabled = !moving && !p.teleop.startPose.isDefault) {
                            startPoseNote = p.teleop.resetStartPose()
                        }
                    }
                    startPoseNote?.let {
                        Spacer(Modifier.height(6.dp))
                        Text(it, style = Hud.label)
                    }
                }

                HudPanel(accent = Hud.cyan) {
                    HudLabel("control mode")
                    Spacer(Modifier.height(6.dp))
                    var mode by remember { mutableStateOf(p.settings.controlMode) }
                    // Switching while in control would jump between two different mappings: only while idle.
                    val idle = !engaged && !moving && status.phase != TeleopStatus.Phase.CALIBRATING
                    Segmented(ControlMode.values().map { it to it.label.uppercase() }, mode) { if (idle) { mode = it; p.settings.controlMode = it } }
                    Spacer(Modifier.height(8.dp))
                    if (mode == ControlMode.MIRROR) {
                        Text("The arm's TCP goes where your hand is relative to your shoulder, scaled; the robot's shoulder (J2) stands in for yours.", style = Hud.label)
                        Spacer(Modifier.height(6.dp))
                        var scale by remember { mutableStateOf(p.settings.armScale.toFloat()) }
                        var width by remember { mutableStateOf(p.settings.shoulderWidthM.toFloat()) }
                        var lift by remember { mutableStateOf(p.settings.mirrorLiftM.toFloat()) }
                        // Calibration changes these from the control thread: show the new values.
                        LaunchedEffect(status.phase) { scale = p.settings.armScale.toFloat(); width = p.settings.shoulderWidthM.toFloat() }
                        HudSlider("arm scale", scale, 0.4f..1.6f, { "%.2f robot m / your m".format(it) }) { scale = it; p.settings.armScale = it.toDouble() }
                        HudSlider("shoulder width", width, 0.28f..0.50f, { "%.0f cm".format(it * 100) }) { width = it; p.settings.shoulderWidthM = it.toDouble() }
                        HudSlider("raise targets", lift, -0.10f..0.30f, { "%+.0f cm".format(it * 100) }) { lift = it; p.settings.mirrorLiftM = it.toDouble() }
                        Spacer(Modifier.height(6.dp))
                        HudButton("CALIBRATE ARM", Hud.magenta, Modifier.fillMaxWidth(), enabled = idle) {
                            p.teleop.calibrate()
                            runCatching { focus.requestFocus() }
                        }
                        Text("Face the camera and hold your arm straight out to the side, level, for 2 s.", style = Hud.label)
                    } else {
                        Text("The arm follows your hand's movement from where you engaged; a fist repositions your hand.", style = Hud.label)
                    }
                    p.tickLogFile?.let { Text("LOG ${it.path}", style = Hud.label) }
                }

                HudPanel(accent = Hud.amber) {
                    Row(Modifier.fillMaxWidth(), horizontalArrangement = Arrangement.SpaceBetween) {
                        HudLabel("gripper")
                        Text("${(status.gripper * 100).toInt()} % open · pinch ${"%.2f".format(status.pinch)}", style = Hud.value.copy(fontSize = 12.sp, color = Hud.amber))
                    }
                    Spacer(Modifier.height(8.dp))
                    Gauge(status.gripper.toFloat(), Hud.amber)
                    Spacer(Modifier.height(12.dp))
                    var mode by remember { mutableStateOf(p.settings.pinchMode) }
                    Segmented(PinchMode.values().map { it to it.label }, mode) { mode = it; p.settings.pinchMode = it }
                    Spacer(Modifier.height(8.dp))
                    var closed by remember { mutableStateOf(p.settings.closedRatio.toFloat()) }
                    var open by remember { mutableStateOf(p.settings.openRatio.toFloat()) }
                    HudSlider("closed at", closed, 0.05f..1.0f, { "%.2f".format(it) }) { closed = minOf(it, open - 0.1f); p.settings.closedRatio = closed.toDouble() }
                    HudSlider("open at", open, 0.4f..2.0f, { "%.2f".format(it) }) { open = maxOf(it, closed + 0.1f); p.settings.openRatio = open.toDouble() }
                }

                HudPanel {
                    HudLabel("operator hand")
                    Spacer(Modifier.height(6.dp))
                    var hand by remember { mutableStateOf(p.settings.hand) }
                    Segmented(listOf(HandLandmarks.Side.LEFT to "LEFT", HandLandmarks.Side.RIGHT to "RIGHT"), hand) {
                        hand = it; p.settings.hand = it
                    }
                    Spacer(Modifier.height(10.dp))
                    var side by remember { mutableStateOf(p.settings.gainSide.toFloat()) }
                    var up by remember { mutableStateOf(p.settings.gainUp.toFloat()) }
                    var reach by remember { mutableStateOf(p.settings.gainReach.toFloat()) }
                    var speed by remember { mutableStateOf(p.settings.speed.toFloat()) }
                    var lens by remember { mutableStateOf(p.settings.focalFraction.toFloat()) }
                    Text("CLUTCH-MODE GAINS: METRES OF ARM PER METRE OF HAND", style = Hud.label)
                    Spacer(Modifier.height(4.dp))
                    HudSlider("sideways", side, 0.2f..3.0f, { "%.1f x".format(it) }) { side = it; p.settings.gainSide = it.toDouble() }
                    HudSlider("up / down", up, 0.2f..3.0f, { "%.1f x".format(it) }) { up = it; p.settings.gainUp = it.toDouble() }
                    HudSlider("reach", reach, 0.0f..3.0f, { "%.1f x".format(it) }) { reach = it; p.settings.gainReach = it.toDouble() }
                    HudSlider("arm speed", speed, 0.1f..1.0f, { "%.0f %%".format(it * 100) }) { speed = it; p.settings.speed = it.toDouble() }
                    var inv by remember { mutableStateOf(p.settings.invertSide) }
                    Segmented(listOf(false to "MIRROR ME", true to "FACE ME"), inv) { inv = it; p.settings.invertSide = it }
                    Spacer(Modifier.height(8.dp))
                    var mirror by remember { mutableStateOf(p.settings.mirrorImage) }
                    Segmented(listOf(true to "MIRROR IMAGE", false to "IMAGE AS SEEN"), mirror) { mirror = it; p.settings.mirrorImage = it }
                    Spacer(Modifier.height(8.dp))
                    // Focal length over frame width; the camera's horizontal field of view is shown so it can be matched to the spec.
                    HudSlider("camera lens", lens, 0.4f..1.4f, { "%.2f · %.0f° wide".format(it, Math.toDegrees(2 * kotlin.math.atan(0.5 / it))) }) {
                        lens = it; p.settings.focalFraction = it.toDouble()
                    }
                }

                HudPanel(accent = Hud.magenta) {
                    HudLabel("hand in metres · wrist roll")
                    Spacer(Modifier.height(6.dp))
                    Row(Modifier.fillMaxWidth(), horizontalArrangement = Arrangement.SpaceBetween) {
                        Readout("distance", status.handDistance?.let { "%.2f m".format(it) } ?: "-", Hud.magenta)
                        Readout("hand roll", status.handRollDeg?.let { "%+.0f°".format(it) } ?: "-", Hud.magenta)
                        Readout("tool roll", "%+.0f°".format(status.toolRollDeg), Hud.cyan)
                    }
                    Spacer(Modifier.height(10.dp))
                    var rollMode by remember { mutableStateOf(p.settings.rollMode) }
                    Segmented(RollMode.values().map { it to "ROLL ${it.label.uppercase()}" }, rollMode) { rollMode = it; p.settings.rollMode = it }
                }

                HudPanel(accent = Hud.green) {
                    HudLabel("end effector (base_link, mm)")
                    Spacer(Modifier.height(6.dp))
                    Row(Modifier.fillMaxWidth(), horizontalArrangement = Arrangement.SpaceBetween) {
                        val t = status.tcp
                        Readout("x", t?.let { "%.0f".format(it.x * 1000) } ?: "-")
                        Readout("y", t?.let { "%.0f".format(it.y * 1000) } ?: "-")
                        Readout("z", t?.let { "%.0f".format(it.z * 1000) } ?: "-")
                    }
                    Spacer(Modifier.height(8.dp))
                    Row(Modifier.fillMaxWidth(), horizontalArrangement = Arrangement.SpaceBetween) {
                        val t = status.target
                        Readout("target x", t?.let { "%.0f".format(it.x * 1000) } ?: "-", Hud.cyan)
                        Readout("target y", t?.let { "%.0f".format(it.y * 1000) } ?: "-", Hud.cyan)
                        Readout("target z", t?.let { "%.0f".format(it.z * 1000) } ?: "-", Hud.cyan)
                    }
                }
            }
        }
    }
}

private data class Fit(val left: Float, val top: Float, val width: Float, val height: Float, val mirror: Boolean)

private fun fitRect(w: Float, h: Float, area: Size, mirror: Boolean): Fit {
    val s = minOf(area.width / w, area.height / h)
    val fw = w * s
    val fh = h * s
    return Fit((area.width - fw) / 2, (area.height - fh) / 2, fw, fh, mirror)
}

/** Frame pixel -> screen, mirrored like the picture when [Fit.mirror]. */
private fun Fit.map(x: Float, y: Float, fw: Int, fh: Int) =
    Offset(left + width * (if (mirror) 1f - x / fw else x / fw), top + height * (y / fh))

private fun DrawScope.drawSkeleton(o: Observation, fit: Fit, settings: TeleopSettings) {
    val mode = settings.pinchMode
    val operator = settings.hand
    val fw = o.frame.width
    val fh = o.frame.height
    val stroke = 4.dp.toPx()
    o.body?.let { body ->
        for ((a, b) in BodyPose.LIMBS) {
            val ka = body[a]
            val kb = body[b]
            if (ka.score < 0.3f || kb.score < 0.3f) continue
            val c = when {
                a in intArrayOf(5, 7) && b in intArrayOf(7, 9) -> LEFT_ARM
                a in intArrayOf(6, 8) && b in intArrayOf(8, 10) -> RIGHT_ARM
                a <= 4 && b <= 4 -> Hud.dim
                else -> TORSO.copy(alpha = 0.7f)
            }
            glowLine(c, fit.map(ka.x, ka.y, fw, fh), fit.map(kb.x, kb.y, fw, fh), stroke)
        }
        for ((i, k) in body.keypoints.withIndex()) {
            if (k.score < 0.3f || i < 5) continue
            joint(fit.map(k.x, k.y, fw, fh), if (i % 2 == 1) LEFT_ARM else RIGHT_ARM, 5.dp.toPx())
        }
    }
    for (h in o.hands) {
        val emphasis = h.side == operator
        for ((fi, chain) in HandLandmarks.FINGERS.withIndex()) {
            val c = FINGER_COLORS[fi].copy(alpha = if (emphasis) 1f else 0.5f)
            for (k in 0 until chain.size - 1) {
                glowLine(c, fit.map(h[chain[k]].x, h[chain[k]].y, fw, fh), fit.map(h[chain[k + 1]].x, h[chain[k + 1]].y, fw, fh), stroke * 0.7f)
            }
        }
        val palm = HandLandmarks.PALM
        for (k in 0 until palm.size - 1) {
            drawLine(Color.White.copy(alpha = 0.35f), fit.map(h[palm[k]].x, h[palm[k]].y, fw, fh), fit.map(h[palm[k + 1]].x, h[palm[k + 1]].y, fw, fh), stroke * 0.5f)
        }
        for ((i, pt) in h.points.withIndex()) {
            val fi = when (i) { in 1..4 -> 0; in 5..8 -> 1; in 9..12 -> 2; in 13..16 -> 3; in 17..20 -> 4; else -> 2 }
            joint(fit.map(pt.x, pt.y, fw, fh), FINGER_COLORS[fi], (if (i % 4 == 0) 5f else 3.5f).dp.toPx())
        }
        if (emphasis) {
            // The pinch the gripper follows, coloured from closed (red) to open (green).
            val m = HandMetrics.of(h)
            val t = h[HandLandmarks.THUMB_TIP]
            val (tx, ty) = if (mode == PinchMode.THUMB_INDEX) {
                h[HandLandmarks.INDEX_TIP].x to h[HandLandmarks.INDEX_TIP].y
            } else {
                intArrayOf(8, 12, 16, 20).map { h[it].x }.average().toFloat() to intArrayOf(8, 12, 16, 20).map { h[it].y }.average().toFloat()
            }
            val ratio = if (mode == PinchMode.THUMB_INDEX) m.pinchIndex else m.pinchFour
            val open = ((ratio - settings.closedRatio) / (settings.openRatio - settings.closedRatio)).toFloat().coerceIn(0f, 1f)
            val c = Color(1f - open, 0.35f + 0.6f * open, 0.45f, 1f)
            val a = fit.map(t.x, t.y, fw, fh)
            val b = fit.map(tx, ty, fw, fh)
            drawLine(c, a, b, 3.dp.toPx(), cap = StrokeCap.Round, pathEffect = androidx.compose.ui.graphics.PathEffect.dashPathEffect(floatArrayOf(10f, 8f)))
            drawCircle(c, 9.dp.toPx(), Offset((a.x + b.x) / 2, (a.y + b.y) / 2), style = Stroke(2.dp.toPx()))
        }
    }
}

/** The fist countdown: a ring round the operator's palm that fills while the fist is held. */
private fun DrawScope.drawFistRing(o: Observation, fit: Fit, operator: HandLandmarks.Side, progress: Double, engages: Boolean) {
    val h = o.hands.firstOrNull { it.side == operator } ?: return
    val ids = intArrayOf(0, 5, 9, 13, 17)
    val c = fit.map(ids.map { h[it].x }.average().toFloat(), ids.map { h[it].y }.average().toFloat(), o.frame.width, o.frame.height)
    val r = 46.dp.toPx()
    val color = if (engages) Hud.cyan else Hud.amber
    val tl = Offset(c.x - r, c.y - r)
    drawCircle(color.copy(alpha = 0.25f), r, c, style = Stroke(6.dp.toPx()))
    drawArc(color, -90f, (360 * progress).toFloat(), false, tl, Size(2 * r, 2 * r), style = Stroke(6.dp.toPx(), cap = StrokeCap.Round))
}

/** Mirror mode: the operating side's shoulder, and the line from it to the palm the arm copies. */
private fun DrawScope.drawShoulderToHand(o: Observation, fit: Fit, operator: HandLandmarks.Side) {
    val b = o.body ?: return
    val s = if (operator == HandLandmarks.Side.LEFT) BodyPose.L_SHOULDER else BodyPose.R_SHOULDER
    if (b[s].score < 0.3f) return
    val fw = o.frame.width
    val fh = o.frame.height
    val a = fit.map(b[s].x, b[s].y, fw, fh)
    drawCircle(Hud.magenta, 9.dp.toPx(), a, style = Stroke(3.dp.toPx()))
    val h = o.hands.firstOrNull { it.side == operator } ?: return
    val ids = intArrayOf(0, 5, 9, 13, 17)
    val c = fit.map(ids.map { h[it].x }.average().toFloat(), ids.map { h[it].y }.average().toFloat(), fw, fh)
    drawLine(Hud.magenta, a, c, 3.dp.toPx(), cap = StrokeCap.Round, pathEffect = androidx.compose.ui.graphics.PathEffect.dashPathEffect(floatArrayOf(14f, 8f)))
    drawCircle(Hud.magenta, 5.dp.toPx(), c)
}

/**
 * Why the arm is where it is, in numbers: the operator's reach (mirror) or hand movement (clutch) in cm,
 * the TCP target before and after the clamp, the arm's actual TCP, and what is limiting it.
 */
@Composable
private fun Diagnostics(s: TeleopStatus, modifier: Modifier) {
    val r = s.reach ?: return
    // Rounded to whole numbers before formatting, so a hair below zero doesn't show as "-0".
    fun cm(v: Double) = "%+d".format(Math.round(v * 100))
    fun mm(v: com.nervus.handteleop.math.Vec3?) = v?.let { "%d %d %d".format(Math.round(it.x * 1000), Math.round(it.y * 1000), Math.round(it.z * 1000)) } ?: "-"
    val tcp = s.tcp?.let { com.nervus.handteleop.math.Vec3(it.x, it.y, it.z) }
    Column(modifier.background(Hud.bg.copy(alpha = 0.72f), RoundedCornerShape(3.dp)).padding(10.dp), horizontalAlignment = Alignment.End) {
        Text(
            (if (s.mode == ControlMode.MIRROR) "SHOULDER → HAND" else "HAND MOVED") + "  FWD ${cm(r.x)} · SIDE ${cm(r.y)} · UP ${cm(r.z)} CM",
            style = Hud.label.copy(color = Hud.magenta),
        )
        Text("TARGET ${mm(s.goalRaw)} MM" + if (s.clamped) "  → ${mm(s.goal)} (CLAMPED)" else "", style = Hud.label.copy(color = if (s.clamped) Hud.amber else Hud.cyan))
        val gap = if (tcp != null && s.goal != null) "  · Δ %.0f MM".format((tcp - s.goal).norm() * 1000) else ""
        Text("TCP ${mm(tcp)} MM$gap", style = Hud.label.copy(color = Hud.green))
        if (s.slipping) Text("AT THE EDGE: ANCHOR SLIPPING", style = Hud.label.copy(color = Hud.amber))
        s.servoNote?.let { Text(it.uppercase(), style = Hud.label.copy(color = Hud.amber)) }
    }
}

private fun DrawScope.glowLine(c: Color, a: Offset, b: Offset, w: Float) {
    drawLine(c.copy(alpha = c.alpha * 0.25f), a, b, w * 3f, cap = StrokeCap.Round)
    drawLine(c, a, b, w, cap = StrokeCap.Round)
}

private fun DrawScope.joint(at: Offset, c: Color, r: Float) {
    drawCircle(c.copy(alpha = 0.3f), r * 2.2f, at)
    drawCircle(c, r, at)
    drawCircle(Color.White, r * 0.45f, at)
}

/** Thin scan lines at the edges of the image: the sci-fi frame. */
private fun DrawScope.drawScanFrame(fit: Fit, c: Color) {
    val l = 26.dp.toPx()
    val w = 2.dp.toPx()
    val x0 = fit.left; val y0 = fit.top; val x1 = fit.left + fit.width; val y1 = fit.top + fit.height
    for ((p, dx, dy) in listOf(Triple(Offset(x0, y0), 1f, 1f), Triple(Offset(x1, y0), -1f, 1f), Triple(Offset(x0, y1), 1f, -1f), Triple(Offset(x1, y1), -1f, -1f))) {
        drawLine(c, p, Offset(p.x + dx * l, p.y), w)
        drawLine(c, p, Offset(p.x, p.y + dy * l), w)
    }
    // centre reticle
    val cx = x0 + fit.width / 2
    val cy = y0 + fit.height / 2
    drawLine(c.copy(alpha = 0.35f), Offset(cx - 12f, cy), Offset(cx + 12f, cy), 1f)
    drawLine(c.copy(alpha = 0.35f), Offset(cx, cy - 12f), Offset(cx, cy + 12f), 1f)
}

private fun RgbFrame.toImageBitmap(): ImageBitmap {
    val bytes = ByteArray(width * height * 4)
    // 0xAARRGGBB stored little-endian is B, G, R, A in memory
    ByteBuffer.wrap(bytes).order(ByteOrder.LITTLE_ENDIAN).asIntBuffer().put(argb)
    val info = ImageInfo(width, height, ColorType.BGRA_8888, ColorAlphaType.OPAQUE)
    return org.jetbrains.skia.Image.makeRaster(info, bytes, width * 4).toComposeImageBitmap()
}
