package com.nervus.settings

import androidx.compose.ui.unit.dp
import com.nervus.sdk.component.ComponentConfig
import com.nervus.sdk.component.InterfaceRequirement
import com.nervus.sdk.component.NervusApp
import com.nervus.sdk.ui.NervusWindow
import com.nervus.sdk.ui.attachComposeDesktop
import com.nervus.sysui.PowerAction
import com.nervus.sysui.PowerControl
import io.github.nervusos.iface.permission.v1.OpenManagerRequest
import io.github.nervusos.iface.pkgmanager.v1.*
import java.util.logging.Logger
import kotlin.system.exitProcess

/**
 * The Settings app.
 *
 * Everything it can do comes from the four methods of `nervus.interface.pkg.manager`. **It makes no decisions itself**:
 * what to list, whether something can be uninstalled or disabled, is all decided by nervud. This app shows the
 * result and turns what the user wants into a call.
 *
 * That isn't laziness: `pkgregistry` has a full set of admission rules (protected list, system packages can't be
 * uninstalled, signing lineage...). Copying those checks into the UI would create a second source of truth, and once they
 * disagree, a button the user can press is bound to fail.
 */
class Settings(config: ComponentConfig) : NervusApp(config) {

    override val requiredInterfaces: List<InterfaceRequirement> = listOf(
        InterfaceRequirement(
            id = PkgManager.INTERFACE_ID,
            // Optional: pkgmanagerd may not be up yet (system services start in random order).
            // Settings should still open and explain why, rather than the whole component failing
            isRequired = false,
        ),
        InterfaceRequirement(
            id = PowerControl.INTERFACE_ID,
            // Also optional: it's a built-in interface, present while nervud is alive, so in theory it can't fail to resolve.
            // But "the power button doesn't resolve" shouldn't keep all of Settings closed; the user has other things to do
            isRequired = false,
        ),
        InterfaceRequirement(
            id = PermissionUiInterface.INTERFACE_ID,
            // Optional: permissionui is an on-demand UI component; whether it's running has nothing
            // to do with whether Settings can open
            isRequired = false,
            // [Must be false]. permissionui's launch_mode is on-demand, and resolving it is what
            // starts it. Resolving at startup would launch the permission manager as soon as Settings opens,
            // and the user would see a window they never asked for pop up.
            //
            // Resolve on the first call in openPermissionManager() instead: at that point the user has just
            // pressed "Open permission manager", so starting it is exactly what they want
            resolveEagerly = false,
        ),
    )

    private val log = Logger.getLogger(Settings::class.java.name)

    fun listPackages(): List<PackageInfo> =
        ListResult.parseFrom(
            call(
                interfaceId = PkgManager.INTERFACE_ID,
                methodId = PkgManager.LIST,
                payload = ListRequest.getDefaultInstance().toByteArray(),
            )
        ).packagesList.sortedBy { it.packageId }

    /**
     * Uninstalls a package.
     *
     * nervud refuses system image packages with `ErrSystemPackageImmutable`. **Don't block that here
     * in advance**: let the kernel give the authoritative answer and show it as-is. Same reasoning as the class comment.
     */
    fun uninstall(packageId: String) {
        log.info("uninstall $packageId")
        call(
            interfaceId = PkgManager.INTERFACE_ID,
            methodId = PkgManager.UNINSTALL,
            payload = UninstallRequest.newBuilder().setPackageId(packageId).build().toByteArray(),
        )
    }

    /**
     * Opens the permission manager (`nervus.permissionui`).
     *
     * # Why launch another app instead of a page in this one
     *
     * Changing the runtime grant of a USER_CONSENT permission needs `perm.permission.admin`,
     * which requires the **platform-release** signing role. This app is signed platform-systemapp:
     * Platform trust, but not that permission. That's intended, not a configuration slip: "the power to give any app
     * the camera and motion control" shouldn't share a signing chain with a large package that changes often
     * (Android makes PermissionController a separate APK for the same reason).
     *
     * So the entry in Settings only [links across]; granting happens in permissionui.
     *
     * # Why an interface call instead of LaunchComponent
     *
     * This used to use `LaunchComponent` (envelope 80), because permissionui didn't export
     * any interface yet. That brought it up but **couldn't pass any argument**: a user pressing "Manage permissions"
     * on one app in Settings landed on the overview of every app and had to find it again.
     *
     * Now permissionui exports `permission.ui`, so this is an ordinary call that carries
     * `package_id`, and the UI narrows straight to that package. A side benefit is not needing
     * `perm.system.launch` any more: that permission doesn't restrict the target and can start any component,
     * while this app only wants to open the permissions UI. The grant shrinks to just what's needed.
     *
     * The kernel starts the component on Resolve because of `launch_mode = on-demand`, so this app
     * doesn't need to know whether it's running; permissionui activates its own window too (it knows its
     * window title).
     *
     * @param packageId show only this package's permissions; the empty string (default) opens the overview.
     *
     * Blocks (the kernel waits for the target process to start), so call it from a background thread.
     */
    fun openPermissionManager(packageId: String = "") {
        log.info("openManager packageId='${packageId.ifEmpty { "(all)" }}'")
        call(
            interfaceId = PermissionUiInterface.INTERFACE_ID,
            methodId = PermissionUiInterface.OPEN_MANAGER,
            payload = OpenManagerRequest.newBuilder()
                .setPackageId(packageId)
                .build()
                .toByteArray(),
        )
    }

