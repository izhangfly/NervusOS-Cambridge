package com.nervus.handteleop.control

import com.nervus.handteleop.SyntheticPerson
import com.nervus.handteleop.math.Quat
import com.nervus.handteleop.math.Vec3
import kotlinx.serialization.json.JsonArray
import kotlinx.serialization.json.double
import kotlinx.serialization.json.int
import kotlinx.serialization.json.jsonPrimitive
import java.io.File
import java.nio.file.Files
import java.util.logging.Logger
import kotlin.test.AfterTest
import kotlin.test.Test
import kotlin.test.assertEquals
import kotlin.test.assertNotNull
import kotlin.test.assertTrue

/**
 * "Mirror my arm" end to end: Teleop + DriverArm + [FakePiper] (an ephemeral loopback port, never the
 * real driver), with a synthetic operator 1.6 m from a 1280x720 camera and simulated time (50 ms ticks).
 * Reaches are shoulder-to-palm vectors in camera axes: -z is toward the camera (forward), -y up, -x the
 * operator's right.
 */
class MirrorFlowTest {
    private val log = Logger.getLogger("test")
    private val dir: File = Files.createTempDirectory("mirror").toFile()
    private var fake = FakePiper(PiperKinematics.READY_POSE)
    private val settings = TeleopSettings()
    private lateinit var teleop: Teleop
    private var now = 1_000_000_000L

    private fun start(joints: List<Double> = PiperKinematics.READY_POSE) {
        fake.close()
        fake = FakePiper(joints)
        teleop = Teleop(
            DriverArm(fake.url, log = log, enableSettleMs = 0), settings, log, StartPoseStore(null, log),
            BodySizeStore(SettingsFile(File(dir, "hand-teleop.json"))), TickLog(File(dir, "log"), log),
        )
    }

    @AfterTest
    fun stop() {
        fake.close()
        dir.deleteRecursively()
    }

    private fun tick(reach: Vec3?, q: Quat = Quat.IDENTITY, width: Double = 0.37) {
        teleop.tick(reach?.let { SyntheticPerson.reaching(1.6, it, now, q, width = width) }, now)
        now += 50_000_000L
    }

    private fun ticks(n: Int, reach: Vec3?) = repeat(n) { tick(reach) }

    private val phase get() = teleop.status.phase
    private fun lastServo() = (fake.bodies("/api/servo").last()["pose"] as JsonArray).map { it.jsonPrimitive.double }
    private val scale = TeleopSettings.ROBOT_REACH_M / TeleopSettings.DEFAULT_ARM_LENGTH_M

    @Test
    fun `mirror is the default, and shows where the arm would go before engaging`() {
        start()
        assertEquals(ControlMode.MIRROR, settings.controlMode)
        ticks(10, Vec3(0.0, 0.0, -0.40))
        val s = teleop.status
        assertEquals(TeleopStatus.Phase.IDLE, s.phase)
        val r = assertNotNull(s.reach)
        assertEquals(0.40, r.x, 0.01, "forward")
        assertEquals(0.0, r.y, 0.01)
        assertEquals(0.0, r.z, 0.01)
        val g = assertNotNull(s.goal)
        assertEquals(0.123 + 0.0, g.z, 0.01, "level with the robot's shoulder")
        assertEquals(scale * 0.40, g.x, 0.01)
        assertEquals(0, fake.count("/api/servo"), "nothing moves before engaging")
    }

    @Test
    fun `engaging travels to the hand slowly on one goal, then follows every move`() {
        start()
        val forward = Vec3(0.0, 0.0, -0.40)
        ticks(5, forward)
        teleop.engage()
        tick(forward)
        assertEquals(TeleopStatus.Phase.APPROACH, phase, teleop.status.message)
        assertTrue(fake.enabled)

        var i = 0
        while (phase == TeleopStatus.Phase.APPROACH && i++ < 600) tick(forward)
        assertEquals(TeleopStatus.Phase.ENGAGED, phase, teleop.status.message)
        val approach = fake.bodies("/api/servo").dropLast(1)
        assertTrue(approach.size > 20)
        assertEquals(1, approach.map { b -> (b["pose"] as JsonArray).take(3).map { Math.round(it.jsonPrimitive.double) } }.distinct().size,
            "the approach keeps one goal (one plan in the driver)")
        assertTrue(approach.all { it["speed"]!!.jsonPrimitive.int < 40 }, "and goes slowly")
        val t = assertNotNull(fake.tcp)
        assertEquals(scale * 0.40 * 1000, t[0], 20.0)
        assertEquals(123.0, t[2], 20.0)

        // Following: the hand moves 10 cm up and 15 cm to the operator's right over half a second.
        val to = Vec3(-0.15, -0.10, -0.40)
        for (k in 1..10) tick(forward + (to - forward) * (k / 10.0))
        ticks(40, to)
        val p = lastServo()
        assertEquals(scale * 0.40 * 1000, p[0], 5.0)
        assertEquals(-scale * 0.15 * 1000, p[1], 5.0, "the operator's right is the robot's -y (MIRROR ME)")
        assertEquals(123 + scale * 0.10 * 1000, p[2], 5.0)
        assertEquals(62, fake.bodies("/api/servo").last()["speed"]!!.jsonPrimitive.int, "at the normal speed")
        assertEquals(TeleopStatus.Phase.ENGAGED, phase)
    }

