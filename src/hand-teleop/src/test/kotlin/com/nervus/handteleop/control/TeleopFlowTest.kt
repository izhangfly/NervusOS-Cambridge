package com.nervus.handteleop.control

import com.nervus.handteleop.HandShapes
import com.nervus.handteleop.math.Vec3
import com.nervus.handteleop.vision.HandLandmarks
import com.nervus.handteleop.vision.Kp
import com.nervus.handteleop.vision.Observation
import com.nervus.handteleop.vision.RgbFrame
import kotlinx.serialization.json.JsonArray
import kotlinx.serialization.json.double
import kotlinx.serialization.json.int
import kotlinx.serialization.json.jsonPrimitive
import java.util.logging.Logger
import kotlin.test.AfterTest
import kotlin.test.Test
import kotlin.test.assertEquals
import kotlin.test.assertNotNull
import kotlin.test.assertTrue

/**
 * The control loop end to end against [FakePiper]: Teleop + DriverArm + the fake driver's HTTP, with
 * synthetic hands in front of a 1280x720 camera and simulated time (one tick = 50 ms).
 */
class TeleopFlowTest {
    private val log = Logger.getLogger("test")
    private val folded = List(6) { 0.0 }
    private var fake = FakePiper(folded)
    private val settings = TeleopSettings()
    private lateinit var teleop: Teleop
    private var now = 1_000_000_000L
    private var seq = 0L

    private fun start(joints: List<Double>) {
        fake.close()
        fake = FakePiper(joints)
        settings.controlMode = ControlMode.RELATIVE // these are the clutch mode's flows; MirrorFlowTest has the other
        teleop = Teleop(DriverArm(fake.url, log = log, enableSettleMs = 0), settings, log, StartPoseStore(null, log))
    }

    @AfterTest
    fun stop() = fake.close()

    /** A right hand seen at 0.5 m in front of the camera, in the shape [world]. */
    private fun seen(world: List<Vec3>): Observation {
        val f = 0.7 * 1280
        val px = world.map { val c = it + Vec3(0.0, 0.0, 0.5); Kp((f * c.x / c.z + 640).toFloat(), (f * c.y / c.z + 360).toFloat(), 1f) }
        val hand = HandLandmarks(px, 1f, HandLandmarks.Side.RIGHT, world, 0.5f)
        return Observation(RgbFrame(1280, 720, IntArray(0), now, ++seq), null, listOf(hand), 0f, 0f, atNanos = now)
    }

    private fun tick(world: List<Vec3>? = HandShapes.open) {
        teleop.tick(world?.let { seen(it) }, now)
        now += 50_000_000L
    }

    private fun ticks(n: Int, world: List<Vec3>? = HandShapes.open) = repeat(n) { tick(world) }

    private val phase get() = teleop.status.phase

    @Test
    fun `engaging outside the workspace prompts, and the prompt's move goes to READY and engages`() {
        start(folded)
        ticks(5)
        teleop.engage()
        tick()
        assertEquals(TeleopStatus.Phase.OUTSIDE, phase, teleop.status.message)
        assertTrue("outside the workspace" in teleop.status.message)
        ticks(20)
        assertEquals(TeleopStatus.Phase.OUTSIDE, phase, "the prompt stays up until it's answered")
        assertEquals(0, fake.count("/api/enable") + fake.count("/api/servo") + fake.count("/api/joint"), "nothing moved or switched on")

        teleop.goToStartPose(engageAfter = true)
        tick()
        assertEquals(TeleopStatus.Phase.MOVING, phase, teleop.status.message)
        assertTrue(fake.enabled)
        assertEquals(DriverArm.MOVE_SPEED, fake.bodies("/api/speed").first()["speed"]!!.jsonPrimitive.int)
        val joints = fake.bodies("/api/joint").associate { it["joint"]!!.jsonPrimitive.int to it["deg"]!!.jsonPrimitive.double }
        assertEquals((1..6).associateWith { PiperKinematics.READY_POSE[it - 1] }, joints)

        // Through the move: heartbeats keep coming (the driver streams joint targets only while one is fresh).
        var lastBeatTick = 0
        var worstGap = 0
        var beats = fake.count("/api/heartbeat")
        var i = 0
        var lastProgress = 0.0
        while (phase == TeleopStatus.Phase.MOVING && i < 2000) {
            tick()
            i++
            val b = fake.count("/api/heartbeat")
            if (b != beats) { worstGap = maxOf(worstGap, i - lastBeatTick); lastBeatTick = i; beats = b }
            teleop.status.moveProgress?.let { assertTrue(it >= lastProgress - 1e-9, "progress only goes up"); lastProgress = it }
        }
        assertTrue(beats > 0)
        assertTrue(worstGap * 50 <= 600, "a heartbeat at least every 0.6 s (was ${worstGap * 50} ms)")
        assertTrue(fake.joints.indices.all { kotlin.math.abs(fake.joints[it] - PiperKinematics.READY_POSE[it]) <= 1.0 }, "at READY: ${fake.joints}")
        assertEquals(15, fake.speed, "the driver's speed is put back")

        // Engaged from there: the arm is anchored where it is, so the first targets are the READY pose.
        ticks(10)
        assertEquals(TeleopStatus.Phase.ENGAGED, phase, teleop.status.message)
        val pose = (fake.bodies("/api/servo").last()["pose"] as JsonArray).map { it.jsonPrimitive.double }
        assertEquals(138.2, pose[0], 1.0)
        assertEquals(0.0, pose[1], 1.0)
        assertEquals(417.9, pose[2], 1.0)
    }

