package com.nervus.launcher

import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.rememberCoroutineScope
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.unit.dp
import androidx.compose.ui.window.Window
import androidx.compose.ui.window.WindowPosition
import androidx.compose.ui.window.application
import androidx.compose.ui.window.rememberWindowState
import com.nervus.sdk.component.ComponentConfig
import com.nervus.sdk.component.InterfaceRequirement
import com.nervus.sdk.component.NervusApp
import com.nervus.sdk.ipc.ConnectionState
import com.nervus.sdk.ui.NervusTheme
import com.nervus.sysui.X11WindowControl
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.delay
import kotlinx.coroutines.isActive
import kotlinx.coroutines.launch
import kotlinx.coroutines.withContext
import java.awt.GraphicsEnvironment
import java.util.logging.Logger
import kotlin.system.exitProcess

/**
 * The persistent right-side system navigation bar.
 *
 * The window deliberately cannot take keyboard focus. A touch on Back therefore
 * leaves focus on the application and sends the Nervus Back key to that window.
 * Home ensures the desktop process exists and then asks Openbox to activate it.
 */
class Navigation(config: ComponentConfig) : NervusApp(config) {
    override val requiredInterfaces: List<InterfaceRequirement> = emptyList()

    fun back() {
        check(X11WindowControl.sendBack()) { "cannot deliver Back to the focused window" }
    }

    fun home() {
        launchComponent(DESKTOP_PACKAGE, DESKTOP_COMPONENT)
        check(X11WindowControl.activateWindow(DESKTOP_WINDOW_TITLE)) {
            "desktop is running but its window cannot be activated"
        }
    }
}

fun main() {
    val log = Logger.getLogger("navigation")
    val navigation = Navigation(ComponentConfig(componentId = NAVIGATION_COMPONENT))

    try {
        navigation.start()
    } catch (e: Exception) {
        log.severe("navigation: cannot reach control plane: ${e.message}")
        exitProcess(1)
    }

    Runtime.getRuntime().addShutdownHook(Thread { navigation.close() })

    application {
        val scope = rememberCoroutineScope()
        var busy by remember { mutableStateOf(false) }
        var error by remember { mutableStateOf<String?>(null) }
        val graphicsConfiguration = remember {
            GraphicsEnvironment.getLocalGraphicsEnvironment()
                .defaultScreenDevice
                .defaultConfiguration
        }
        val screenBounds = graphicsConfiguration.bounds
        val scaleX = graphicsConfiguration.defaultTransform.scaleX
        val scaleY = graphicsConfiguration.defaultTransform.scaleY

        fun runAction(action: () -> Unit) {
            if (busy) return
            busy = true
            error = null
            scope.launch {
                try {
                    withContext(Dispatchers.IO) { action() }
                } catch (e: Exception) {
                    error = e.message ?: "system navigation failed"
                    log.warning("navigation action failed: ${e.message}")
                } finally {
                    busy = false
                }
            }
        }

        Window(
            onCloseRequest = {
                // This system surface has no user-close lifecycle.
            },
            title = NAVIGATION_WINDOW_TITLE,
            state = rememberWindowState(
                position = WindowPosition(Alignment.CenterEnd),
                width = (NAVIGATION_WIDTH_PX / scaleX).dp,
                height = (screenBounds.height / scaleY).dp,
            ),
            undecorated = true,
            resizable = false,
            focusable = false,
            alwaysOnTop = true,
        ) {
            // Compose state is expressed in dp while Openbox margins are pixels.
            // Pin the actual AWT window to the current X11 screen bounds once so
            // non-1280x800 panels and non-1x scale factors cannot leave a strip
            // uncovered or extend below the display.
            LaunchedEffect(window) {
                val bounds = window.graphicsConfiguration.bounds
                window.setBounds(
                    bounds.x + bounds.width - NAVIGATION_WIDTH_PX,
                    bounds.y,
                    NAVIGATION_WIDTH_PX,
                    bounds.height,
                )
                delay(1_000)
                log.info("navigation renderer=${window.renderApi}")
            }

            NervusTheme {
                NavigationScreen(
                    busy = busy,
                    error = error,
                    onBack = { runAction(navigation::back) },
                    onHome = { runAction(navigation::home) },
                )

                LaunchedEffect(Unit) {
                    while (isActive) {
                        delay(500)
                        if (navigation.state == ConnectionState.DISCONNECTED) {
                            log.severe("navigation: control plane lost, exiting")
                            exitProcess(1)
                        }
                    }
                }
            }
        }
    }
}

const val NAVIGATION_WINDOW_TITLE = "Nervus Navigation"
const val DESKTOP_WINDOW_TITLE = "Nervus"
const val NAVIGATION_WIDTH_PX = 80

private const val DESKTOP_PACKAGE = "nervus.launcher"
private const val DESKTOP_COMPONENT = "desktop"
private const val NAVIGATION_COMPONENT = "navigation"