    /**
     * Disables or enables a component.
     *
     * Components on the protected list (Settings itself, the permission UI, pkgmanagerd...) are refused by the kernel
     * with `ErrComponentProtected`. The UI shows those failures.
     */
    fun setComponentEnabled(packageId: String, componentId: String, enabled: Boolean) {
        log.info("setEnabled $packageId/$componentId -> $enabled")
        call(
            interfaceId = PkgManager.INTERFACE_ID,
            methodId = PkgManager.SET_COMPONENT_ENABLED,
            payload = SetComponentEnabledRequest.newBuilder()
                .setPackageId(packageId)
                .setComponentId(componentId)
                .setEnabled(enabled)
                .build()
                .toByteArray(),
        )
    }

    /**
     * Sends an orderly power action (restart / power off).
     *
     * "Orderly" is the point: the kernel runs systemd's full `shutdown.target`, so running components
     * get SIGTERM and a chance to save, and filesystems unmount cleanly. It's the same as typing
     * `systemctl reboot` in a terminal, and **not** a `reboot(2)` hard reset (that path is for fault recovery
     * and only for platform-release-signed packages, see the kernel's `perm.authority.reboot`).
     *
     * The call **normally doesn't return normally**: systemd starts shutting down at once and the control-plane
     * connection drops. An exception therefore doesn't mean the restart failed; the UI always treats it as "sent".
     * Don't report a failure because of the exception, or the user will think nothing happened and press again.
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

/**
 * Method IDs of `nervus.interface.pkg.manager`.
 *
 * The values come from the `PackageManagerMethod` enum in `nervus-ipc`, and **the proto wins**.
 * Getting one wrong produces a precise NOT_FOUND; it never silently calls a different method.
 */
object PkgManager {
    const val INTERFACE_ID = "nervus.interface.pkg.manager"
    const val INSTALL = 1
    const val UNINSTALL = 2
    const val LIST = 3
    const val SET_COMPONENT_ENABLED = 4
}

/**
 * Method IDs of `nervus.interface.permission.ui`.
 *
 * The values follow the proto's `PermissionUiMethod` enum. This app only uses `OPEN_MANAGER`;
 * `CONFIRM_INSTALL` needs `perm.pkg.install`, and that's the installer's job.
 */
object PermissionUiInterface {
    const val INTERFACE_ID = "nervus.interface.permission.ui"
    const val OPEN_MANAGER = 2
}


fun main() {
    val log = Logger.getLogger("settings")
    val settings = Settings(ComponentConfig(componentId = "main"))

    try {
        settings.start()
    } catch (e: Exception) {
        log.severe("settings: cannot reach control plane: ${e.message}")
        exitProcess(1)
    }

    Runtime.getRuntime().addShutdownHook(Thread { settings.close() })

    // Window handle, so Back hides the window instead of killing the process (see onUnhandledBack below).
    val windowHandle = NervusWindow()

    settings.attachComposeDesktop(
        title = "Settings",
        width = 1100.dp,
        height = 760.dp,
        // Back hides the window rather than ending the process.
        //
        // [Not X11WindowControl.hideActiveWindow]: that is an xdotool minimise, which here
        // makes Compose fire onCloseRequest, so one press of Back quits the whole process.
        // The next launch pays the JVM + Compose cold start again, while the user thought
        // they had only gone back one level.
        //
        // Returning true means this app handled Back and it doesn't bubble further.
        onUnhandledBack = {
            windowHandle.hide()
            true
        },
        windowHandle = windowHandle,
        onDisconnect = {
            log.severe("settings: control plane lost, exiting")
            exitProcess(1)
        },
    ) {
        SettingsScreen(settings)
    }
}
