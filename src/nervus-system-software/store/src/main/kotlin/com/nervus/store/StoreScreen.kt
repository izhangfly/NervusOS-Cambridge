package com.nervus.store

import androidx.compose.foundation.background
import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.PaddingValues
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.lazy.grid.GridCells
import androidx.compose.foundation.lazy.grid.LazyVerticalGrid
import androidx.compose.foundation.lazy.grid.items
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.material3.AlertDialog
import androidx.compose.material3.Button
import androidx.compose.material3.Card
import androidx.compose.material3.CardDefaults
import androidx.compose.material3.Icon
import androidx.compose.material3.LinearProgressIndicator
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedButton
import androidx.compose.material3.Scaffold
import androidx.compose.material3.Surface
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.rememberCoroutineScope
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.clip
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import com.nervus.sdk.ui.NervusBackHandler
import com.nervus.sysui.iconForPackage
import com.nervus.sysui.rememberPolled
import io.github.nervusos.iface.pkgmanager.v1.InspectResult
import io.github.nervusos.iface.pkgmanager.v1.PackageInfo
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.launch
import kotlinx.coroutines.withContext

/** What the store can do with one catalog entry right now. */
private enum class EntryState { Get, Update, Installed }

private fun stateOf(entry: CatalogEntry, installed: PackageInfo?): EntryState = when {
    installed == null -> EntryState.Get
    entry.versionCode > installed.versionCode -> EntryState.Update
    else -> EntryState.Installed
}

/** An install that is waiting for the user to accept its permissions. */
private data class PendingConsent(
    val entry: CatalogEntry,
    val relPath: String,
    val inspect: InspectResult,
)

@Composable
fun StoreScreen(store: Store) {
    val catalog by rememberPolled(initial = emptyList<CatalogEntry>(), intervalMs = 10_000) {
        store.catalog()
    }
    val installed by rememberPolled(initial = emptyMap<String, PackageInfo>(), intervalMs = 3000) {
        store.installed()
    }

    var busy by remember { mutableStateOf<String?>(null) }
    var message by remember { mutableStateOf<String?>(null) }
    var consent by remember { mutableStateOf<PendingConsent?>(null) }
    var details by remember { mutableStateOf<CatalogEntry?>(null) }
    var confirmUninstall by remember { mutableStateOf<CatalogEntry?>(null) }
    val scope = rememberCoroutineScope()

    NervusBackHandler(enabled = consent != null || details != null || confirmUninstall != null || message != null) {
        when {
            consent != null -> consent = null
            confirmUninstall != null -> confirmUninstall = null
            details != null -> details = null
            else -> message = null
        }
    }

    fun runTask(label: String, packageId: String, block: suspend () -> Unit) {
        busy = packageId
        message = null
        scope.launch {
            try {
                block()
            } catch (e: Exception) {
                message = "$label failed: ${e.message}"
            } finally {
                busy = null
            }
        }
    }

    // Step 1 and 2 of an install: stage the file and ask the kernel what it wants
    fun beginInstall(entry: CatalogEntry) = runTask("Install", entry.packageId) {
        val (relPath, inspect) = withContext(Dispatchers.IO) {
            val rel = store.stage(entry)
            rel to store.inspect(rel)
        }
        consent = PendingConsent(entry, relPath, inspect)
    }

    // Step 3: the user said yes
    fun finishInstall(p: PendingConsent) = runTask("Install", p.entry.packageId) {
        val info = withContext(Dispatchers.IO) {
            store.install(
                relPath = p.relPath,
                consented = p.inspect.consentPermissionsList.map { it.permissionId },
                manifestDigest = p.inspect.manifestDigest,
            )
        }
        message = "${p.entry.name} ${info.version} installed. It is on the desktop now."
    }

    Scaffold(
        topBar = { StoreHeader(busy = busy != null || catalog.loading) },
        containerColor = MaterialTheme.colorScheme.background,
    ) { padding ->
        Box(Modifier.fillMaxSize().padding(padding)) {
            when {
                catalog.value.isEmpty() && catalog.loading ->
                    CenteredText("Loading the catalog…", null)

                catalog.value.isEmpty() ->
                    CenteredText(
                        "The catalog is empty",
                        "Put packages in ${Catalog.packagesDir()} and list them in ${Catalog.ROOT}/catalog.ini",
                    )

                else -> LazyVerticalGrid(
                    columns = GridCells.Adaptive(minSize = 300.dp),
                    contentPadding = PaddingValues(24.dp),
                    horizontalArrangement = Arrangement.spacedBy(16.dp),
                    verticalArrangement = Arrangement.spacedBy(16.dp),
                    modifier = Modifier.fillMaxSize(),
                ) {
                    items(catalog.value, key = { it.packageId }) { entry ->
                        val info = installed.value[entry.packageId]
                        AppCard(
                            entry = entry,
                            installed = info,
                            state = stateOf(entry, info),
                            busy = busy == entry.packageId,
                            enabled = busy == null,
                            onDetails = { details = entry },
                            onGet = { beginInstall(entry) },
                            onOpen = {
                                runTask("Open", entry.packageId) {
                                    withContext(Dispatchers.IO) { store.open(entry) }
                                }
                            },
                            onUninstall = { confirmUninstall = entry },
                        )
                    }
                }
            }

            if (installed.error != null && installed.value.isEmpty()) {
                Banner(
                    "Can't reach the package manager yet: ${installed.error}",
                    Modifier.align(Alignment.BottomCenter),
                )
            }
            message?.let { Banner(it, Modifier.align(Alignment.BottomCenter)) }
        }
    }

    consent?.let { p ->
        ConsentDialog(
            pending = p,
            onDismiss = { consent = null },
            onConfirm = {
                consent = null
                finishInstall(p)
            },
        )
    }

    details?.let { entry ->
        AlertDialog(
            onDismissRequest = { details = null },
            title = { Text(entry.name) },
            text = {
                Column {
                    Text(
                        listOf(entry.publisher, entry.category, "version ${entry.version}")
                            .filter { it.isNotBlank() }
                            .joinToString(" · "),
                        style = MaterialTheme.typography.bodySmall,
                        color = MaterialTheme.colorScheme.onSurfaceVariant,
                    )
                    Spacer(Modifier.height(12.dp))
                    Text(entry.description.ifBlank { entry.summary })
                    Spacer(Modifier.height(12.dp))
                    Text(
                        entry.packageId,
                        style = MaterialTheme.typography.labelSmall,
                        color = MaterialTheme.colorScheme.onSurfaceVariant,
                    )
                }
            },
            confirmButton = { TextButton(onClick = { details = null }) { Text("Close") } },
        )
    }

    confirmUninstall?.let { entry ->
        AlertDialog(
            onDismissRequest = { confirmUninstall = null },
            title = { Text("Uninstall ${entry.name}?") },
            text = { Text("The app and its private data are removed. You can install it again from the store.") },
            confirmButton = {
                TextButton(onClick = {
                    confirmUninstall = null
                    runTask("Uninstall", entry.packageId) {
                        withContext(Dispatchers.IO) { store.uninstall(entry.packageId) }
                        message = "${entry.name} uninstalled."
                    }
                }) { Text("Uninstall") }
            },
            dismissButton = { TextButton(onClick = { confirmUninstall = null }) { Text("Cancel") } },
        )
    }
}

