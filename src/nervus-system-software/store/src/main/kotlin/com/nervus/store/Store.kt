package com.nervus.store

import androidx.compose.ui.unit.dp
import com.nervus.sdk.component.ComponentConfig
import com.nervus.sdk.component.InterfaceRequirement
import com.nervus.sdk.component.NervusApp
import com.nervus.sdk.ui.NervusWindow
import com.nervus.sdk.ui.attachComposeDesktop
import com.nervus.sysui.AppIdentity
import com.nervus.sysui.X11WindowControl
import io.github.nervusos.iface.pkgmanager.v1.InspectRequest
import io.github.nervusos.iface.pkgmanager.v1.InspectResult
import io.github.nervusos.iface.pkgmanager.v1.InstallRequest
import io.github.nervusos.iface.pkgmanager.v1.InstallResult
import io.github.nervusos.iface.pkgmanager.v1.ListRequest
import io.github.nervusos.iface.pkgmanager.v1.ListResult
import io.github.nervusos.iface.pkgmanager.v1.PackageInfo
import io.github.nervusos.iface.pkgmanager.v1.PackageManagerErrorDetail
import io.github.nervusos.iface.pkgmanager.v1.UninstallRequest
import java.nio.file.Files
import java.nio.file.Path
import java.nio.file.Paths
import java.nio.file.StandardCopyOption
import java.nio.file.attribute.PosixFilePermissions
import java.util.logging.Logger
import kotlin.system.exitProcess

/**
 * The App Store.
 *
 * Browses the catalog shipped in the image, and installs, opens and uninstalls
 * packages through `nervus.interface.pkg.manager`. Like Settings, it makes no
 * decisions of its own: signature checks, ABI matching, downgrade protection
 * and permission grants all happen in the kernel. The store shows the result.
 *
 * Install is three steps:
 *
 * 1. Copy the .nspkg into `/var/lib/nervus/user-data/.store/`. pkgmanagerd only
 *    takes packages from the shared user-data area (`nspkg_relpath`).
 * 2. INSPECT it: the kernel unpacks and verifies it and lists the sensitive
 *    permissions it asks for. The store shows those on a consent screen.
 * 3. INSTALL it with the permissions the user agreed to and the manifest digest
 *    from step 2, so the kernel refuses a package that changed in between.
 */
class Store(config: ComponentConfig) : NervusApp(config) {

    override val requiredInterfaces: List<InterfaceRequirement> = listOf(
        InterfaceRequirement(
            id = PkgManager.INTERFACE_ID,
            // pkgmanagerd may still be starting. The store should open and say so.
            isRequired = false,
        ),
    )

    private val log = Logger.getLogger(Store::class.java.name)

    fun catalog(): List<CatalogEntry> = Catalog.load()

    fun installed(): Map<String, PackageInfo> =
        ListResult.parseFrom(
            call(
                interfaceId = PkgManager.INTERFACE_ID,
                methodId = PkgManager.LIST,
                payload = ListRequest.getDefaultInstance().toByteArray(),
            )
        ).packagesList.associateBy { it.packageId }

    /** Copies the package into the hand-off area and returns its relative path. */
    fun stage(entry: CatalogEntry): String {
        val source = Catalog.packagesDir().resolve(entry.file)
        require(Files.isRegularFile(source)) { "package file missing from the catalog: ${entry.file}" }
        Files.createDirectories(STAGING_DIR)
        val target = STAGING_DIR.resolve(entry.file)
        Files.copy(source, target, StandardCopyOption.REPLACE_EXISTING)
        // pkgmanagerd runs under its own UID and has to be able to read the file
        Files.setPosixFilePermissions(target, PosixFilePermissions.fromString("rw-r--r--"))
        return USER_DATA.relativize(target).toString()
    }

    fun inspect(relPath: String): InspectResult =
        InspectResult.parseFrom(
            call(
                interfaceId = PkgManager.INTERFACE_ID,
                methodId = PkgManager.INSPECT,
                payload = InspectRequest.newBuilder().setNspkgRelpath(relPath).build().toByteArray(),
                timeoutSeconds = 120,
            )
        )

    fun install(relPath: String, consented: List<String>, manifestDigest: String): PackageInfo {
        log.info("install $relPath (consented: $consented)")
        val outcome = callOperation(
            interfaceId = PkgManager.INTERFACE_ID,
            methodId = PkgManager.INSTALL,
            payload = InstallRequest.newBuilder()
                .setNspkgRelpath(relPath)
                .addAllConsentedPermissions(consented)
                .setExpectedManifestDigest(manifestDigest)
                .build()
                .toByteArray(),
            timeoutSeconds = 300,
        )
        if (!outcome.isSuccess) {
            val reason = runCatching { PackageManagerErrorDetail.parseFrom(outcome.errorDetail).reason.name }
                .getOrNull()
            throw RuntimeException("install failed: ${outcome.code}${reason?.let { " ($it)" } ?: ""}")
        }
        return InstallResult.parseFrom(outcome.result).`package`
    }

    fun uninstall(packageId: String) {
        log.info("uninstall $packageId")
        call(
            interfaceId = PkgManager.INTERFACE_ID,
            methodId = PkgManager.UNINSTALL,
            payload = UninstallRequest.newBuilder().setPackageId(packageId).build().toByteArray(),
        )
    }

    /** Starts the app, or brings its window to the front if it is already running. */
    fun open(entry: CatalogEntry) {
        log.info("open ${entry.packageId}/${entry.launchComponent}")
        val alreadyRunning = launchComponent(entry.packageId, entry.launchComponent)
        if (alreadyRunning) {
            X11WindowControl.activateWindow(AppIdentity.displayName(entry.packageId))
        }
    }

    companion object {
        val USER_DATA: Path = Paths.get("/var/lib/nervus/user-data")
        val STAGING_DIR: Path = USER_DATA.resolve(".store")
    }
}

/**
 * Method IDs of `nervus.interface.pkg.manager`. The values come from the
 * `PackageManagerMethod` enum in nervus-ipc; a wrong one fails with NOT_FOUND.
 */
object PkgManager {
    const val INTERFACE_ID = "nervus.interface.pkg.manager"
    const val INSTALL = 1
    const val UNINSTALL = 2
    const val LIST = 3
    const val INSPECT = 5
}

fun main() {
    val log = Logger.getLogger("store")
    val store = Store(ComponentConfig(componentId = "main"))

    try {
        store.start()
    } catch (e: Exception) {
        log.severe("store: cannot reach control plane: ${e.message}")
        exitProcess(1)
    }

    Runtime.getRuntime().addShutdownHook(Thread { store.close() })

    val windowHandle = NervusWindow()

    store.attachComposeDesktop(
        // Must match AppIdentity.displayName("nervus.store"): the desktop finds
        // the running window by this exact title.
        title = "App Store",
        width = 1100.dp,
        height = 760.dp,
        onUnhandledBack = {
            windowHandle.hide()
            true
        },
        windowHandle = windowHandle,
        onDisconnect = {
            log.severe("store: control plane lost, exiting")
            exitProcess(1)
        },
    ) {
        StoreScreen(store)
    }
}
