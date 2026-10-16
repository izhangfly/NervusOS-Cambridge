package com.nervus.launcher

import com.nervus.sdk.component.ComponentConfig
import com.nervus.sdk.component.InterfaceRequirement
import com.nervus.sdk.component.NervusApp
import androidx.compose.ui.unit.dp
import com.nervus.sdk.ui.attachComposeDesktop
import com.nervus.sysui.AppIdentity
import com.nervus.sysui.PowerAction
import com.nervus.sysui.PowerControl
import com.nervus.sysui.X11WindowControl
import io.github.nervusos.iface.pkgmanager.v1.PackageInfo
import java.util.logging.Logger
import kotlin.system.exitProcess

/**
 * The desktop (the launcher itself).
 *
 * - Uses `nervus.interface.pkg.manager` to list installed apps;
 * - opens them with `LaunchComponent` (Envelope body 80).
 *
 * It **exports no interface of its own**: since the kernel gained LaunchComponent, "can be started" no longer
 * needs "has an interface that can be resolved", so the session service just asks the kernel to start
 * `nervus.launcher/desktop`.
 */
class Desktop(config: ComponentConfig) : NervusApp(config) {

    override val requiredInterfaces: List<InterfaceRequirement> = listOf(
        InterfaceRequirement(
            id = PkgManagerMethods.INTERFACE_ID,
            // Optional: pkgmanagerd may not be up yet (system services start in random order;
            // the Go runtime deliberately randomises map iteration). If apps can't be listed the desktop should still show,
            // with a readable message, rather than the whole component failing with no window at all
            isRequired = false,
        ),
        InterfaceRequirement(
            id = PowerControl.INTERFACE_ID,
            // A built-in interface, present whenever nervud is alive. Still optional: the power buttons being unavailable
            // shouldn't stop the desktop from starting; the user at least needs to see the installed apps
            isRequired = false,
        ),
    )

    private val log = Logger.getLogger(Desktop::class.java.name)

    /** Fetches the apps to show on the desktop. Call from a background thread (see rememberPolled) */
    fun listApps(): List<PackageInfo> =
        try {
            decodeList(
                call(
                    interfaceId = PkgManagerMethods.INTERFACE_ID,
                    methodId = PkgManagerMethods.LIST,
                    payload = listRequestPayload(),
                )
            ).filterNot { AppIdentity.isHeadless(it.packageId) }
                .sortedBy { AppIdentity.displayName(it.packageId) }
        } catch (e: IllegalStateException) {
            // pkgmanager didn't resolve (the service isn't up). Not an error state, just "not ready yet"
            throw PkgManagerUnavailable(e.message ?: "package manager not resolved")
        }

    /**
     * Starts an app.
     *
     * Uses the Envelope's LaunchComponent(80), a message the kernel added for exactly this. Before it,
     * the only way to start something was indirectly, by sending ResolveEndpoint to an interface the target exports.
     * That forced every app to export a placeholder interface it didn't need, and the audit log couldn't tell
     * "resolve an interface" from "start an app".
     *
     * Blocks (the kernel waits for the target process to come up), so call it from a background thread.
     */
    fun launch(packageId: String, componentId: String) {
        log.info("launching $packageId/$componentId")
        val alreadyRunning = launchComponent(packageId, componentId)
        if (alreadyRunning) {
            log.info("$packageId/$componentId was already running")
            if (!X11WindowControl.activateWindow(AppIdentity.displayName(packageId))) {
                throw IllegalStateException(
                    "$packageId/$componentId is running but its window cannot be activated"
                )
            }
        }
    }

    /**
     * Sends an orderly power action.
     *
     * The desktop has this entry too because powering off shouldn't require remembering it lives in Settings:
     * it's the most basic operation a robot has. The meaning is exactly the same as in Settings (same built-in
     * interface, same confirmation text, see [PowerControl]).
     *
     * **It normally doesn't return normally**: systemd starts shutting down at once and the control-plane
     * connection drops. An exception here doesn't mean the shutdown failed, so don't show it as one.
     */
    fun power(action: PowerAction) {
        log.info("power action requested: ${action.label}")
        call(
            interfaceId = PowerControl.INTERFACE_ID,
            methodId = action.methodId,
            payload = ByteArray(0),
        )
    }
}

/** pkgmanager isn't available yet. Kept apart from "the call failed" because the UI words them differently */
class PkgManagerUnavailable(message: String) : Exception(message)

fun main() {
    val log = Logger.getLogger("desktop")
    val desktop = Desktop(ComponentConfig(componentId = "desktop"))

    try {
        desktop.start()
    } catch (e: Exception) {
        log.severe("desktop: cannot reach control plane: ${e.message}")
        exitProcess(1)
    }

    Runtime.getRuntime().addShutdownHook(Thread { desktop.close() })

    desktop.attachComposeDesktop(
        title = "Nervus",
        width = 1280.dp,
        height = 800.dp,
        // The desktop is the root of the UI task. Back at this level is consumed
        // instead of cycling to an application behind the home screen.
        onUnhandledBack = { true },
        onDisconnect = {
            // Exit when the control plane goes away and let the supervisor restart us. An empty window that can't reach
            // the kernel is worse than no window: it looks fine, and nothing in it works
            log.severe("desktop: control plane lost, exiting for restart")
            exitProcess(1)
        },
    ) {
        DesktopScreen(desktop)
    }
}
