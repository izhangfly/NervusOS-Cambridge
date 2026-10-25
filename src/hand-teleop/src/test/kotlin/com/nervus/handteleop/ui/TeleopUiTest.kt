package com.nervus.handteleop.ui

import androidx.compose.ui.test.ExperimentalTestApi
import androidx.compose.ui.test.SkikoComposeUiTest
import androidx.compose.ui.test.onNodeWithText
import androidx.compose.ui.test.performClick
import androidx.compose.ui.test.runSkikoComposeUiTest
import com.nervus.handteleop.HandShapes
import com.nervus.handteleop.app.Pipeline
import com.nervus.handteleop.camera.FrameSource
import com.nervus.handteleop.control.DriverArm
import com.nervus.handteleop.control.FakePiper
import com.nervus.handteleop.control.TeleopStatus
import com.nervus.handteleop.math.Vec3
import com.nervus.handteleop.vision.HandLandmarks
import com.nervus.handteleop.vision.Kp
import com.nervus.handteleop.vision.Observation
import com.nervus.handteleop.vision.RgbFrame
import java.util.logging.Logger
import kotlin.test.Test
import kotlin.test.assertEquals

/**
 * The real screen, rendered off-screen, against [FakePiper] (an ephemeral loopback port, never the real
 * driver). The pipeline isn't started: the test runs the control loop's ticks itself, so the flow is
 * deterministic, and clicks the buttons by their words.
 */
@OptIn(ExperimentalTestApi::class)
class TeleopUiTest {
    private val log = Logger.getLogger("test")

    private class StillCamera : FrameSource {
        override fun start() {}
        override fun latest() = RgbFrame(64, 36, IntArray(64 * 36) { 0xFF203040.toInt() }, 0, 1)
        override val fps = 30f
        override val format = "TEST 64x36"
        override val error: String? = null
        override val waiting = "-"
        override fun close() {}
    }

    private fun hand(world: List<Vec3>, at: Long): Observation {
        val f = 0.7 * 1280
        val px = world.map { val c = it + Vec3(0.0, 0.0, 0.5); Kp((f * c.x / c.z + 640).toFloat(), (f * c.y / c.z + 360).toFloat(), 1f) }
        return Observation(RgbFrame(1280, 720, IntArray(0), at, at), null, listOf(HandLandmarks(px, 1f, HandLandmarks.Side.RIGHT, world, 0.5f)), 0f, 0f, atNanos = at)
    }

    private var now = 1_000_000_000L

    private fun SkikoComposeUiTest.tick(p: Pipeline, n: Int = 1) {
        repeat(n) {
            p.teleop.tick(hand(HandShapes.open, now), now)
            now += 50_000_000L
        }
        // The screen copies the status every 33 ms of its own clock.
        mainClock.advanceTimeBy(100)
    }

    @Test
    fun `mirror mode shows the shoulder-to-hand vector and the target before engaging`() = FakePiper(com.nervus.handteleop.control.PiperKinematics.READY_POSE).use { fake ->
        val p = Pipeline(StillCamera(), DriverArm(fake.url, log = log, enableSettleMs = 0), log, standalone = true, settingsFile = null, tickLogDir = null)
        runSkikoComposeUiTest {
            mainClock.autoAdvance = false
            setContent { TeleopScreen(p) }
            repeat(6) {
                p.teleop.tick(com.nervus.handteleop.SyntheticPerson.reaching(1.6, Vec3(0.0, 0.0, -0.40), now), now)
                now += 50_000_000L
            }
            mainClock.advanceTimeBy(100)
            onNodeWithText("MIRROR MY ARM").assertExists()
            onNodeWithText("SHOULDER → HAND  FWD +40 · SIDE +0 · UP +0 CM").assertExists()
            onNodeWithText("TARGET 323 0 123 MM").assertExists()
            onNodeWithText("CALIBRATE ARM").assertExists()
        }
    }

    @Test
    fun `outside the workspace the screen offers the start pose, shows the move, and STOP ends it`() = FakePiper(List(6) { 0.0 }).use { fake ->
        val p = Pipeline(StillCamera(), DriverArm(fake.url, log = log, enableSettleMs = 0), log, standalone = true, settingsFile = null, tickLogDir = null)
        runSkikoComposeUiTest {
            mainClock.autoAdvance = false
            p.settings.controlMode = com.nervus.handteleop.control.ControlMode.RELATIVE // the prompt is the clutch mode's
            setContent { TeleopScreen(p) }
            tick(p, 5)
            onNodeWithText("ARM: MOTORS OFF (ENGAGING TURNS THEM ON)").assertExists()
            onNodeWithText("TCP 138, 0, 418 mm").assertExists()

            onNodeWithText("ENGAGE").performClick()
            tick(p)
            assertEquals(TeleopStatus.Phase.OUTSIDE, p.teleop.status.phase)
            onNodeWithText("THE ARM IS OUTSIDE THE WORKSPACE").assertExists()

            onNodeWithText("MOVE TO START POSE").performClick()
            tick(p, 4)
            assertEquals(TeleopStatus.Phase.MOVING, p.teleop.status.phase, p.teleop.status.message)
            onNodeWithText("MOVING TO THE START POSE · ESC STOPS").assertExists()

            onNodeWithText("STOP").performClick()
            tick(p)
            assertEquals(TeleopStatus.Phase.IDLE, p.teleop.status.phase, p.teleop.status.message)
            onNodeWithText("Move to the start pose stopped").assertExists()
        }
    }
}
