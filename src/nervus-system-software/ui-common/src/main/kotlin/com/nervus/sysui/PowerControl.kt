package com.nervus.sysui

import androidx.compose.material3.AlertDialog
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.runtime.Composable
import java.io.File

/**
 * `nervus.interface.power`: orderly reboot / power off of the whole machine.
 *
 * ## It is a built-in interface
 *
 * nervud implements it itself, and the Authority Gate does the work. Only the
 * kernel may shut the machine down, so no external Provider could offer it.
 * Callers still use a plain Resolve + Request and don't need to know whether
 * the other end is the kernel or a Provider.
 *
 * ## The constants have no proto behind them
 *
 * The pkgmanager method IDs can be checked against the generated
 * `PackageManagerMethod` enum, but this interface has no `.proto` yet. The only
 * definition is in the kernel, `nervud/internal/power/builtin.go`, and this
 * object mirrors it by hand. It lives in ui-common rather than in each app so
 * there is exactly one mirror. Change the kernel and you must change this too,
 * or the symptom is "I pressed it and nothing happened": the call is routed to
 * a method that doesn't exist and gets NOT_FOUND.
 *
 * Once v2 adds `power_control.proto`, both sides should take the values from
 * the generated enum.
 *
 * ## Calling it needs perm.authority.power
 *
 * MinTrust=Platform, so only packages signed with a platform role can use it,
 * and they must list it under `permissions` in the manifest or Resolve fails
 * with PERMISSION_DENIED.
 *
 * ## Hosted mode
 *
 * When NervusOS runs as a session on top of Ubuntu (see NervusOS-Cambridge
 * hosted/), the kernel routes these two methods to a hook: power off ends the
 * NervusOS session and returns to Ubuntu, reboot restarts NervusOS. The
 * installer writes `/etc/nervus/hosted` so the UI can word the buttons to match.
 */
object PowerControl {
    const val INTERFACE_ID = "nervus.interface.power"
    const val REBOOT = 1
    const val POWER_OFF = 2

    /** True when NervusOS runs as a session on a desktop Linux host. */
    val hosted: Boolean by lazy { File("/etc/nervus/hosted").exists() }
}

/** A power action. [label] doubles as the button text and the confirm dialog's verb. */
enum class PowerAction(val methodId: Int) {
    Reboot(PowerControl.REBOOT),
    PowerOff(PowerControl.POWER_OFF);

    val label: String
        get() = when (this) {
            Reboot -> if (PowerControl.hosted) "Restart NervusOS" else "Restart"
            PowerOff -> if (PowerControl.hosted) "Exit NervusOS" else "Power off"
        }

    val confirmText: String
        get() = when (this) {
            Reboot -> if (PowerControl.hosted) {
                "NervusOS stops every running app and service and starts again. Ubuntu keeps running."
            } else {
                "The robot stops every running component and restarts. Work in progress is not saved."
            }
            PowerOff -> if (PowerControl.hosted) {
                "NervusOS stops every running app and service, the arm driver included, and you return to Ubuntu."
            } else {
                "The robot stops every running component and powers off. Press the power button to start it again."
            }
        }
}

/**
 * The confirmation dialog for a power action.
 *
 * It must exist: this is the one operation in the system that can't be undone
 * and leaves the whole machine unusable. One mis-tap means walking over to the
 * robot to press its power button. An M3 [AlertDialog] is the standard shape
 * for a destructive confirmation.
 *
 * The desktop and Settings both have a power entry, and the wording must be the
 * same in both, so the dialog lives here instead of being written twice.
 */
@Composable
fun PowerConfirmDialog(
    action: PowerAction,
    onConfirm: () -> Unit,
    onDismiss: () -> Unit,
) {
    AlertDialog(
        onDismissRequest = onDismiss,
        title = { Text("${action.label}?") },
        text = { Text(action.confirmText) },
        confirmButton = { TextButton(onClick = onConfirm) { Text(action.label) } },
        dismissButton = { TextButton(onClick = onDismiss) { Text("Cancel") } },
    )
}
