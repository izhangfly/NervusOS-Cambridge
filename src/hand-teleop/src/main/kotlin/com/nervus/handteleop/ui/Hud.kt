package com.nervus.handteleop.ui

import androidx.compose.foundation.ExperimentalFoundationApi
import androidx.compose.foundation.TooltipArea
import androidx.compose.foundation.background
import androidx.compose.foundation.border
import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.*
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.material3.Slider
import androidx.compose.material3.SliderDefaults
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.drawBehind
import androidx.compose.ui.geometry.Offset
import androidx.compose.ui.graphics.Brush
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.graphics.drawscope.Stroke
import androidx.compose.ui.text.TextStyle
import androidx.compose.ui.text.font.FontFamily
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.unit.Dp
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp

/** A restrained sci-fi palette: deep navy, one cyan accent, signal colours only where they mean something. */
object Hud {
    val bg = Color(0xFF04070D)
    val panel = Color(0xFF0A1220)
    val panelHi = Color(0xFF0F1A2D)
    val line = Color(0xFF1B2C44)
    val cyan = Color(0xFF39E1FF)
    val cyanDim = Color(0xFF1A6C80)
    val magenta = Color(0xFFFF4FD8)
    val amber = Color(0xFFFFB547)
    val green = Color(0xFF45F5A1)
    val red = Color(0xFFFF4D5E)
    val text = Color(0xFFD9E7F5)
    val dim = Color(0xFF6E829B)
    val mono = FontFamily.Monospace

    val label = TextStyle(color = dim, fontSize = 11.sp, fontFamily = mono, letterSpacing = 1.5.sp)
    val value = TextStyle(color = text, fontSize = 15.sp, fontFamily = mono)
    val title = TextStyle(color = text, fontSize = 20.sp, fontWeight = FontWeight.SemiBold, letterSpacing = 3.sp)
}

/** A panel with corner brackets instead of a full border. */
@Composable
fun HudPanel(modifier: Modifier = Modifier, accent: Color = Hud.cyan, padding: Dp = 14.dp, content: @Composable ColumnScope.() -> Unit) {
    Column(
        modifier
            .background(Hud.panel, RoundedCornerShape(4.dp))
            .drawBehind {
                val l = 14.dp.toPx()
                val w = 2.dp.toPx()
                val c = accent.copy(alpha = 0.85f)
                // four corner brackets
                drawLine(c, Offset(0f, 0f), Offset(l, 0f), w); drawLine(c, Offset(0f, 0f), Offset(0f, l), w)
                drawLine(c, Offset(size.width, 0f), Offset(size.width - l, 0f), w); drawLine(c, Offset(size.width, 0f), Offset(size.width, l), w)
                drawLine(c, Offset(0f, size.height), Offset(l, size.height), w); drawLine(c, Offset(0f, size.height), Offset(0f, size.height - l), w)
                drawLine(c, Offset(size.width, size.height), Offset(size.width - l, size.height), w)
                drawLine(c, Offset(size.width, size.height), Offset(size.width, size.height - l), w)
                drawRect(Hud.line, style = Stroke(1f))
            }
            .padding(padding),
        content = content,
    )
}

@Composable
fun HudLabel(text: String) = Text(text.uppercase(), style = Hud.label)

/** A small readout: label above, value below. */
@Composable
fun Readout(label: String, value: String, color: Color = Hud.text, modifier: Modifier = Modifier) {
    Column(modifier) {
        HudLabel(label)
        Text(value, style = Hud.value.copy(color = color))
    }
}

