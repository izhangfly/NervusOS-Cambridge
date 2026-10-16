package com.nervus.filemanager

import androidx.compose.ui.unit.dp
import com.nervus.sdk.component.ComponentConfig
import com.nervus.sdk.component.InterfaceRequirement
import com.nervus.sdk.component.NervusApp
import com.nervus.sdk.ui.NervusWindow
import com.nervus.sdk.ui.attachComposeDesktop
import java.util.logging.Logger
import kotlin.system.exitProcess

/**
 * The file manager.
 *
 * It is the only system app that **calls no IPC interface**: file operations use local `java.nio`,
 * and access comes from the sandbox (declaring `perm.storage.user` makes the kernel add
 * `/var/lib/nervus/user-data` to ReadWritePaths).
 *
 * So why connect to the control plane at all? Two reasons:
 *
 *  1. **The handshake is an identity check.** Connecting means nervud confirmed via PID → cgroup → unit
 *     that "this process really is nervus.filemanager/main". If it can't connect, the kernel didn't start
 *     it, and exiting beats running a file manager of unknown identity.
 *  2. **Losing the control plane means the kernel is gone.** The app should exit so the supervisor restarts
 *     it, rather than leave a window that can still delete files while the system is out of control.
 *
 * A file-picker interface for other apps is left for v2: it needs a `.proto` in nervus-ipc
 * first, and hand-rolling a temporary payload encoding is a hard no.
 */
class FileManager(config: ComponentConfig) : NervusApp(config) {
    override val requiredInterfaces: List<InterfaceRequirement> = emptyList()
}

fun main() {
    val log = Logger.getLogger("filemanager")
    val fm = FileManager(ComponentConfig(componentId = "main"))

    try {
        fm.start()
    } catch (e: Exception) {
        log.severe("filemanager: cannot reach control plane: ${e.message}")
        exitProcess(1)
    }

    Runtime.getRuntime().addShutdownHook(Thread { fm.close() })

    val storageReady = UserStorage.ensureRoot()
    if (!storageReady) {
        // Don't exit: open the window and explain. This failure is nearly always configuration
        // (preflight never ran, or the manifest is missing perm.storage.user),
        // and a readable message helps far more than a process that silently exits
        log.warning("user-data is not writable: ${UserStorage.ROOT}")
    }

    // Window handle, so Back hides the window instead of killing the process (see onUnhandledBack below).
    val windowHandle = NervusWindow()

    fm.attachComposeDesktop(
        title = "Files",
        width = 1000.dp,
        height = 700.dp,
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
            log.severe("filemanager: control plane lost, exiting")
            exitProcess(1)
        },
    ) {
        FileManagerScreen(storageReady)
    }
}
