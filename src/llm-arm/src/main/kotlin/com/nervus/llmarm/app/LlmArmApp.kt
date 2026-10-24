package com.nervus.llmarm.app

import androidx.compose.ui.unit.dp
import androidx.compose.ui.window.Window
import androidx.compose.ui.window.application
import androidx.compose.ui.window.rememberWindowState
import com.nervus.llmarm.engine.LlmArm
import com.nervus.llmarm.ui.LlmArmScreen
import com.nervus.sdk.component.ComponentConfig
import com.nervus.sdk.component.InterfaceRequirement
import com.nervus.sdk.component.NervusApp
import com.nervus.sdk.ui.attachComposeDesktop

fun main(args: Array<String>) {
    if ("--standalone" in args) {
        // Development: a plain window, no NervusOS kernel. Point it at any service with LLM_ARM
        // (default http://127.0.0.1:8796), e.g. `python -m nervus_llmarm --sim --demo`.
        val m = LlmArm()
        m.start()
        application {
            Window(onCloseRequest = { m.close(); exitApplication() }, title = "LLM Arm",
                state = rememberWindowState(width = 1500.dp, height = 900.dp)) {
                LlmArmScreen(m)
            }
        }
        return
    }
    val app = LlmArmApp()
    Runtime.getRuntime().addShutdownHook(Thread { app.close() })
    app.start()
}

/**
 * LLM Arm: tell the PiPER what to do in plain language.
 *
 *     this app -(HTTP, loopback)-> nervus-llm-arm (DeepSeek / GPT, tool calls) -> PiPER driver
 *                                         `-> ACT engine cameras (scene, wrist)
 *
 * The app holds no state: the service runs the request, and stops the arm a couple of seconds after the app
 * goes away.
 */
class LlmArmApp : NervusApp(
    config = ComponentConfig(componentId = "main", socketPath = "/run/nervus/nervud.sock"),
) {
    override val requiredInterfaces = emptyList<InterfaceRequirement>()

    private val model = LlmArm()

    override fun onReady() {
        model.start()
        attachComposeDesktop(title = "LLM Arm", width = 1600.dp, height = 900.dp) {
            LlmArmScreen(model)
        }
    }

    override fun onStop() {
        model.close()
    }
}
