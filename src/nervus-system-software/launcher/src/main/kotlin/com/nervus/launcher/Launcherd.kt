package com.nervus.launcher

import com.nervus.sdk.component.ComponentConfig
import com.nervus.sdk.component.InterfaceRequirement
import com.nervus.sdk.component.NervusApp
import java.util.logging.Logger
import kotlin.system.exitProcess

/**
 * launcherd: the resident service that starts the desktop and the right-hand navigation bar and keeps them alive.
 *
 * ⚠ **Don't confuse it with `nervus.sessiond`**: that's something else, a system service for HUMAN/AI
 * control-principal sessions that works with ControlLease `controller_class` (it is
 * `nervus.sessiond/main` on the kernel's protected list) and has nothing to do with the desktop.
 *
 * ## Why it exists
 *
 * The desktop is an app, and the kernel refuses **always-on apps**
 * (`pkgregistry.ErrLaunchModeTypeMismatch`), so the desktop can't start itself at boot.
 *
 * launcherd does that one job: it runs always-on and uses `LaunchComponent` (Envelope body 80)
 * to ask the kernel to start `nervus.launcher/desktop` and `nervus.launcher/navigation`.
 *
 * It needs `perm.system.launch` (MinTrust=Platform), so it must be a platform-signed
 * system image package: a third party can't install a session service that hijacks the desktop.
 *
 * ## Why no retry loop of its own
 *
 * If it can't reach nervud it just exits and lets nervud's supervisor restart it with exponential backoff
 * (the common rule for system services, see the nervus-system-server README). Retrying itself would hide
 * the fact that "nervud isn't up": the process would look healthy while doing nothing.
 *
 * The crash budget never runs out: backoff goes 1s→2s→4s→8s, so crashes land at about t=0,1,3,7,15, while the breaker
 * trips at 5 crashes in a 10-second window. Exponential backoff can never reach that.
 *
 * ## When the system UI dies
 *
 * [keepSystemUiAlive] asks for the desktop and the navigation bar again at intervals. While the components are alive this is
 * a cheap lookup; if one died, it gets started again. Polling rather than events, because nervud doesn't
 * implement Subscribe yet (it closes the connection if it gets one).
 */
class Launcherd(config: ComponentConfig) : NervusApp(config) {

    // The desktop interfaces aren't declared in requiredInterfaces: those resolve inside start(), and any failure
    // fails the whole component. "Can't start the desktop right now" mustn't stop launcherd from starting,
    // because launcherd being alive is what keeps trying. So resolve with resolveNow at our own pace after start
    override val requiredInterfaces: List<InterfaceRequirement> = emptyList()

    private val log = Logger.getLogger(Launcherd::class.java.name)

    fun keepSystemUiAlive(intervalMs: Long = SYSTEM_UI_CHECK_INTERVAL_MS) {
        while (true) {
            SYSTEM_UI_COMPONENTS.forEach { component ->
                try {
                    // LaunchComponent is idempotent: while the component runs this is a cheap lookup,
                    // and if it isn't running it gets started on the spot.
                    val alreadyRunning = launchComponent(LAUNCHER_PACKAGE, component)
                    if (!alreadyRunning) {
                        log.info("$component was not running; launched it")
                    }
                } catch (e: Exception) {
                    // One UI component failing mustn't stop the other from being checked.
                    log.warning("launch $component failed, will retry: ${e.message}")
                }
            }
            Thread.sleep(intervalMs)
        }
    }

    private companion object {
        const val LAUNCHER_PACKAGE = "nervus.launcher"
        const val SYSTEM_UI_CHECK_INTERVAL_MS = 5_000L
        val SYSTEM_UI_COMPONENTS = listOf("desktop", "navigation")
    }
}

fun main() {
    val log = Logger.getLogger("launcherd")
    val launcherd = Launcherd(ComponentConfig(componentId = "launcherd"))

    try {
        launcherd.start()
    } catch (e: Exception) {
        // Can't reach the control plane: exit and let the supervisor back off and restart us.
        // When the component is started, the ipc module may not be listening yet (service registers 8th, ipc 12th);
        // that race is expected, and the backoff restart gets past it
        log.severe("launcherd: cannot reach control plane: ${e.message}")
        exitProcess(1)
    }

    log.info("launcherd: up, waking desktop and navigation")
    Runtime.getRuntime().addShutdownHook(Thread { launcherd.close() })
    launcherd.keepSystemUiAlive()
}
