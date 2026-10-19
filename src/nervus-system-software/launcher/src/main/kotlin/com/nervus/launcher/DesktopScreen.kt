package com.nervus.launcher

import androidx.compose.foundation.background
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.PaddingValues
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.lazy.grid.GridCells
import androidx.compose.foundation.lazy.grid.LazyVerticalGrid
import androidx.compose.foundation.lazy.grid.items
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.material3.Card
import androidx.compose.material3.CardDefaults
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.material3.LinearProgressIndicator
import androidx.compose.foundation.layout.Spacer
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Scaffold
import androidx.compose.material3.Surface
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.rememberCoroutineScope
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.clip
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.style.TextAlign
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import com.nervus.sdk.ui.NervusBackHandler
import com.nervus.sysui.AppIdentity
import com.nervus.sysui.NervusIcons
import com.nervus.sysui.PowerAction
import com.nervus.sysui.PowerConfirmDialog
import com.nervus.sysui.iconForPackage
import com.nervus.sysui.rememberPolled
import io.github.nervusos.iface.pkgmanager.v1.PackageInfo
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.launch
import kotlinx.coroutines.withContext
import java.time.LocalTime
import java.time.format.DateTimeFormatter

@Composable
fun DesktopScreen(desktop: Desktop) {
    // Refetch the app list every 3 seconds. No subscription: nervud doesn't implement Subscribe yet
    // and closes the connection if it gets one (see rememberPolled)
    val apps by rememberPolled<List<PackageInfo>>(initial = emptyList(), intervalMs = 3000) {
        desktop.listApps()
    }

    var launching by remember { mutableStateOf<String?>(null) }
    var launchError by remember { mutableStateOf<String?>(null) }
    // Power confirmation. Never reset once sent: the machine is going down, so UI state no longer matters
    var pendingPower by remember { mutableStateOf<PowerAction?>(null) }
    val scope = rememberCoroutineScope()

    NervusBackHandler(enabled = pendingPower != null || launchError != null) {
        when {
            pendingPower != null -> pendingPower = null
            launchError != null -> launchError = null
        }
    }

    Scaffold(
        topBar = {
            StatusBar(
                busy = apps.loading || launching != null,
                onPower = { pendingPower = it },
            )
        },
        containerColor = MaterialTheme.colorScheme.background,
    ) { padding ->
        Box(Modifier.fillMaxSize().padding(padding)) {
            when {
                apps.value.isEmpty() && apps.error != null ->
                    CenteredMessage(
                        title = "Can't list apps right now",
                        detail = apps.error!!,
                    )

                apps.value.isEmpty() && apps.loading ->
                    CenteredMessage(title = "Reading installed apps…", detail = null)

                apps.value.isEmpty() ->
                    CenteredMessage(
                        title = "No apps to launch yet",
                        detail = "Install one from the App Store or with nervusctl install, or check that pkgmanagerd is running",
                    )

                else -> AppGrid(
                    apps = apps.value,
                    launchingPackageId = launching,
                    onLaunch = { pkg ->
                        launching = pkg
                        launchError = null
                        scope.launch {
                            try {
                                // Resolve blocks until the target component has started and registered,
                                // so this must run off the UI thread
                                withContext(Dispatchers.IO) {
                                    desktop.launch(pkg, AppIdentity.launchComponentOf(pkg))
                                }
                            } catch (e: Exception) {
                                launchError = "${AppIdentity.displayName(pkg)}: ${e.message}"
                            } finally {
                                launching = null
                            }
                        }
                    },
                )
            }

            launchError?.let { msg ->
                ErrorBanner(msg, Modifier.align(Alignment.BottomCenter))
            }
        }
    }

    pendingPower?.let { action ->
        PowerConfirmDialog(
            action = action,
            onDismiss = { pendingPower = null },
            onConfirm = {
                pendingPower = null
                scope.launch {
                    // Blocks until the connection drops, so it must run off the UI thread
                    withContext(Dispatchers.IO) {
                        // A failure here is most likely "connection closed", which is what success looks like.
                        // The desktop has no good place to explain that, so stay quiet:
                        // if it really didn't shut down, the user sees the desktop is still there and presses again
                        runCatching { desktop.power(action) }
                    }
                }
            },
        )
    }
}