    @Test
    fun `a jump is dropped, and a lost hand holds and then travels back slowly`() {
        start()
        val v = Vec3(0.0, -0.05, -0.35)
        ticks(5, v)
        teleop.engage()
        var i = 0
        while (phase != TeleopStatus.Phase.ENGAGED && i++ < 600) tick(v)
        ticks(10, v)
        val before = lastServo()
        tick(Vec3(0.30, -0.05, -0.35)) // 30 cm in one frame: a landmark glitch
        assertTrue("jumped" in teleop.status.message, teleop.status.message)
        assertEquals(before[0], lastServo()[0], 1.0)
        assertEquals(before[1], lastServo()[1], 1.0, "the glitch didn't move the target")
        ticks(5, v)

        val stops = fake.count("/api/stop")
        ticks(10, null)
        assertEquals(TeleopStatus.Phase.HOLD, phase)
        assertTrue(fake.count("/api/stop") > stops, "the arm was stopped")
        tick(Vec3(0.10, -0.05, -0.35))
        tick(Vec3(0.10, -0.05, -0.35))
        assertEquals(TeleopStatus.Phase.APPROACH, phase, "back after a loss: slowly, as when engaging")
    }

    @Test
    fun `a target out of reach is clamped, and the arm follows from where it stopped`() {
        start()
        fake.servoStepMm = 0.0 // the arm gets no closer (as if the planner said partial)
        fake.servoPlan = "partial"
        fake.servoReason = "the goal itself is out of reach"
        val far = Vec3(0.0, 0.0, -0.90)
        ticks(5, far)
        teleop.engage()
        tick(far)
        assertEquals(TeleopStatus.Phase.APPROACH, phase, teleop.status.message)
        var i = 0
        while (phase == TeleopStatus.Phase.APPROACH && i++ < 200) tick(far)
        assertEquals(TeleopStatus.Phase.ENGAGED, phase, "it doesn't wait forever: ${teleop.status.message}")
        assertTrue("short" in teleop.status.message, teleop.status.message)
        val s = teleop.status
        assertTrue(s.clamped)
        assertEquals(0.48, assertNotNull(s.goal).x, 1e-9, "the driver's box")
        assertTrue(s.goalRaw!!.x > 0.6)
        assertEquals("driver: partial (the goal itself is out of reach)", s.servoNote)
    }

    @Test
    fun `a smaller driver box is followed`() {
        start()
        fake.workspaceMm = listOf(60.0, 250.0, -200.0, 200.0, 150.0, 400.0)
        ticks(10, Vec3(0.0, 0.10, -0.40))
        val g = assertNotNull(teleop.status.goal)
        assertEquals(0.25, g.x, 1e-9)
        assertEquals(0.15, g.z, 1e-9)
        assertTrue(teleop.status.clamped)
    }

    @Test
    fun `calibrating measures the arm and saves it`() {
        start()
        teleop.calibrate()
        val out = Vec3(-0.66, 0.0, 0.0) // straight out to the operator's right
        var i = 0
        do { tick(out, Quat.about(Vec3.Z, Math.PI / 2), width = 0.40); i++ } while (phase == TeleopStatus.Phase.CALIBRATING && i < 200)
        assertTrue(teleop.status.message.startsWith("Calibrated: arm 0.66 m, shoulders 0.40 m"), teleop.status.message)
        assertEquals(0.40, settings.shoulderWidthM, 0.01)
        assertEquals(0.5 / 0.66, settings.armScale, 0.02)
        val json = File(dir, "hand-teleop.json").readText()
        assertTrue("\"arm_length_m\":0.66" in json && "\"shoulder_width_m\":0.4" in json, json)
        // And it's used at the next start.
        val again = TeleopSettings()
        Teleop(DriverArm(fake.url, log = log), again, log, StartPoseStore(null, log), BodySizeStore(SettingsFile(File(dir, "hand-teleop.json"))))
        assertEquals(0.5 / 0.66, again.armScale, 0.02)
    }

    @Test
    fun `the session is logged tick by tick`() {
        start()
        val v = Vec3(0.0, 0.0, -0.40)
        ticks(5, v)
        teleop.engage()
        ticks(60, v) // the log is flushed once a second
        val log = File(dir, "log").listFiles()!!.single()
        val lines = log.readLines()
        assertEquals(TickLog.HEADER, lines.first())
        assertTrue(lines.size > 40, "${lines.size} lines")
        val row = lines.last().split(",")
        assertEquals(16, row.size, lines.last())
        assertTrue(row[1] in setOf("APPROACH", "ENGAGED"), row[1])
        assertEquals(0.40, row[3].toDouble(), 0.01, "hand forward")
        assertTrue(row[12].isNotEmpty(), "the TCP")
    }
}
