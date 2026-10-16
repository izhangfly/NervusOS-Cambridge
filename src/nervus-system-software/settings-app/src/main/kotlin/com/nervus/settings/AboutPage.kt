package com.nervus.settings

import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.verticalScroll
import androidx.compose.material3.Card
import androidx.compose.material3.CardDefaults
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Modifier
import androidx.compose.ui.text.font.FontFamily
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.unit.dp

/**
 * The About page. Tapping the version number [TAPS_TO_UNLOCK] times unlocks developer options, the same
 * gesture as Android, so nobody needs to be taught it.
 */
@Composable
fun AboutPage(
    developerUnlocked: Boolean,
    onUnlock: () -> Unit,
) {
    // The tap count only lives while the page is visible: leave About and come back and it starts again.
    // The unlocked state itself is held by the caller (hoisted to SettingsScreen), or switching tabs would lose it
    var taps by remember { mutableStateOf(0) }

    Column(
        Modifier.fillMaxSize().verticalScroll(rememberScrollState()).padding(32.dp),
        verticalArrangement = Arrangement.spacedBy(20.dp),
    ) {
        Text("Nervus OS", style = MaterialTheme.typography.headlineMedium)

        Card(colors = CardDefaults.cardColors(MaterialTheme.colorScheme.surfaceVariant)) {
            Column(Modifier.padding(20.dp), verticalArrangement = Arrangement.spacedBy(10.dp)) {
                InfoRow("Platform API level", SystemInfo.API_LEVEL.toString())
                InfoRow("Control plane", SystemInfo.CONTROL_SOCKET)

                // The version row is the unlock entry point. The whole row is tappable, not just the number: a finger
                // misses a small number, and the whole point of this gesture is that it can be found by accident
                Row(
                    Modifier.fillMaxWidth().clickable(enabled = !developerUnlocked) {
                        taps++
                        if (taps >= TAPS_TO_UNLOCK) {
                            taps = 0
                            onUnlock()
                        }
                    },
                    horizontalArrangement = Arrangement.SpaceBetween,
                ) {
                    Text("System version", style = MaterialTheme.typography.bodyMedium)
                    Text(
                        SystemInfo.VERSION,
                        style = MaterialTheme.typography.bodyMedium,
                        fontWeight = FontWeight.Medium,
                    )
                }

                // Only hint when it's nearly there. Saying "7 more taps" straight away turns an easter egg into a manual;
                // saying nothing makes people think it's broken after three taps
                val remaining = TAPS_TO_UNLOCK - taps
                if (!developerUnlocked && taps in HINT_FROM until TAPS_TO_UNLOCK) {
                    Text(
                        "$remaining more taps to enable developer options",
                        style = MaterialTheme.typography.bodySmall,
                        color = MaterialTheme.colorScheme.primary,
                    )
                }
                if (developerUnlocked) {
                    Text(
                        "Developer options are on",
                        style = MaterialTheme.typography.bodySmall,
                        color = MaterialTheme.colorScheme.primary,
                    )
                }
            }
        }

        Text(
            "An operating system for robots. The nervud kernel handles safety, identity, permissions and component lifecycles; " +
                "apps and services talk to it over the control-plane IPC.",
            style = MaterialTheme.typography.bodyMedium,
            color = MaterialTheme.colorScheme.onSurfaceVariant,
        )
    }
}

@Composable
private fun InfoRow(label: String, value: String) {
    Row(Modifier.fillMaxWidth(), horizontalArrangement = Arrangement.SpaceBetween) {
        Text(label, style = MaterialTheme.typography.bodyMedium)
        Text(
            value,
            style = MaterialTheme.typography.bodyMedium,
            fontFamily = FontFamily.Monospace,
            color = MaterialTheme.colorScheme.onSurfaceVariant,
        )
    }
}

private const val TAPS_TO_UNLOCK = 6
private const val HINT_FROM = 3

/**
 * System identification.
 *
 * **These values are compile-time constants for now.** The kernel has no way to project its version over IPC:
 * `PackageInfo` carries each package's version but not the system's own. Showing the real value needs a
 * system-information interface (or a new pkgmanager method). That's v2.
 *
 * `API_LEVEL` must equal the kernel's `pkgregistry.CurrentAPILevel`: a manifest whose
 * `min_nervus_api` is higher won't install, so a wrong number here would send people debugging a fake value.
 */
object SystemInfo {
    const val API_LEVEL = 1
    const val VERSION = "0.1.0"
    const val CONTROL_SOCKET = "/run/nervus/nervud.sock"
}