    @Test
    fun `STOP aborts the move and the arm stays where it got to`() {
        start(folded)
        ticks(3)
        teleop.goToStartPose()
        ticks(6)
        assertEquals(TeleopStatus.Phase.MOVING, phase)
        teleop.stopNow()
        tick()
        assertEquals(TeleopStatus.Phase.IDLE, phase)
        assertTrue("stopped" in teleop.status.message, teleop.status.message)
        assertTrue(fake.count("/api/stop") >= 1)
        val where = fake.joints.toList()
        assertTrue(where[1] in 1.0..44.0, "part way: $where")
        ticks(40)
        assertEquals(where, fake.joints.toList(), "nothing moves after STOP")
        assertEquals(15, fake.speed)
    }

    @Test
    fun `a move that doesn't arrive gives up after 25 s`() {
        start(folded)
        fake.stepDeg = 0.0 // the arm doesn't move (blocked, or ignoring us)
        ticks(3)
        teleop.goToStartPose()
        ticks(24 * 20)
        assertEquals(TeleopStatus.Phase.MOVING, phase, "still trying at 24 s")
        ticks(2 * 20)
        assertEquals(TeleopStatus.Phase.ERROR, phase)
        assertTrue("wasn't reached in 25 s" in teleop.status.message, teleop.status.message)
        assertTrue(fake.count("/api/stop") >= 1)
        ticks(40, null)
        assertTrue("wasn't reached" in teleop.status.message, "the reason stays on screen: ${teleop.status.message}")
    }

    @Test
    fun `without engaging after, the move ends at the start pose with control given back`() {
        start(folded)
        ticks(3)
        teleop.goToStartPose()
        tick()
        assertEquals(TeleopStatus.Phase.MOVING, phase)
        var i = 0
        while (phase == TeleopStatus.Phase.MOVING && i++ < 2000) tick()
        assertEquals(TeleopStatus.Phase.IDLE, phase)
        assertTrue("At the start pose" in teleop.status.message, teleop.status.message)
        assertTrue(fake.count("/api/release") >= 1)
        assertEquals(0, fake.count("/api/servo"))
    }

    @Test
    fun `a fist held 1_5 s engages, the open hand then drives, and another fist disengages`() {
        start(PiperKinematics.READY_POSE)
        ticks(3)
        ticks(20, HandShapes.fist) // 1.0 s
        assertTrue(teleop.status.gestureProgress in 0.5..0.8, "counting down: ${teleop.status.gestureProgress}")
        assertEquals(0, fake.count("/api/enable"))
        ticks(12, HandShapes.fist) // past 1.5 s
        assertTrue(fake.enabled, "engaged: motors on")
        assertEquals(TeleopStatus.Phase.HOLD, phase)
        assertTrue("open your hand" in teleop.status.message, teleop.status.message)
        ticks(20, HandShapes.fist)
        assertEquals(0, fake.count("/api/servo"), "nothing moves while the fist is still closed")
        assertEquals(TeleopStatus.Phase.HOLD, phase, "the fist that engaged doesn't disengage")

        ticks(10)
        assertEquals(TeleopStatus.Phase.ENGAGED, phase, teleop.status.message)
        val servos = fake.count("/api/servo")
        assertTrue(servos > 0)

        ticks(10, HandShapes.fist) // a short fist is a clutch
        assertEquals(TeleopStatus.Phase.HOLD, phase)
        assertEquals(servos, fake.count("/api/servo"), "the clutch holds the arm")
        ticks(5)
        assertEquals(TeleopStatus.Phase.ENGAGED, phase, "open again: following")

        ticks(32, HandShapes.fist)
        assertEquals(TeleopStatus.Phase.IDLE, phase, teleop.status.message)
        assertTrue(fake.count("/api/release") >= 1)
    }

    @Test
    fun `a pinch never engages, and the gesture can be switched off`() {
        start(PiperKinematics.READY_POSE)
        ticks(80, HandShapes.pinch)
        assertEquals(TeleopStatus.Phase.IDLE, phase)
        ticks(80, HandShapes.beak)
        assertEquals(TeleopStatus.Phase.IDLE, phase)
        settings.fistGesture = false
        ticks(80, HandShapes.fist)
        assertEquals(TeleopStatus.Phase.IDLE, phase)
        assertEquals(0, fake.count("/api/enable"))
        settings.fistGesture = true
    }

    @Test
    fun `the arm's state is told in words`() {
        start(PiperKinematics.READY_POSE)
        tick()
        assertEquals("motors off (engaging turns them on)", teleop.status.armWords)
        fake.enabled = true
        ticks(5)
        assertEquals("ready", teleop.status.armWords)
        fake.owner = "act"
        ticks(5)
        assertEquals("controlled by another app (act)", teleop.status.armWords)
        assertNotNull(teleop.status.tcp)
    }
}