@Composable
private fun StatusBar(busy: Boolean, onPower: (PowerAction) -> Unit) {
    Surface(
        color = MaterialTheme.colorScheme.surface,
        tonalElevation = 2.dp,
    ) {
        Column {
            Row(
                Modifier.fillMaxWidth().padding(horizontal = 28.dp, vertical = 10.dp),
                verticalAlignment = Alignment.CenterVertically,
                horizontalArrangement = Arrangement.SpaceBetween,
            ) {
                Row(
                    verticalAlignment = Alignment.CenterVertically,
                    horizontalArrangement = Arrangement.spacedBy(10.dp),
                ) {
                    Surface(
                        color = MaterialTheme.colorScheme.primaryContainer,
                        shape = RoundedCornerShape(12.dp),
                    ) {
                        Icon(
                            imageVector = NervusIcons.Apps,
                            contentDescription = null,
                            tint = MaterialTheme.colorScheme.onPrimaryContainer,
                            modifier = Modifier.padding(8.dp).size(22.dp),
                        )
                    }
                    Text(
                        "Nervus",
                        style = MaterialTheme.typography.titleLarge,
                        fontWeight = FontWeight.SemiBold,
                        color = MaterialTheme.colorScheme.onSurface,
                    )
                }
                Row(
                    verticalAlignment = Alignment.CenterVertically,
                    horizontalArrangement = Arrangement.spacedBy(4.dp),
                ) {
                    Clock()
                    IconButton(onClick = { onPower(PowerAction.Reboot) }) {
                        Icon(
                            imageVector = NervusIcons.Restart,
                            contentDescription = PowerAction.Reboot.label,
                            tint = MaterialTheme.colorScheme.onSurfaceVariant,
                        )
                    }
                    IconButton(onClick = { onPower(PowerAction.PowerOff) }) {
                        Icon(
                            imageVector = NervusIcons.Power,
                            contentDescription = PowerAction.PowerOff.label,
                            tint = MaterialTheme.colorScheme.error,
                        )
                    }
                }
            }
            // The bar's 2 dp always keep their place, so the grid never jumps; but an indeterminate indicator animates
            // forever even when transparent, and that repainted the whole desktop 60 times a second (half a core in
            // software rendering). So it exists only while busy, with a spacer of the same height otherwise.
            if (busy) {
                LinearProgressIndicator(
                    modifier = Modifier.fillMaxWidth().height(2.dp),
                    color = MaterialTheme.colorScheme.primary,
                    trackColor = Color.Transparent,
                )
            } else {
                Spacer(Modifier.fillMaxWidth().height(2.dp))
            }
        }
    }
}

@Composable
private fun Clock() {
    // Minutes, not seconds: every change repaints the window, and a seconds display cost a repaint a second.
    // The poll still runs every second, but the value only changes (and only repaints) when the minute does.
    val now by rememberPolled(initial = LocalTime.now().withSecond(0).withNano(0), intervalMs = 1000) {
        LocalTime.now().withSecond(0).withNano(0)
    }
    Text(
        now.value.format(TIME_FORMAT),
        style = MaterialTheme.typography.titleMedium,
        color = MaterialTheme.colorScheme.onSurfaceVariant,
    )
}

private val TIME_FORMAT: DateTimeFormatter = DateTimeFormatter.ofPattern("HH:mm")

@Composable
private fun AppGrid(
    apps: List<PackageInfo>,
    launchingPackageId: String?,
    onLaunch: (String) -> Unit,
) {
    LazyVerticalGrid(
        columns = GridCells.Adaptive(minSize = 150.dp),
        contentPadding = PaddingValues(28.dp),
        horizontalArrangement = Arrangement.spacedBy(20.dp),
        verticalArrangement = Arrangement.spacedBy(20.dp),
        modifier = Modifier.fillMaxSize(),
    ) {
        items(apps, key = { it.packageId }) { info ->
            AppTile(
                info = info,
                launching = info.packageId == launchingPackageId,
                onClick = { onLaunch(info.packageId) },
            )
        }
    }
}

