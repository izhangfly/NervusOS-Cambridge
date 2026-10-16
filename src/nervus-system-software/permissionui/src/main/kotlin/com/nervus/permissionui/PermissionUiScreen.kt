package com.nervus.permissionui

import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.PaddingValues
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.material3.Card
import androidx.compose.material3.CardDefaults
import androidx.compose.material3.HorizontalDivider
import androidx.compose.material3.Icon
import androidx.compose.material3.ListItem
import androidx.compose.material3.ListItemDefaults
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Scaffold
import androidx.compose.material3.Surface
import androidx.compose.material3.Switch
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.runtime.Composable
import androidx.compose.runtime.collectAsState
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateMapOf
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.rememberCoroutineScope
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.unit.dp
import com.nervus.sysui.NervusIcons
import com.nervus.sysui.iconForPackage
import com.nervus.sysui.rememberPolled
import io.github.nervusos.iface.permission.v1.GrantState
import io.github.nervusos.iface.permission.v1.PackageGrants
import io.github.nervusos.iface.permission.v1.PermissionGrant
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.launch
import kotlinx.coroutines.withContext

/**
 * The permission manager.
 *
 * One card per package, one row per USER_CONSENT permission. **The kernel supplies everything in the list**,
 * including each permission's name and description: third-party packages can define their own permissions, and the UI
 * can't know their text in advance.
 */
@Composable
fun PermissionUiScreen(app: PermissionUi) {
    // Polling rather than a subscription: grants are mostly changed by the user right here, and outside changes
    // (install, uninstall) are rare. PermissionAdmin doesn't declare any events either
    val packages by rememberPolled<List<PackageGrants>>(emptyList(), intervalMs = 5000) {
        app.listGrants()
    }

    // The "show only this package" filter from OpenManager. Empty string = overview.
    //
    // An IPC dispatch thread writes it and this code (the UI thread) reads it, so a StateFlow sits
    // in between rather than Compose state; see PermissionUi.managerFilter
    val filter by app.managerFilter.collectAsState()

    // A permission request waiting for the user's answer. Also behind a StateFlow, since a worker thread writes it.
    //
    // The dialog sits [on top of the manager] rather than in a separate window: Nervus shows one foreground window,
    // a second window might not come to the front, and a consent dialog that never shows would make the request time out silently.
    val pending by app.pendingRequest.collectAsState()

    // Filtering happens [in the view] rather than asking the kernel for one package: ListGrants has no
    // per-package filter, and the overview and the single-package view read the same data. A second call would only
    // give the two views a chance to disagree
    val shown = if (filter.isEmpty()) {
        packages.value
    } else {
        packages.value.filter { it.packageId == filter }
    }

    Surface(Modifier.fillMaxSize(), color = MaterialTheme.colorScheme.background) {
        Scaffold { inner ->
            val box = Modifier.fillMaxSize().padding(inner)
            when {
                // Three states kept apart. "Couldn't read" and "there are none" must never read the same:
                // the latter means "nobody asked for sensitive permissions", the former "I don't know whether anyone did",
                // and presenting the former as the latter would make users think nothing in the system needs their attention
                packages.value.isEmpty() && packages.error != null ->
                    Message(box, "Can't read the permission list", packages.error)

                packages.value.isEmpty() && packages.loading ->
                    Message(box, "Loading…", null)

                packages.value.isEmpty() ->
                    Message(
                        box,
                        "No app has asked for sensitive permissions",
                        "Apps appear here once they ask for the camera, user files or motion control",
                    )

                // Empty after filtering is [a different case]: the data was read, that package just has nothing
                // to grant (or isn't installed). Saying "no app has asked for sensitive permissions" would make someone who came
                // from Settings think there's nothing to manage anywhere
                shown.isEmpty() ->
                    Message(
                        box,
                        "This app has no permissions to manage",
                        filter,
                    )

                else -> LazyColumn(
                    modifier = box,
                    contentPadding = PaddingValues(20.dp),
                    verticalArrangement = Arrangement.spacedBy(16.dp),
                ) {
                    // When we came from Settings, offer a way back to the overview. Without it the user is stuck
                    // in the single-package view, and this screen has no other navigation
                    if (filter.isNotEmpty()) {
                        item {
                            TextButton(onClick = { app.clearManagerFilter() }) {
                                Text("← All apps' permissions")
                            }
                        }
                    }
                    items(shown, key = { it.packageId }) { pkg ->
                        PackageCard(app, pkg)
                    }
                }
            }
        }
    }

    // The request dialog sits on top of the manager. It goes outside the Surface, at the end of the composable: AlertDialog
    // is its own overlay, and inside the Scaffold content it would pick up that padding and scroll state.
    //
    // The filter doesn't apply to it: a permission request must always reach the user, whatever package the
    // screen happens to be narrowed to
    pending?.let { PermissionRequestDialog(app, it) }
}

/** One centred line plus an optional subtitle, shared by loading / unreadable / empty. */
@Composable
private fun Message(modifier: Modifier, title: String, detail: String?) {
    Box(modifier, contentAlignment = Alignment.Center) {
        Column(horizontalAlignment = Alignment.CenterHorizontally) {
            Icon(
                NervusIcons.Apps,
                contentDescription = null,
                modifier = Modifier.size(48.dp),
                tint = MaterialTheme.colorScheme.outline,
            )
            Text(
                title,
                style = MaterialTheme.typography.titleMedium,
                modifier = Modifier.padding(top = 16.dp),
            )
            if (!detail.isNullOrEmpty()) {
                Text(
                    detail,
                    style = MaterialTheme.typography.bodyMedium,
                    color = MaterialTheme.colorScheme.outline,
                    modifier = Modifier.padding(top = 4.dp),
                )
            }
        }
    }
}

