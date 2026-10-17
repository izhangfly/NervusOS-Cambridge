package com.nervus.act

import androidx.compose.ui.test.ExperimentalTestApi
import androidx.compose.ui.test.SemanticsNodeInteractionsProvider
import androidx.compose.ui.test.hasSetTextAction
import androidx.compose.ui.test.hasText
import androidx.compose.ui.test.isEnabled
import androidx.compose.ui.test.onAllNodesWithText
import androidx.compose.ui.test.onNodeWithText
import androidx.compose.ui.test.performClick
import androidx.compose.ui.test.performTextInput
import androidx.compose.ui.test.runComposeUiTest
import com.nervus.act.engine.EngineClient
import com.nervus.act.engine.Studio
import com.nervus.act.engine.Tab
import com.nervus.act.engine.arr
import com.nervus.act.engine.get
import com.nervus.act.engine.int
import com.nervus.act.engine.str
import com.nervus.act.ui.StudioScreen
import kotlin.test.Test
import kotlin.test.assertEquals
import kotlin.test.assertTrue

/**
 * Drives ACT Studio's real screens against a running engine, the way a person would: by the words on the
 * buttons. Start a simulated engine first and point the test at it:
 *
 *     PYTHONPATH=act/engine python -m nervus_act --sim --shared /tmp/acttest/NervusACT --port 8795
 *     ACT_ENGINE_TEST=http://127.0.0.1:8795 ./gradlew test
 *
 * Without ACT_ENGINE_TEST the test is skipped.
 */
@OptIn(ExperimentalTestApi::class)
class StudioUiTest {
    private val base = System.getenv("ACT_ENGINE_TEST").orEmpty()

    private fun SemanticsNodeInteractionsProvider.seen(text: String, substring: Boolean = true) =
        onAllNodesWithText(text, substring = substring).fetchSemanticsNodes().isNotEmpty()

    @Test
    fun recordATakeAndQueueATrainingJob() {
        if (base.isEmpty()) return
        val engine = EngineClient(base)
        val studio = Studio(engine, previews = false, pollMs = 5_000)
        val name = "ui_${System.currentTimeMillis() % 100000}"
        studio.start()
        try {
            runComposeUiTest {
                setContent { StudioScreen(studio) }
                waitUntil(timeoutMillis = 15_000L) { studio.connected && seen("ACT STUDIO") }
                assertTrue(seen("SIMULATED ARM"), "the top bar says the arm is simulated")

                // Record: open a dataset, take one drag-taught take.
                onNodeWithText("RECORD").performClick()
                waitUntil(timeoutMillis = 5_000L) { onAllNodes(hasSetTextAction()).fetchSemanticsNodes().size >= 2 }
                onAllNodes(hasSetTextAction())[0].performTextInput(name)
                onAllNodes(hasSetTextAction())[1].performTextInput("move the arm out and back")
                onNodeWithText("Open").performClick()
                waitUntil(timeoutMillis = 60_000L) { onAllNodes(hasText("START", substring = true) and isEnabled()).fetchSemanticsNodes().isNotEmpty() }
                val sim = EngineClient(studio.status["engine"]["sim_driver"].str ?: error("the engine is not running --sim"))
                sim.post("/sim/teach", mapOf("on" to true))
                onNodeWithText("START  (space)").performClick()
                waitUntil(timeoutMillis = 10_000L) { studio.status["recorder"]["state"].str == "recording" }
                // Move the simulated arm by hand for about three seconds.
                for (i in 0..60) {
                    val w = i / 60.0
                    sim.post("/sim/drag", mapOf("joints" to listOf(20 * w, 45 + 20 * w, -60 - 20 * w, 0, 45, 0), "gripper_mm" to 10 + 30 * w))
                    Thread.sleep(50)
                }
                onNodeWithText("STOP  (space)").performClick()
                waitUntil(timeoutMillis = 60_000L) { studio.status["recorder"]["saved_episodes"].int == 1 && studio.status["recorder"]["state"].str == "idle" }
                sim.post("/sim/teach", mapOf("on" to false))
                waitUntil(timeoutMillis = 10_000L) { seen("Saved episode 1") }

                // Data: the dataset and its take are listed.
                onNodeWithText("DATA").performClick()
                waitUntil(timeoutMillis = 10_000L) { seen(name) }
                onNodeWithText(name).performClick()
                waitUntil(timeoutMillis = 20_000L) { seen("Takes in $name") }

                // Train: queue a job for the Mac.
                onNodeWithText("TRAIN").performClick()
                waitUntil(timeoutMillis = 5_000L) { seen("New training job") || seen("NEW TRAINING JOB") }
                onAllNodes(hasSetTextAction())[0].performTextInput("$name-job")
                onNodeWithText(name, substring = true).performClick()
                onNodeWithText("Send to the Mac").performClick()
                waitUntil(timeoutMillis = 15_000L) { studio.jobs.any { it["job"]["name"].str == "$name-job" } }
                val job = studio.jobs.first { it["job"]["name"].str == "$name-job" }
                assertEquals(listOf(name), job["job"]["datasets"].arr.map { it.str })

                // The other tabs render with the engine's data.
                for (t in listOf(Tab.Policies, Tab.Run, Tab.PostTrain, Tab.System, Tab.Overview)) {
                    onNodeWithText(t.label.uppercase()).performClick()
                    waitForIdle()
                }
                waitUntil(timeoutMillis = 5_000L) { seen("Mac worker", substring = true) || seen("MAC WORKER") }
            }
        } finally {
            studio.close()
        }
    }
}
