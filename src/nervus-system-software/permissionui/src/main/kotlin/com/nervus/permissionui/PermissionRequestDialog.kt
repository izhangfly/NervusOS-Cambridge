package com.nervus.permissionui

import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.verticalScroll
import androidx.compose.material3.AlertDialog
import androidx.compose.material3.Checkbox
import androidx.compose.material3.Icon
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Surface
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.runtime.Composable
import androidx.compose.runtime.mutableStateMapOf
import androidx.compose.runtime.remember
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.unit.dp
import com.nervus.sysui.NervusIcons
import io.github.nervusos.iface.permission.v1.PermissionGrant
import io.github.nervusos.ipc.v1.RiskClass

/**
 * The permission request dialog.
 *
 * # Security design: three things must hold
 *
 * **1. The requester's identity comes from the kernel.** [PendingRequest.callerPackageId] is what nervud worked out
 * from the connection's credentials, not what the request claims. Show that rather than the app's own name, or one package
 * could dress its request up as another package's.
 *
 * **2. The rationale is untrusted text.** [PendingRequest.rationale] comes from the app, so it is labelled
 * "The app says" and sits in a container that looks different from system text. Otherwise
 * "The system requires you to grant camera access" would make users think the system said it.
 *
 * **3. Nothing is ticked by default, and every exit other than "Allow" means deny.** Closing the dialog, Esc and timeout
 * all count as deny. Pre-ticking would agree to half of it on the user's behalf, which is exactly what this dialog prevents.
 *
 * # All permission text comes from the kernel
 *
 * Names, descriptions and risk levels are in [PermissionGrant] (taken from the permission definitions in the Catalog).
 * The UI hard-codes none of it: third-party packages can define their own permissions, and the UI can't know their text in advance.
 */
@Composable
fun PermissionRequestDialog(app: PermissionUi, request: PendingRequest) {
    // Nothing ticked by default. Fail closed: the user has to agree to each one actively
    val checked = remember(request) { mutableStateMapOf<String, Boolean>() }

    // Close / Esc / clicking outside all come here: deny everything
    val deny = { app.resolvePendingRequest(emptySet()) }

    AlertDialog(
        onDismissRequest = deny,
        icon = {
            Icon(
                imageVector = NervusIcons.Shield,
                contentDescription = null,
                modifier = Modifier.size(28.dp),
            )
        },
        title = {
            // The title shows the requester the kernel identified; fall back to the package ID only if the label is missing
            Text("${request.callerLabel.ifEmpty { request.callerPackageId }} is asking for permissions")
        },
        text = {
            Column(
                modifier = Modifier.verticalScroll(rememberScrollState()),
                verticalArrangement = Arrangement.spacedBy(12.dp),
            ) {
                // The package ID always gets its own line: the label is a self-description from the manifest,
                // two packages can use the same label, and the package ID is the one unique identity
                Text(
                    request.callerPackageId,
                    style = MaterialTheme.typography.bodySmall,
                    color = MaterialTheme.colorScheme.outline,
                )

                if (request.rationale.isNotEmpty()) {
                    RationaleBlock(request.rationale)
                }

                Text(
                    "Tick the permissions you're willing to grant:",
                    style = MaterialTheme.typography.bodyMedium,
                    fontWeight = FontWeight.Medium,
                )

                request.items.forEach { grant ->
                    PermissionRequestRow(
                        grant = grant,
                        checked = checked[grant.permissionId] == true,
                        onCheckedChange = { checked[grant.permissionId] = it },
                    )
                }
            }
        },
        confirmButton = {
            TextButton(onClick = {
                // Only the ticked ones go back. Nothing ticked is the same as deny, with no special case:
                // an empty set takes the deny path
                app.resolvePendingRequest(
                    request.items
                        .map { it.permissionId }
                        .filter { checked[it] == true }
                        .toSet()
                )
            }) {
                Text("Allow selected")
            }
        },
        dismissButton = {
            TextButton(onClick = deny) { Text("Deny") }
        },
    )
}

/**
 * The reason the app gives.
 *
 * It sits in a tinted container under "The app says" so that it **doesn't look like
 * the system talking**. This is the anti-spoofing part of the UI: the requester controls this text completely.
 */
@Composable
private fun RationaleBlock(rationale: String) {
    Surface(
        color = MaterialTheme.colorScheme.surfaceVariant,
        shape = MaterialTheme.shapes.small,
    ) {
        Column(Modifier.padding(12.dp), verticalArrangement = Arrangement.spacedBy(4.dp)) {
            Text(
                "The app says",
                style = MaterialTheme.typography.labelSmall,
                color = MaterialTheme.colorScheme.outline,
            )
            Text(rationale, style = MaterialTheme.typography.bodyMedium)
        }
    }
}

/** One permission to tick. Name, description and risk level all come from the kernel. */
@Composable
private fun PermissionRequestRow(
    grant: PermissionGrant,
    checked: Boolean,
    onCheckedChange: (Boolean) -> Unit,
) {
    Row(
        modifier = Modifier.fillMaxWidth(),
        verticalAlignment = Alignment.Top,
        horizontalArrangement = Arrangement.spacedBy(8.dp),
    ) {
        Checkbox(checked = checked, onCheckedChange = onCheckedChange)
        Column(Modifier.padding(top = 12.dp), verticalArrangement = Arrangement.spacedBy(2.dp)) {
            // English first, then Chinese, and only then the permission ID; a missing text is the defining package's bug,
            // but the UI shouldn't show a blank because of it
            Text(
                grant.displayName.en.ifEmpty {
                    grant.displayName.zhCn.ifEmpty { grant.permissionId }
                },
                style = MaterialTheme.typography.bodyLarge,
            )
            val desc = grant.description.en.ifEmpty { grant.description.zhCn }
            if (desc.isNotEmpty()) {
                Text(
                    desc,
                    style = MaterialTheme.typography.bodySmall,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                )
            }
            // The two high-risk levels get an explicit warning. **The risk level comes from the kernel, not a UI guess**:
            // if a permission that makes the arm move looks the same in the list as one that reads a file,
            // users treat them with the same casual attitude
            riskWarning(grant.riskClass)?.let {
                Text(
                    it,
                    style = MaterialTheme.typography.labelMedium,
                    color = MaterialTheme.colorScheme.error,
                    fontWeight = FontWeight.Medium,
                )
            }
        }
    }
}

/**
 * Warning text for the high-risk levels; returns null for normal ones (no noise).
 *
 * Only PHYSICAL_CONTROL and CRITICAL_SAFETY get a warning: their consequences land in the physical world,
 * a different order from "read a file it shouldn't have". A moving machine can hurt someone.
 */
private fun riskWarning(riskClass: RiskClass): String? = when (riskClass) {
    RiskClass.RISK_CLASS_PHYSICAL_CONTROL -> "⚠ This permission lets the app control the machine's movement"
    RiskClass.RISK_CLASS_CRITICAL_SAFETY -> "⚠ This permission touches safety-critical functions"
    else -> null
}
