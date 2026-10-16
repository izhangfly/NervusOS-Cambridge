package com.nervus.settings

import androidx.compose.foundation.background
import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxHeight
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.material3.AlertDialog
import androidx.compose.material3.AssistChip
import androidx.compose.material3.AssistChipDefaults
import androidx.compose.material3.Card
import androidx.compose.material3.CardDefaults
import androidx.compose.material3.HorizontalDivider
import androidx.compose.material3.Icon
import androidx.compose.material3.ListItem
import androidx.compose.material3.ListItemDefaults
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.NavigationRail
import androidx.compose.material3.NavigationRailItem
import androidx.compose.material3.Scaffold
import androidx.compose.material3.Surface
import androidx.compose.material3.Switch
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateListOf
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.rememberCoroutineScope
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.clip
import androidx.compose.ui.graphics.vector.ImageVector
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.unit.dp
import com.nervus.sdk.ui.NervusBackHandler
import com.nervus.sysui.AppIdentity
import com.nervus.sysui.NervusIcons
import com.nervus.sysui.iconForPackage
import com.nervus.sysui.rememberPolled
import io.github.nervusos.iface.pkgmanager.v1.PackageInfo
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.launch
import kotlinx.coroutines.withContext

private enum class Page(val label: String, val icon: ImageVector) {
    Packages("Installed apps", NervusIcons.Apps),
    Permissions("Permissions", NervusIcons.Shield),
    Power("Power", NervusIcons.Power),
    About("About", NervusIcons.Info),
    Developer("Developer", NervusIcons.DeveloperMode),
}

@Composable
fun SettingsScreen(settings: Settings) {
    var page by remember { mutableStateOf(Page.Packages) }
    val pageHistory = remember { mutableStateListOf<Page>() }
    // The unlocked state is hoisted here rather than kept in About; otherwise switching pages would lose it
    // and the user would have to tap six times again
    var developerUnlocked by remember { mutableStateOf(false) }

    fun navigateTo(target: Page) {
        if (target == page) return
        pageHistory += page
        page = target
    }

    NervusBackHandler(enabled = pageHistory.isNotEmpty()) {
        page = pageHistory.removeAt(pageHistory.lastIndex)
    }

    Row(Modifier.fillMaxSize().background(MaterialTheme.colorScheme.background)) {
        NavigationRail(
            containerColor = MaterialTheme.colorScheme.surface,
            header = {
                Text(
                    "Settings",
                    style = MaterialTheme.typography.titleMedium,
                    fontWeight = FontWeight.SemiBold,
                    modifier = Modifier.padding(vertical = 20.dp),
                )
            },
        ) {
            // The developer entry [doesn't appear] until unlocked. Showing it greyed out would tell users it exists,
            // and then it isn't an easter egg any more
            Page.entries.filter { it != Page.Developer || developerUnlocked }.forEach { p ->
                NavigationRailItem(
                    selected = page == p,
                    onClick = { navigateTo(p) },
                    icon = {
                        Icon(
                            imageVector = p.icon,
                            contentDescription = p.label,
                        )
                    },
                    label = { Text(p.label) },
                    alwaysShowLabel = true,
                )
            }
        }
        Box(Modifier.fillMaxSize()) {
            when (page) {
                Page.Packages -> PackagesPage(settings)
                Page.Permissions -> PermissionsPage(settings)
                Page.Power -> PowerPage(settings)
                Page.About -> AboutPage(
                    developerUnlocked = developerUnlocked,
                    onUnlock = {
                        developerUnlocked = true
                        navigateTo(Page.Developer)
                    },
                )
                Page.Developer -> DeveloperPage(settings)
            }
        }
    }
}

