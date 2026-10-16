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
import androidx.compose.material3.ButtonDefaults
import androidx.compose.material3.Card
import androidx.compose.material3.CardDefaults
import androidx.compose.material3.Icon
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedButton
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
import com.nervus.sdk.ui.NervusBackHandler
import com.nervus.sysui.NervusIcons
import com.nervus.sysui.PowerAction
import com.nervus.sysui.PowerConfirmDialog
import com.nervus.sysui.PowerControl
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.launch
import kotlinx.coroutines.withContext

/**
 * Power page: orderly restart / power off.
 *
 * ## Why an exception from the call isn't shown as a failure
 *
 * Once the kernel gets the request, systemd starts shutting down straight away and the control-plane
 * connection drops, so **on the success path the call most likely throws**. Showing that as "power off
 * failed" makes people think nothing happened and press again, and every press is a real shutdown
 * request. So the page always says "sent" and demotes any exception to a line of diagnostics.
 */
@Composable
fun PowerPage(settings: Settings) {
    val scope = rememberCoroutineScope()

    // pending != null means we're waiting for the user to confirm an action
    var pending by remember { mutableStateOf<PowerAction?>(null) }
    // Once sent, nothing becomes clickable again: the machine is going down, so another button is pointless
    var issued by remember { mutableStateOf<PowerAction?>(null) }
    var diagnostic by remember { mutableStateOf<String?>(null) }

    NervusBackHandler(enabled = pending != null) {
        pending = null
    }

    Column(
        Modifier.fillMaxSize().padding(32.dp),
        verticalArrangement = Arrangement.spacedBy(16.dp),
    ) {
        Text("Power", style = MaterialTheme.typography.headlineMedium)
        Text(
            if (PowerControl.hosted) {
                "Restart and exit apply to NervusOS only: every app and service gets a stop signal and " +
                    "Ubuntu keeps running."
            } else {
                "Restart and power off both run systemd's full shutdown: running components get a stop " +
                    "signal and filesystems are unmounted cleanly."
            },
            style = MaterialTheme.typography.bodyMedium,
            color = MaterialTheme.colorScheme.onSurfaceVariant,
        )

        Spacer(Modifier.height(8.dp))

        val current = issued
        if (current != null) {
            Card(
                Modifier.fillMaxWidth(),
                colors = CardDefaults.cardColors(
                    containerColor = MaterialTheme.colorScheme.primaryContainer,
                ),
            ) {
                Row(
                    Modifier.padding(20.dp),
                    verticalAlignment = Alignment.CenterVertically,
                    horizontalArrangement = Arrangement.spacedBy(16.dp),
                ) {
                    Icon(
                        imageVector = if (current == PowerAction.Reboot) {
                            NervusIcons.Restart
                        } else {
                            NervusIcons.Power
                        },
                        contentDescription = null,
                        tint = MaterialTheme.colorScheme.onPrimaryContainer,
                        modifier = Modifier.size(32.dp),
                    )
                    Column(verticalArrangement = Arrangement.spacedBy(6.dp)) {
                        Text(
                            "${current.label}: request sent",
                            style = MaterialTheme.typography.titleMedium,
                            color = MaterialTheme.colorScheme.onPrimaryContainer,
                        )
                        Text(
                            if (PowerControl.hosted) "NervusOS is stopping its components." else "The system is stopping its components; the screen will go dark shortly.",
                            style = MaterialTheme.typography.bodyMedium,
                            color = MaterialTheme.colorScheme.onPrimaryContainer,
                        )
                    }
                }
            }
        } else {
            Row(horizontalArrangement = Arrangement.spacedBy(16.dp)) {
                OutlinedButton(
                    onClick = { pending = PowerAction.Reboot },
                    modifier = Modifier.height(52.dp),
                ) {
                    Icon(
                        imageVector = NervusIcons.Restart,
                        contentDescription = null,
                        modifier = Modifier.size(20.dp),
                    )
                    Spacer(Modifier.width(8.dp))
                    Text(PowerAction.Reboot.label)
                }
                Button(
                    onClick = { pending = PowerAction.PowerOff },
                    modifier = Modifier.height(52.dp),
                    // error colours: power off is destructive, and M3 uses the error container for that
                    colors = ButtonDefaults.buttonColors(
                        containerColor = MaterialTheme.colorScheme.error,
                        contentColor = MaterialTheme.colorScheme.onError,
                    ),
                ) {
                    Icon(
                        imageVector = NervusIcons.Power,
                        contentDescription = null,
                        modifier = Modifier.size(20.dp),
                    )
                    Spacer(Modifier.width(8.dp))
                    Text(PowerAction.PowerOff.label)
                }
            }
        }

        diagnostic?.let {
            Text(
                it,
                style = MaterialTheme.typography.bodySmall,
                color = MaterialTheme.colorScheme.onSurfaceVariant,
            )
        }
    }

    pending?.let { action ->
        PowerConfirmDialog(
            action = action,
            onDismiss = { pending = null },
            onConfirm = {
                pending = null
                issued = action
                scope.launch {
                    // The call blocks until the connection drops, so it must run off the UI thread,
                    // or the UI freezes for the whole shutdown
                    withContext(Dispatchers.IO) {
                        runCatching { settings.power(action) }.onFailure {
                            // See the class comment: this is most likely "connection closed", which is what success looks like.
                            // Show it only as diagnostics; it doesn't change the "sent" message above
                            diagnostic = "Control plane replied: ${it.message ?: it::class.simpleName}"
                        }
                    }
                }
            },
        )
    }
}
