package com.nervus.settings

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
import androidx.compose.material3.Surface
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.ui.Modifier
import androidx.compose.ui.text.font.FontFamily
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.unit.dp
import com.nervus.sysui.rememberPolled
import io.github.nervusos.iface.pkgmanager.v1.PackageInfo

/**
 * Developer options.
 *
 * ## Why this page has [diagnostics only, no switches]
 *
 * The kernel's developer switches (`allow_unverified_signature` / `allow_downgrade` /
 * `skip_oem_countersign`) live in `/var/lib/nervus/registry/_devmode.json`,
 * and that directory is `InaccessiblePaths` for every component, so Settings can neither read nor write it.
 * **That's intended**: switches that change package admission shouldn't be held by a GUI app.
 *
 * Changing them takes `nervusctl` (over the root-only admin channel). So this page lays out
 * the kernel's real state as it is now, instead of showing switches that do nothing when pressed. Those would be
 * worse than nothing: people would think they had turned something off.
 *
 * Real switches can go here once the kernel offers a management interface for them.
 */
@Composable
fun DeveloperPage(settings: Settings) {
    val packages by rememberPolled<List<PackageInfo>>(emptyList(), intervalMs = 5000) {
        settings.listPackages()
    }

    Column(
        Modifier.fillMaxSize().verticalScroll(rememberScrollState()).padding(28.dp),
        verticalArrangement = Arrangement.spacedBy(18.dp),
    ) {
        Text("Developer options", style = MaterialTheme.typography.headlineSmall)

        // ---- Package counts: shows at a glance whether trust levels are working ----
        val bySource = packages.value.groupingBy { it.source }.eachCount()
        val byTrust = packages.value.groupingBy { it.trust }.eachCount()

        Card(colors = CardDefaults.cardColors(MaterialTheme.colorScheme.surfaceVariant)) {
            Column(Modifier.padding(20.dp), verticalArrangement = Arrangement.spacedBy(8.dp)) {
                Text("Installed packages", style = MaterialTheme.typography.titleSmall)
                Kv("Total", packages.value.size.toString())
                bySource.forEach { (k, v) -> Kv("Source $k", v.toString()) }
                byTrust.forEach { (k, v) -> Kv("trust $k", v.toString()) }
            }
        }

        // Every package at trust ordinary means the trust root isn't set up. It's the easiest mistake to miss on a
        // production image and the most expensive: trust levels stop working across the machine while everything keeps running
        if (packages.value.isNotEmpty() && byTrust.keys.all { it == "ordinary" }) {
            Warning(
                "Every package has trust ordinary",
                "Platform signatures weren't verified: either this is a development build (nervud has no embedded platform root), " +
                    "or the bundle in /usr/share/nervus/trust is misconfigured. " +
                    "On a production image this means trust levels are broken for the whole machine.",
            )
        }

        Warning(
            "Permission enforcement is off",
            "The kernel has permission.V1GrantAll = true: whatever a manifest asks for is granted, " +
                "trust floors are ignored and nothing asks the user at run time. The permissions listed under each package below are " +
                "\"asked for, therefore granted\", and say nothing about what it would get once enforcement is back on.",
        )

        // ---- The full permission projection, package by package ----
        Card(colors = CardDefaults.cardColors(MaterialTheme.colorScheme.surfaceVariant)) {
            Column(Modifier.padding(20.dp), verticalArrangement = Arrangement.spacedBy(12.dp)) {
                Text("Permission projection (the kernel's decision)", style = MaterialTheme.typography.titleSmall)
                if (packages.value.isEmpty()) {
                    Text("(can't read the package list)", style = MaterialTheme.typography.bodySmall)
                }
                packages.value.forEach { info ->
                    Column {
                        Text(
                            info.packageId,
                            style = MaterialTheme.typography.bodyMedium,
                            fontWeight = FontWeight.Medium,
                        )
                        Text(
                            if (info.grantedPermissionsCount == 0) "(none)"
                            else info.grantedPermissionsList.joinToString("  "),
                            style = MaterialTheme.typography.bodySmall,
                            fontFamily = FontFamily.Monospace,
                            color = MaterialTheme.colorScheme.onSurfaceVariant,
                        )
                        if (info.disabledComponentsCount > 0) {
                            Text(
                                "Disabled components: ${info.disabledComponentsList.joinToString(", ")}",
                                style = MaterialTheme.typography.bodySmall,
                                color = MaterialTheme.colorScheme.error,
                            )
                        }
                    }
                }
            }
        }

        // ---- Kernel switches: say where they live, don't pretend they can be changed here ----
        Card(colors = CardDefaults.cardColors(MaterialTheme.colorScheme.surfaceVariant)) {
            Column(Modifier.padding(20.dp), verticalArrangement = Arrangement.spacedBy(8.dp)) {
                Text("Kernel developer switches", style = MaterialTheme.typography.titleSmall)
                Text(
                    "Stored in /var/lib/nervus/registry/_devmode.json. That directory is " +
                        "inaccessible to every component (sandbox InaccessiblePaths), so Settings can't read or change it.",
                    style = MaterialTheme.typography.bodySmall,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                )
                Kv("allow_unverified_signature", "see nervusctl")
                Kv("allow_downgrade", "see nervusctl")
                Kv("skip_oem_countersign", "see nervusctl")
            }
        }
    }
}

@Composable
private fun Kv(k: String, v: String) {
    Row(Modifier.fillMaxWidth(), horizontalArrangement = Arrangement.SpaceBetween) {
        Text(k, style = MaterialTheme.typography.bodySmall)
        Text(
            v,
            style = MaterialTheme.typography.bodySmall,
            fontFamily = FontFamily.Monospace,
            color = MaterialTheme.colorScheme.onSurfaceVariant,
        )
    }
}

@Composable
private fun Warning(title: String, detail: String) {
    Surface(
        color = MaterialTheme.colorScheme.errorContainer,
        shape = MaterialTheme.shapes.medium,
        modifier = Modifier.fillMaxWidth(),
    ) {
        Column(Modifier.padding(16.dp), verticalArrangement = Arrangement.spacedBy(4.dp)) {
            Text(
                title,
                style = MaterialTheme.typography.titleSmall,
                color = MaterialTheme.colorScheme.onErrorContainer,
            )
            Text(
                detail,
                style = MaterialTheme.typography.bodySmall,
                color = MaterialTheme.colorScheme.onErrorContainer,
            )
        }
    }
}