/** A status pill with a lit dot. */
@Composable
fun StatusChip(label: String, value: String, ok: Boolean?) {
    val c = when (ok) { true -> Hud.green; false -> Hud.red; null -> Hud.amber }
    Row(
        Modifier.background(Hud.panelHi, RoundedCornerShape(3.dp)).border(1.dp, Hud.line, RoundedCornerShape(3.dp))
            .padding(horizontal = 10.dp, vertical = 6.dp),
        verticalAlignment = Alignment.CenterVertically,
    ) {
        Box(Modifier.size(7.dp).background(c, RoundedCornerShape(50)))
        Spacer(Modifier.width(8.dp))
        Text(label.uppercase(), style = Hud.label)
        Spacer(Modifier.width(8.dp))
        Text(value, style = Hud.value.copy(fontSize = 13.sp, color = c))
    }
}

/** A large flat button. */
@Composable
fun HudButton(text: String, color: Color, modifier: Modifier = Modifier, filled: Boolean = false, enabled: Boolean = true, onClick: () -> Unit) {
    val c = if (enabled) color else Hud.dim
    Box(
        modifier
            .background(if (filled) c.copy(alpha = 0.22f) else Hud.panelHi, RoundedCornerShape(4.dp))
            .border(1.5.dp, c, RoundedCornerShape(4.dp))
            .clickable(enabled = enabled, onClick = onClick)
            .padding(vertical = 14.dp, horizontal = 12.dp),
        contentAlignment = Alignment.Center,
    ) {
        Text(text, style = Hud.title.copy(fontSize = 16.sp, color = c))
    }
}

/** [content] with a tooltip saying [text] on hover (for a disabled button: why); just [content] when [text] is null. */
@OptIn(ExperimentalFoundationApi::class)
@Composable
fun Hint(text: String?, content: @Composable () -> Unit) {
    if (text == null) {
        content()
        return
    }
    TooltipArea(tooltip = {
        Box(Modifier.background(Hud.panelHi, RoundedCornerShape(3.dp)).border(1.dp, Hud.line, RoundedCornerShape(3.dp)).padding(8.dp)) {
            Text(text, style = Hud.label.copy(color = Hud.text))
        }
    }) { content() }
}

/** Two-option toggle. */
@Composable
fun <T> Segmented(options: List<Pair<T, String>>, selected: T, onSelect: (T) -> Unit) {
    Row(Modifier.fillMaxWidth().border(1.dp, Hud.line, RoundedCornerShape(4.dp))) {
        options.forEach { (value, label) ->
            val on = value == selected
            Box(
                Modifier.weight(1f).background(if (on) Hud.cyan.copy(alpha = 0.18f) else Hud.panelHi)
                    .clickable { onSelect(value) }.padding(vertical = 9.dp),
                contentAlignment = Alignment.Center,
            ) {
                Text(label, style = Hud.value.copy(fontSize = 12.sp, color = if (on) Hud.cyan else Hud.dim))
            }
        }
    }
}

/** A horizontal gauge, 0..1. */
@Composable
fun Gauge(value: Float, color: Color, modifier: Modifier = Modifier) {
    Box(modifier.height(10.dp).fillMaxWidth().background(Hud.panelHi, RoundedCornerShape(2.dp)).border(1.dp, Hud.line, RoundedCornerShape(2.dp))) {
        Box(
            Modifier.fillMaxHeight().fillMaxWidth(value.coerceIn(0f, 1f))
                .background(Brush.horizontalGradient(listOf(color.copy(alpha = 0.35f), color)), RoundedCornerShape(2.dp))
        )
    }
}

@Composable
fun HudSlider(label: String, value: Float, range: ClosedFloatingPointRange<Float>, format: (Float) -> String, onChange: (Float) -> Unit) {
    Column {
        Row(Modifier.fillMaxWidth(), horizontalArrangement = Arrangement.SpaceBetween) {
            HudLabel(label)
            Text(format(value), style = Hud.value.copy(fontSize = 12.sp, color = Hud.cyan))
        }
        Slider(
            value = value, onValueChange = onChange, valueRange = range,
            colors = SliderDefaults.colors(thumbColor = Hud.cyan, activeTrackColor = Hud.cyan, inactiveTrackColor = Hud.line),
            modifier = Modifier.height(28.dp),
        )
    }
}