@Composable
private fun AppTile(info: PackageInfo, launching: Boolean, onClick: () -> Unit) {
    val name = AppIdentity.displayName(info.packageId)
    Card(
        onClick = onClick,
        enabled = !launching,
        colors = CardDefaults.cardColors(
            containerColor = MaterialTheme.colorScheme.surface,
        ),
        elevation = CardDefaults.cardElevation(
            defaultElevation = 1.dp,
            hoveredElevation = 4.dp,
            pressedElevation = 2.dp,
        ),
        shape = RoundedCornerShape(24.dp),
        modifier = Modifier.height(160.dp),
    ) {
        Column(
            Modifier.fillMaxSize().padding(18.dp),
            horizontalAlignment = Alignment.CenterHorizontally,
            verticalArrangement = Arrangement.Center,
        ) {
            AppIcon(packageId = info.packageId, dimmed = launching)
            Text(
                name,
                style = MaterialTheme.typography.titleMedium,
                color = MaterialTheme.colorScheme.onSurface,
                maxLines = 1,
                overflow = TextOverflow.Ellipsis,
                textAlign = TextAlign.Center,
                modifier = Modifier.padding(top = 12.dp),
            )
            // The UI must tell system packages from third-party ones: the source is the first thing to check when debugging
            if (info.source == SYSTEM_IMAGE_SOURCE) {
                Text(
                    "System",
                    style = MaterialTheme.typography.labelSmall,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                )
            }
        }
    }
}

private const val SYSTEM_IMAGE_SOURCE = "system-image"

/**
 * App icon.
 *
 * Built-in apps get stable system icons; others show a generic app icon. `PackageInfo` has
 * no icon field yet, and the manifest's icon is inside the package (under the sandbox the desktop
 * can't read another package's directory). Once pkgmanager projects real icons, replace this mapping.
 *
 * The colour is derived from package_id, so an app keeps its colour across boots and users can spot it by colour.
 */
@Composable
private fun AppIcon(packageId: String, dimmed: Boolean) {
    val palette = MaterialTheme.colorScheme
    val colors = listOf(
        palette.primaryContainer,
        palette.secondaryContainer,
        palette.tertiaryContainer,
    )
    val onColors = listOf(
        palette.onPrimaryContainer,
        palette.onSecondaryContainer,
        palette.onTertiaryContainer,
    )
    // hashCode can be negative; Math.floorMod keeps it in [0, size)
    val idx = Math.floorMod(packageId.hashCode(), colors.size)

    Box(
        Modifier.size(64.dp).clip(RoundedCornerShape(20.dp)).background(colors[idx]),
        contentAlignment = Alignment.Center,
    ) {
        Icon(
            imageVector = iconForPackage(packageId),
            contentDescription = null,
            tint = if (dimmed) onColors[idx].copy(alpha = 0.4f) else onColors[idx],
            modifier = Modifier.size(34.dp),
        )
    }
}

@Composable
private fun CenteredMessage(title: String, detail: String?) {
    Box(Modifier.fillMaxSize(), contentAlignment = Alignment.Center) {
        Column(
            horizontalAlignment = Alignment.CenterHorizontally,
            verticalArrangement = Arrangement.spacedBy(8.dp),
        ) {
            Icon(
                imageVector = NervusIcons.Apps,
                contentDescription = null,
                tint = MaterialTheme.colorScheme.onSurfaceVariant,
                modifier = Modifier.size(44.dp),
            )
            Text(
                title,
                style = MaterialTheme.typography.titleMedium,
                color = MaterialTheme.colorScheme.onBackground,
            )
            detail?.let {
                Text(
                    it,
                    style = MaterialTheme.typography.bodyMedium,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                    textAlign = TextAlign.Center,
                    modifier = Modifier.padding(horizontal = 32.dp),
                )
            }
        }
    }
}

@Composable
private fun ErrorBanner(message: String, modifier: Modifier = Modifier) {
    Surface(
        color = MaterialTheme.colorScheme.errorContainer,
        modifier = modifier.fillMaxWidth().padding(16.dp),
        shape = MaterialTheme.shapes.medium,
    ) {
        Text(
            message,
            style = MaterialTheme.typography.bodyMedium,
            color = MaterialTheme.colorScheme.onErrorContainer,
            modifier = Modifier.padding(16.dp),
        )
    }
}
