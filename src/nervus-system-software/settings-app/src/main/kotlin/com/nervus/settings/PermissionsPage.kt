package com.nervus.settings

import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.width
import androidx.compose.material3.Button
import androidx.compose.material3.Card
import androidx.compose.material3.CardDefaults
import androidx.compose.material3.Icon
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.rememberCoroutineScope
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.unit.dp
import com.nervus.sysui.NervusIcons
import java.util.logging.Logger
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.launch
import kotlinx.coroutines.withContext

/** Logger for this page. Failures must reach the journal, not just the screen; see the note at the button. */
private val pageLog = Logger.getLogger("com.nervus.settings.PermissionsPage")

/**
 * Permissions page: a link to the permission manager.
 *
 * ## Why a link and not management right here
 *
 * Changing a permission's runtime grant needs `perm.permission.admin`, and that permission requires
 * the **platform-release** signing role. Settings is signed platform-systemapp: enough for Platform
 * trust, but not for this one.
 *
 * That's not a configuration slip. If "the power to give any app the camera and motion control" sat in Settings,
 * it would share one signing chain with a large package that changes often. Android makes
 * PermissionController a separate APK for the same reason.
 *
 * So this page does one thing: send the user to `nervus.permissionui`.
 *
 * ## Why the entry still lives in Settings
 *
 * In people's heads, permissions belong in Settings. Without this entry the only way in would be an icon on the
 * desktop, and most people wouldn't think to look there to revoke an app's camera permission.
 */
@Composable
fun PermissionsPage(settings: Settings) {
    val scope = rememberCoroutineScope()
    var launching by remember { mutableStateOf(false) }
    var error by remember { mutableStateOf<String?>(null) }

    Column(
        Modifier.fillMaxSize().padding(32.dp),
        verticalArrangement = Arrangement.spacedBy(16.dp),
    ) {
        Text("Permissions", style = MaterialTheme.typography.headlineMedium)
        Text(
            "See which sensitive permissions each app asked for and decide on each one. Revoking takes effect at once, " +
                "with no app restart.",
            style = MaterialTheme.typography.bodyMedium,
            color = MaterialTheme.colorScheme.onSurfaceVariant,
        )

        Spacer(Modifier.height(8.dp))

        Card(
            Modifier.fillMaxWidth(),
            colors = CardDefaults.cardColors(
                containerColor = MaterialTheme.colorScheme.surfaceVariant,
            ),
        ) {
            Row(
                Modifier.padding(20.dp),
                verticalAlignment = Alignment.CenterVertically,
                horizontalArrangement = Arrangement.spacedBy(16.dp),
            ) {
                Icon(
                    imageVector = NervusIcons.Shield,
                    contentDescription = null,
                    tint = MaterialTheme.colorScheme.onSurfaceVariant,
                    modifier = Modifier.size(32.dp),
                )
                Column(verticalArrangement = Arrangement.spacedBy(6.dp)) {
                    Text(
                        "Permissions are managed by a separate system component",
                        style = MaterialTheme.typography.titleMedium,
                    )
                    Text(
                        "Settings can't grant or revoke sensitive permissions. A separately signed component " +
                            "holds that power alone, so even a compromised Settings can't change any app's permissions.",
                        style = MaterialTheme.typography.bodyMedium,
                        color = MaterialTheme.colorScheme.onSurfaceVariant,
                    )
                }
            }
        }

        Button(
            onClick = {
                error = null
                launching = true
                scope.launch {
                    // The kernel waits for the target process to start and check in, which can block for seconds, so leave the UI thread
                    withContext(Dispatchers.IO) {
                        runCatching { settings.openPermissionManager() }
                            .onFailure {
                                // [Log it too, don't only show it on screen].
                                //
                                // This used to set only the error variable, so the message existed only on screen:
                                // the journal had [nothing] after "resolved -> endpoint N".
                                // Debugging meant reading a log without the real error,
                                // guessing, and paying a build-flash-reproduce cycle per guess.
                                //
                                // Include the stack trace: the message only says "failed", and this path goes through
                                // the SDK's call/re-resolve, so the stack shows which layer threw.
                                pageLog.log(
                                    java.util.logging.Level.SEVERE,
                                    "openPermissionManager failed", it,
                                )
                                error = it.message ?: it::class.simpleName
                            }
                    }
                    launching = false
                }
            },
            enabled = !launching,
            modifier = Modifier.height(52.dp),
        ) {
            Icon(
                imageVector = NervusIcons.Shield,
                contentDescription = null,
                modifier = Modifier.size(20.dp),
            )
            Spacer(Modifier.width(8.dp))
            Text(if (launching) "Opening…" else "Open permission manager")
        }

        // Show the failure. Resolving permission.ui can fail because permissionui is disabled
        // or the kernel couldn't start it; staying silent leaves the user with "I clicked and nothing happened"
        error?.let {
            Text(
                "Can't open the permission manager: $it",
                style = MaterialTheme.typography.bodySmall,
                color = MaterialTheme.colorScheme.error,
            )
        }
    }
}