@Composable
private fun PackageCard(app: PermissionUi, pkg: PackageGrants) {
    Card(
        modifier = Modifier.fillMaxWidth(),
        colors = CardDefaults.cardColors(
            containerColor = MaterialTheme.colorScheme.surfaceContainerLow,
        ),
    ) {
        Row(
            modifier = Modifier.fillMaxWidth().padding(16.dp),
            verticalAlignment = Alignment.CenterVertically,
        ) {
            Icon(
                iconForPackage(pkg.packageId),
                contentDescription = null,
                modifier = Modifier.size(28.dp),
                tint = MaterialTheme.colorScheme.primary,
            )
            Column(Modifier.padding(start = 12.dp)) {
                // The label may be empty (the manifest didn't set one); fall back to package_id
                Text(
                    pkg.label.ifEmpty { pkg.packageId },
                    style = MaterialTheme.typography.titleMedium,
                    fontWeight = FontWeight.Medium,
                )
                Text(
                    pkg.packageId,
                    style = MaterialTheme.typography.bodySmall,
                    color = MaterialTheme.colorScheme.outline,
                )
            }
        }
        HorizontalDivider()
        pkg.permissionsList.forEach { grant ->
            PermissionRow(app, pkg.packageId, grant)
        }
    }
}

@Composable
private fun PermissionRow(app: PermissionUi, packageId: String, grant: PermissionGrant) {
    val scope = rememberCoroutineScope()
    // Local overlay: a switch reflects what the user meant the moment it's flipped, then gets corrected once the kernel answers.
    // Without it the switch waits for an IPC round trip before moving and feels stuck
    val pending = remember { mutableStateMapOf<String, Boolean>() }
    var failure by remember { mutableStateOf<String?>(null) }

    // [The switch shows effectiveGranted, not state].
    //
    // For system software the two differ: the consent exemption skips the "ask the user" step itself
    // and doesn't fake a grant record, so system software's state is always NOT_REQUESTED while the kernel's
    // AllowedAt is always true. Showing state would turn the file manager's "user files" switch
    // off while it can actually read and write the user folder: the switch would contradict reality.
    val authoritative = grant.effectiveGranted

    // The exemption test: the kernel says it's allowed, but the user never made any decision.
    //
    // Ordinary apps never land in this combination: for them to be allowed, state must be GRANTED,
    // which only happens when the user says yes.
    val exempt = grant.effectiveGranted &&
        grant.state == GrantState.GRANT_STATE_NOT_REQUESTED

    val checked = pending[grant.permissionId] ?: authoritative

    ListItem(
        colors = ListItemDefaults.colors(containerColor = androidx.compose.ui.graphics.Color.Transparent),
        headlineContent = {
            // The kernel gives LocalizedText{zh_cn, en}; use English, fall back to the other text,
            // and only then to the permission ID. A missing text is the defining package's bug,
            // but the UI shouldn't show a blank because of it
            Text(
                grant.displayName.en.ifEmpty {
                    grant.displayName.zhCn.ifEmpty { grant.permissionId }
                }
            )
        },
        supportingContent = {
            val desc = grant.description.en.ifEmpty { grant.description.zhCn }
            Column {
                if (desc.isNotEmpty()) Text(desc)
                // An exemption needs an explanation. A switch that's on but won't move, with no reason given,
                // just looks broken, when the truth is "it's part of the system and can't be revoked"
                if (exempt) {
                    Text(
                        "System app: this permission comes with the system and can't be turned off",
                        color = MaterialTheme.colorScheme.outline,
                        style = MaterialTheme.typography.bodySmall,
                    )
                }
                failure?.let {
                    Text(
                        it,
                        color = MaterialTheme.colorScheme.error,
                        style = MaterialTheme.typography.bodySmall,
                    )
                }
            }
        },
        trailingContent = {
            Switch(
                checked = checked,
                // Exempt switches can't be flipped: it would mean nothing, and the kernel would refuse with
                // FAILED_PRECONDITION (system software's runtime state isn't the user's to decide).
                // Letting it flip would give the user a switch that only ever errors
                enabled = !exempt,
                onCheckedChange = { want ->
                    // The user is working in this screen, so finishing a concurrent permission request mustn't hide it.
                    // Set it here as well, not only from OpenManager: opening this app directly
                    // from the desktop involves no IPC call through us at all
                    app.markWindowWanted()
                    pending[grant.permissionId] = want
                    failure = null
                    scope.launch {
                        val result = withContext(Dispatchers.IO) {
                            runCatching { app.setGrantState(packageId, grant.permissionId, want) }
                        }
                        result.onSuccess { state ->
                            // Trust the state the kernel returned, not the value we just asked for
                            pending[grant.permissionId] =
                                state == GrantState.GRANT_STATE_GRANTED
                        }.onFailure { e ->
                            // On failure, put the switch back to the authoritative state and say why. Leaving it where
                            // the user flipped it would make them think the change worked
                            pending.remove(grant.permissionId)
                            failure = "Couldn't change it: ${e.message ?: "unknown reason"}"
                        }
                    }
                },
            )
        },
    )
}