@Composable
private fun PackagesPage(settings: Settings) {
    // Poll every 5 seconds. After disable/uninstall there's no manual refresh; wait for the next round, so the UI
    // always shows the kernel's actual state rather than an optimistic "I think that worked". The difference shows on failure:
    // an optimistic update would make a refused disable look successful
    val packages by rememberPolled<List<PackageInfo>>(emptyList(), intervalMs = 5000) {
        settings.listPackages()
    }

    var selected by remember { mutableStateOf<String?>(null) }
    var actionError by remember { mutableStateOf<String?>(null) }
    var confirmUninstall by remember { mutableStateOf<String?>(null) }
    val scope = rememberCoroutineScope()

    NervusBackHandler(
        enabled = confirmUninstall != null || actionError != null || selected != null,
    ) {
        when {
            confirmUninstall != null -> confirmUninstall = null
            actionError != null -> actionError = null
            selected != null -> selected = null
        }
    }

    fun run(block: () -> Unit) {
        scope.launch {
            actionError = null
            try {
                withContext(Dispatchers.IO) { block() }
            } catch (e: Exception) {
                // Show the kernel's refusal reason as-is: system packages can't be uninstalled, the component is protected...
                // The UI doesn't restate or rewrite it, because the authoritative decision is made over there
                actionError = e.message ?: "Operation failed"
            }
        }
    }

    Scaffold(containerColor = MaterialTheme.colorScheme.background) { pad ->
        Row(Modifier.fillMaxSize().padding(pad)) {
            Column(Modifier.width(360.dp).fillMaxHeight()) {
                Text(
                    "Installed apps",
                    style = MaterialTheme.typography.titleLarge,
                    fontWeight = FontWeight.SemiBold,
                    modifier = Modifier.padding(20.dp),
                    color = MaterialTheme.colorScheme.onBackground,
                )
                when {
                    packages.value.isEmpty() && packages.error != null ->
                        Message("Can't list apps", packages.error)
                    packages.value.isEmpty() && packages.loading ->
                        Message("Loading…", null)
                    packages.value.isEmpty() ->
                        Message("Nothing installed", null)
                    else -> LazyColumn {
                        items(packages.value, key = { it.packageId }) { info ->
                            ListItem(
                                leadingContent = {
                                    PackageGlyph(
                                        packageId = info.packageId,
                                        selected = selected == info.packageId,
                                    )
                                },
                                headlineContent = { Text(AppIdentity.displayName(info.packageId)) },
                                supportingContent = {
                                    Text(
                                        "${info.packageId} · ${info.version}",
                                        style = MaterialTheme.typography.bodySmall,
                                    )
                                },
                                trailingContent = { SourceChip(info.source) },
                                colors = ListItemDefaults.colors(
                                    containerColor = if (selected == info.packageId)
                                        MaterialTheme.colorScheme.surfaceVariant
                                    else MaterialTheme.colorScheme.background
                                ),
                                modifier = Modifier.clickableRow { selected = info.packageId },
                            )
                            HorizontalDivider(color = MaterialTheme.colorScheme.outlineVariant)
                        }
                    }
                }
            }

            Surface(
                color = MaterialTheme.colorScheme.surface,
                modifier = Modifier.fillMaxSize(),
            ) {
                val info = packages.value.firstOrNull { it.packageId == selected }
                if (info == null) {
                    Message("Pick an item on the left to see its details", null)
                } else {
                    PackageDetail(
                        info = info,
                        error = actionError,
                        onToggleComponent = { comp, enabled ->
                            run { settings.setComponentEnabled(info.packageId, comp, enabled) }
                        },
                        onUninstall = { confirmUninstall = info.packageId },
                    )
                }
            }
        }
    }

    confirmUninstall?.let { pkg ->
        AlertDialog(
            onDismissRequest = { confirmUninstall = null },
            title = { Text("Uninstall ${AppIdentity.displayName(pkg)}?") },
            text = { Text("$pkg and its data will be removed. This can't be undone.") },
            confirmButton = {
                TextButton(onClick = {
                    confirmUninstall = null
                    selected = null
                    run { settings.uninstall(pkg) }
                }) { Text("Uninstall") }
            },
            dismissButton = {
                TextButton(onClick = { confirmUninstall = null }) { Text("Cancel") }
            },
        )
    }
}