@Composable
private fun StoreHeader(busy: Boolean) {
    Surface(color = MaterialTheme.colorScheme.surfaceContainer) {
        Column(Modifier.fillMaxWidth()) {
            Row(
                Modifier.fillMaxWidth().padding(horizontal = 24.dp, vertical = 16.dp),
                verticalAlignment = Alignment.CenterVertically,
            ) {
                Column(Modifier.weight(1f)) {
                    Text("App Store", style = MaterialTheme.typography.headlineSmall, fontWeight = FontWeight.SemiBold)
                    Text(
                        "Apps for this robot. Every package is verified by the kernel before it installs.",
                        style = MaterialTheme.typography.bodyMedium,
                        color = MaterialTheme.colorScheme.onSurfaceVariant,
                    )
                }
            }
            if (busy) LinearProgressIndicator(Modifier.fillMaxWidth()) else Spacer(Modifier.height(4.dp))
        }
    }
}

@Composable
private fun AppCard(
    entry: CatalogEntry,
    installed: PackageInfo?,
    state: EntryState,
    busy: Boolean,
    enabled: Boolean,
    onDetails: () -> Unit,
    onGet: () -> Unit,
    onOpen: () -> Unit,
    onUninstall: () -> Unit,
) {
    Card(
        modifier = Modifier.fillMaxWidth().height(210.dp).clickable(onClick = onDetails),
        colors = CardDefaults.cardColors(containerColor = MaterialTheme.colorScheme.surfaceContainerLow),
    ) {
        Column(Modifier.fillMaxSize().padding(16.dp)) {
            Row(verticalAlignment = Alignment.CenterVertically) {
                Box(
                    Modifier.size(52.dp).clip(RoundedCornerShape(14.dp))
                        .background(MaterialTheme.colorScheme.primaryContainer),
                    contentAlignment = Alignment.Center,
                ) {
                    Icon(
                        iconForPackage(entry.packageId),
                        contentDescription = null,
                        tint = MaterialTheme.colorScheme.onPrimaryContainer,
                        modifier = Modifier.size(30.dp),
                    )
                }
                Spacer(Modifier.width(14.dp))
                Column(Modifier.weight(1f)) {
                    Text(entry.name, style = MaterialTheme.typography.titleMedium, fontWeight = FontWeight.SemiBold,
                        maxLines = 1, overflow = TextOverflow.Ellipsis)
                    Text(
                        listOf(entry.publisher, entry.category).filter { it.isNotBlank() }.joinToString(" · "),
                        style = MaterialTheme.typography.bodySmall,
                        color = MaterialTheme.colorScheme.onSurfaceVariant,
                        maxLines = 1,
                    )
                }
            }
            Spacer(Modifier.height(12.dp))
            Text(
                entry.summary,
                style = MaterialTheme.typography.bodyMedium,
                maxLines = 3,
                overflow = TextOverflow.Ellipsis,
                modifier = Modifier.weight(1f),
            )
            Row(verticalAlignment = Alignment.CenterVertically) {
                Text(
                    when (state) {
                        EntryState.Get -> "v${entry.version}"
                        EntryState.Update -> "v${installed?.version} → v${entry.version}"
                        EntryState.Installed -> "Installed · v${installed?.version}"
                    },
                    style = MaterialTheme.typography.labelMedium,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                    modifier = Modifier.weight(1f),
                )
                when {
                    busy -> Text("Working…", style = MaterialTheme.typography.labelLarge)
                    state == EntryState.Get ->
                        Button(onClick = onGet, enabled = enabled) { Text("Get") }
                    state == EntryState.Update ->
                        Button(onClick = onGet, enabled = enabled) { Text("Update") }
                    else -> Row(horizontalArrangement = Arrangement.spacedBy(8.dp)) {
                        OutlinedButton(onClick = onUninstall, enabled = enabled) { Text("Uninstall") }
                        Button(onClick = onOpen, enabled = enabled) { Text("Open") }
                    }
                }
            }
        }
    }
}

