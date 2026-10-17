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
import com.nervus.handteleop.control.PinchMode
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
        (status.phase == TeleopStatus.Phase.ERROR && status.leaseEpoch != 0L && status.target != null)

    Box(
        Modifier.fillMaxSize().background(Hud.bg)
            .focusRequester(focus).focusable()
            .onPreviewKeyEvent { e ->
                if (e.type != KeyEventType.KeyDown) return@onPreviewKeyEvent false
                when (e.key) {
                    Key.Spacebar -> { if (engaged) p.teleop.disengage() else p.teleop.engage(); true }
                    Key.Escape -> { p.teleop.stopNow(); true }
                    else -> false
                }
            },
    ) {
        Row(Modifier.fillMaxSize().padding(18.dp), horizontalArrangement = Arrangement.spacedBy(18.dp)) {
            // ---------------------------------------------------------------- video
            Box(Modifier.weight(1f).fillMaxHeight()) {
                val frameColor = when (status.phase) {
                    TeleopStatus.Phase.ENGAGED -> Hud.cyan
                    TeleopStatus.Phase.HOLD -> Hud.amber
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
                            val fit = fitRect(img.width.toFloat(), img.height.toFloat(), size)
                            // Mirror: the operator sees themselves like in a mirror.
                            withTransform({ scale(-1f, 1f, pivot = Offset(fit.left + fit.width / 2, fit.top + fit.height / 2)) }) {
                                drawImage(img, srcOffset = IntOffset.Zero, srcSize = IntSize(img.width, img.height),
                                    dstOffset = IntOffset(fit.left.toInt(), fit.top.toInt()), dstSize = IntSize(fit.width.toInt(), fit.height.toInt()))
                            }
                            obs?.let { drawSkeleton(it, fit, p.settings.pinchMode, p.settings.hand) }
                            drawScanFrame(fit, frameColor)
                        }
                        if (image == null) {
                            Column(Modifier.align(Alignment.Center), horizontalAlignment = Alignment.CenterHorizontally) {
                                Text("NO SIGNAL", style = Hud.title.copy(color = Hud.dim, fontSize = 28.sp))
                                Spacer(Modifier.height(8.dp))
                                Text(camError ?: "Opening the front camera through nervus.camerad…", style = Hud.value.copy(color = Hud.dim))
                            }
                        }
                        // corner telemetry
                        Column(Modifier.align(Alignment.TopStart).padding(14.dp)) {
                            Text("CAM · FRONT · $camFormat", style = Hud.label.copy(color = Hud.cyan))
                            Text("${"%.0f".format(camFps)} FPS IN · " + if (p.visionIdle) "STANDBY: NOBODY IN VIEW" else "${"%.0f".format(visionFps)} FPS TRACKED", style = Hud.label)
                        }
                        Column(Modifier.align(Alignment.BottomStart).padding(14.dp)) {
                            val o = obs
                            Text("YOLO26N-POSE ${o?.let { "%.1f".format(it.poseMs) } ?: "-"} MS · HAND LANDMARKS ${o?.let { "%.1f".format(it.handMs) } ?: "-"} MS", style = Hud.label)
                            visionError?.let { Text(it, style = Hud.label.copy(color = Hud.red)) }
                        }
                        Text(
                            when (status.phase) {
                                TeleopStatus.Phase.ENGAGED -> "● LIVE CONTROL"
                                TeleopStatus.Phase.HOLD -> "■ HOLD"
                                TeleopStatus.Phase.ERROR -> "▲ ATTENTION"
                                TeleopStatus.Phase.IDLE -> "○ STANDBY"
                            },
                            style = Hud.title.copy(fontSize = 15.sp, color = frameColor),
                            modifier = Modifier.align(Alignment.TopEnd).padding(14.dp),
                        )
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
                    Text("NervusOS · Manipulator@1 · AgileX PiPER", style = Hud.label)
                }
                FlowRow(horizontalArrangement = Arrangement.spacedBy(8.dp), verticalArrangement = Arrangement.spacedBy(8.dp)) {
                    StatusChip("cam", "%.0f fps".format(camFps), camFps > 10f)
                    StatusChip("arm", status.armReadiness.lowercase(), when (status.armReadiness) { "READY" -> true; "-" -> null; else -> false })
                    StatusChip("lease", if (status.leaseEpoch != 0L && engaged) "epoch ${status.leaseEpoch}" else "none", if (engaged) true else null)
                    StatusChip("servo", "%.0f Hz".format(status.servoHz), if (status.phase == TeleopStatus.Phase.ENGAGED) status.servoHz > 12f else null)
                }

                HudPanel(accent = if (engaged) Hud.cyan else Hud.dim) {
                    Row(horizontalArrangement = Arrangement.spacedBy(10.dp)) {
                        HudButton(if (engaged) "DISENGAGE" else "ENGAGE", Hud.cyan, Modifier.weight(1.4f), filled = engaged) {
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
                    HudSlider("sideways", side, 0.1f..0.8f, { "%.0f cm / frame".format(it * 100) }) { side = it; p.settings.gainSide = it.toDouble() }
                    HudSlider("up / down", up, 0.1f..0.8f, { "%.0f cm / frame".format(it * 100) }) { up = it; p.settings.gainUp = it.toDouble() }
                    HudSlider("reach", reach, 0.0f..0.6f, { "%.0f cm / 2x size".format(it * 100) }) { reach = it; p.settings.gainReach = it.toDouble() }
                    HudSlider("arm speed", speed, 0.1f..1.0f, { "%.0f %%".format(it * 100) }) { speed = it; p.settings.speed = it.toDouble() }
                    var inv by remember { mutableStateOf(p.settings.invertSide) }
                    Segmented(listOf(false to "MIRROR ME", true to "FACE ME"), inv) { inv = it; p.settings.invertSide = it }
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

private data class Fit(val left: Float, val top: Float, val width: Float, val height: Float)

private fun fitRect(w: Float, h: Float, area: Size): Fit {
    val s = minOf(area.width / w, area.height / h)
    val fw = w * s
    val fh = h * s
    return Fit((area.width - fw) / 2, (area.height - fh) / 2, fw, fh)
}

/** Frame pixel -> screen, mirrored. */
private fun Fit.map(x: Float, y: Float, fw: Int, fh: Int) =
    Offset(left + width * (1f - x / fw), top + height * (y / fh))

private fun DrawScope.drawSkeleton(o: Observation, fit: Fit, mode: PinchMode, operator: HandLandmarks.Side) {
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
            val open = (ratio / 1.1f).coerceIn(0f, 1f)
            val c = Color(1f - open, 0.35f + 0.6f * open, 0.45f, 1f)
            val a = fit.map(t.x, t.y, fw, fh)
            val b = fit.map(tx, ty, fw, fh)
            drawLine(c, a, b, 3.dp.toPx(), cap = StrokeCap.Round, pathEffect = androidx.compose.ui.graphics.PathEffect.dashPathEffect(floatArrayOf(10f, 8f)))
            drawCircle(c, 9.dp.toPx(), Offset((a.x + b.x) / 2, (a.y + b.y) / 2), style = Stroke(2.dp.toPx()))
        }
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