@Composable
private fun PackageDetail(
    info: PackageInfo,
    error: String?,
    onToggleComponent: (String, Boolean) -> Unit,
    onUninstall: () -> Unit,
) {
    Column(Modifier.fillMaxSize().padding(24.dp), verticalArrangement = Arrangement.spacedBy(16.dp)) {
        Row(
            verticalAlignment = Alignment.CenterVertically,
            horizontalArrangement = Arrangement.spacedBy(14.dp),
        ) {
            PackageGlyph(packageId = info.packageId, selected = true, size = 48)
            Column {
                Text(
                    AppIdentity.displayName(info.packageId),
                    style = MaterialTheme.typography.headlineSmall,
                    color = MaterialTheme.colorScheme.onSurface,
                )
                Text(
                    info.packageId,
                    style = MaterialTheme.typography.bodySmall,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                )
            }
        }
        Row(horizontalArrangement = Arrangement.spacedBy(8.dp)) {
            SourceChip(info.source)
            AssistChip(onClick = {}, label = { Text("trust: ${info.trust}") })
            AssistChip(onClick = {}, label = { Text("v${info.version} (${info.versionCode})") })
        }

        if (info.grantedPermissionsCount > 0) {
            Card(colors = CardDefaults.cardColors(MaterialTheme.colorScheme.surfaceVariant)) {
                Column(Modifier.padding(16.dp)) {
                    Text("Granted permissions", style = MaterialTheme.typography.titleSmall)
                    info.grantedPermissionsList.forEach {
                        Text(it, style = MaterialTheme.typography.bodySmall)
                    }
                }
            }
        }

        // Component switches.
        //
        // PackageInfo only reports the IDs of [disabled] components, not the full list, so all we can do here
        // is list the disabled ones so they can be re-enabled. Showing every component with its own switch needs
        // PackageInfo to project components[] (the same gap as label/icon)
        if (info.disabledComponentsCount > 0) {
            Card(colors = CardDefaults.cardColors(MaterialTheme.colorScheme.surfaceVariant)) {
                Column(Modifier.padding(16.dp)) {
                    Text("Disabled components", style = MaterialTheme.typography.titleSmall)
                    info.disabledComponentsList.forEach { comp ->
                        Row(
                            Modifier.fillMaxWidth().padding(vertical = 4.dp),
                            verticalAlignment = Alignment.CenterVertically,
                            horizontalArrangement = Arrangement.SpaceBetween,
                        ) {
                            Text(comp, style = MaterialTheme.typography.bodyMedium)
                            Switch(
                                checked = false,
                                onCheckedChange = { onToggleComponent(comp, true) },
                            )
                        }
                    }
                }
            }
        }

        error?.let {
            Surface(
                color = MaterialTheme.colorScheme.errorContainer,
                shape = MaterialTheme.shapes.medium,
            ) {
                Text(
                    it,
                    color = MaterialTheme.colorScheme.onErrorContainer,
                    style = MaterialTheme.typography.bodyMedium,
                    modifier = Modifier.padding(16.dp),
                )
            }
        }

        Box(Modifier.fillMaxSize(), contentAlignment = Alignment.BottomStart) {
            // The uninstall button isn't greyed out for system packages: let the kernel give the authoritative answer
            // (ErrSystemPackageImmutable) and show it as-is. Guessing here would create a second source of truth
            TextButton(onClick = onUninstall) { Text("Uninstall") }
        }
    }
}

@Composable
private fun PackageGlyph(
    packageId: String,
    selected: Boolean,
    size: Int = 40,
) {
    Box(
        modifier = Modifier
            .size(size.dp)
            .clip(RoundedCornerShape((size / 3).dp))
            .background(
                if (selected) MaterialTheme.colorScheme.primaryContainer
                else MaterialTheme.colorScheme.surfaceVariant,
            ),
        contentAlignment = Alignment.Center,
    ) {
        Icon(
            imageVector = iconForPackage(packageId),
            contentDescription = null,
            tint = if (selected) {
                MaterialTheme.colorScheme.onPrimaryContainer
            } else {
                MaterialTheme.colorScheme.onSurfaceVariant
            },
            modifier = Modifier.size((size * 0.56f).dp),
        )
    }
}

@Composable
private fun SourceChip(source: String) {
    val isSystem = source == "system-image"
    AssistChip(
        onClick = {},
        label = { Text(if (isSystem) "System" else "Installed") },
        colors = if (isSystem) {
            AssistChipDefaults.assistChipColors(
                containerColor = MaterialTheme.colorScheme.secondaryContainer,
                labelColor = MaterialTheme.colorScheme.onSecondaryContainer,
            )
        } else AssistChipDefaults.assistChipColors(),
    )
}

@Composable
private fun Message(title: String, detail: String?) {
    Box(Modifier.fillMaxSize(), contentAlignment = Alignment.Center) {
        Column(horizontalAlignment = Alignment.CenterHorizontally) {
            Text(title, style = MaterialTheme.typography.titleMedium)
            detail?.let {
                Text(
                    it,
                    style = MaterialTheme.typography.bodySmall,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                    modifier = Modifier.padding(top = 8.dp),
                )
            }
        }
    }
}

/** ListItem has no onClick parameter, so add it through Modifier */
private fun Modifier.clickableRow(onClick: () -> Unit): Modifier = this.clickable(onClick = onClick)