@Composable
private fun ConsentDialog(pending: PendingConsent, onDismiss: () -> Unit, onConfirm: () -> Unit) {
    val perms = pending.inspect.consentPermissionsList
    AlertDialog(
        onDismissRequest = onDismiss,
        title = { Text("Install ${pending.entry.name}?") },
        text = {
            Column {
                Text(
                    "${pending.inspect.packageId} · version ${pending.inspect.version}",
                    style = MaterialTheme.typography.bodySmall,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                )
                Spacer(Modifier.height(12.dp))
                if (perms.isEmpty()) {
                    Text("This app doesn't ask for any sensitive permissions.")
                } else {
                    Text("This app asks for:")
                    Spacer(Modifier.height(8.dp))
                    perms.forEach { p ->
                        val name = p.displayName.en.ifEmpty { p.displayName.zhCn.ifEmpty { p.permissionId } }
                        val desc = p.description.en.ifEmpty { p.description.zhCn }
                        Column(Modifier.padding(vertical = 4.dp)) {
                            Text("• $name", fontWeight = FontWeight.Medium)
                            if (desc.isNotBlank()) {
                                Text(
                                    desc,
                                    style = MaterialTheme.typography.bodySmall,
                                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                                    modifier = Modifier.padding(start = 12.dp),
                                )
                            }
                        }
                    }
                }
            }
        },
        confirmButton = { TextButton(onClick = onConfirm) { Text(if (perms.isEmpty()) "Install" else "Allow and install") } },
        dismissButton = { TextButton(onClick = onDismiss) { Text("Cancel") } },
    )
}

@Composable
private fun CenteredText(title: String, detail: String?) {
    Column(
        Modifier.fillMaxSize().padding(48.dp),
        verticalArrangement = Arrangement.Center,
        horizontalAlignment = Alignment.CenterHorizontally,
    ) {
        Text(title, style = MaterialTheme.typography.titleLarge)
        detail?.let {
            Spacer(Modifier.height(8.dp))
            Text(it, style = MaterialTheme.typography.bodyMedium, color = MaterialTheme.colorScheme.onSurfaceVariant)
        }
    }
}

@Composable
private fun Banner(text: String, modifier: Modifier) {
    Surface(
        modifier = modifier.padding(24.dp),
        color = MaterialTheme.colorScheme.inverseSurface,
        shape = RoundedCornerShape(12.dp),
    ) {
        Text(
            text,
            color = MaterialTheme.colorScheme.inverseOnSurface,
            modifier = Modifier.padding(horizontal = 20.dp, vertical = 14.dp),
        )
    }
}
