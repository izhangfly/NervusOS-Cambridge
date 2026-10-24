package com.nervus.handteleop.control

import com.sun.net.httpserver.HttpServer
import io.github.nervusos.iface.manipulator.v1.ArmReadiness
import kotlinx.serialization.json.Json
import kotlinx.serialization.json.JsonArray
import kotlinx.serialization.json.JsonObject
import kotlinx.serialization.json.double
import kotlinx.serialization.json.int
import kotlinx.serialization.json.jsonPrimitive
import java.net.InetSocketAddress
import java.util.Collections
import java.util.logging.Logger
import kotlin.test.AfterTest
import kotlin.test.Test
import kotlin.test.assertEquals
import kotlin.test.assertFailsWith
import kotlin.test.assertNotNull
import kotlin.test.assertTrue

/**
 * DriverArm against a fake PiPER driver on an ephemeral loopback port. Nothing here talks to the
 * real driver (port 8770) or the arm.
 */
class DriverArmTest {
    private class Call(val path: String, val body: JsonObject)

    /** A stand-in for piper_webctl.py: canned state, scripted replies, and a record of every call. */
    private inner class FakeDriver {
        val calls: MutableList<Call> = Collections.synchronizedList(ArrayList())
        @Volatile var enabled = false
        @Volatile var owner: String? = null
        @Volatile var servoReply: Pair<Int, String>? = null
        val server: HttpServer = HttpServer.create(InetSocketAddress("127.0.0.1", 0), 0)

        init {
            server.createContext("/") { ex ->
                val path = ex.requestURI.path
                val body = ex.requestBody.readBytes().decodeToString()
                val json = if (body.isBlank()) JsonObject(emptyMap()) else Json.parseToJsonElement(body) as JsonObject
                calls += Call(path, json)
                val (code, reply) = when (path) {
                    "/api/state" -> 200 to state()
                    "/api/enable" -> { enabled = true; 200 to """{"ok":true}""" }
                    "/api/servo" -> servoReply ?: if (!enabled) 409 to """{"error":"motors are not enabled"}""" else 200 to """{"ok":true}"""
                    else -> 200 to """{"ok":true}"""
                }
                val bytes = reply.toByteArray()
                ex.sendResponseHeaders(code, bytes.size.toLong())
                ex.responseBody.use { it.write(bytes) }
            }
            server.start()
        }

        fun state() = """{"enabled":$enabled,"feedback_fresh":true,"estopped":false,"teaching":false,
            "soft_fault":false,"arm_status":"NORMAL","pose_current":[250.0,-40.0,310.0,180.0,30.0,0.0],
            "current":[0,45,-60,0,45,0],"gripper_range":[0,95],
            "owner":${owner?.let { """{"client":"$it","kind":"panel"}""" } ?: "null"}}"""

        val url get() = "http://127.0.0.1:${server.address.port}"
        fun paths() = synchronized(calls) { calls.map { it.path } }
        fun last(path: String) = synchronized(calls) { calls.last { it.path == path }.body }
    }

    private val fake = FakeDriver()
    private val arm = DriverArm(fake.url, log = Logger.getLogger("test"), enableSettleMs = 0)

    @AfterTest
    fun stop() = fake.server.stop(0)

    @Test
    fun `state reads the pose in metres and the readiness`() {
        val st = arm.state()
        assertEquals(ArmReadiness.ARM_READINESS_NOT_READY, st.readiness, "motors off")
        assertEquals(0.25, st.tcpPose.x, 1e-12)
        assertEquals(-0.04, st.tcpPose.y, 1e-12)
        assertEquals("base_link", st.tcpPose.frame)
        assertEquals(6, st.joints.positionRadCount)
        fake.enabled = true
        assertEquals(ArmReadiness.ARM_READINESS_READY, arm.state().readiness)
        fake.owner = "act"
        assertEquals(ArmReadiness.ARM_READINESS_NOT_READY, arm.state().readiness, "someone else has it")
    }

    @Test
    fun `acquire enables the motors and servo sends millimetres and degrees as handteleop`() {
        val c = arm.acquire()
        assertTrue(fake.enabled)
        val target = assertNotNull(arm.state().tcpPose).toBuilder().setX(0.3).build()
        arm.servo(target, 0.6)
        val body = fake.last("/api/servo")
        assertEquals("handteleop", body["client"]!!.jsonPrimitive.content)
        val pose = (body["pose"] as JsonArray).map { it.jsonPrimitive.double }
        assertEquals(300.0, pose[0], 1e-9)
        assertEquals(-40.0, pose[1], 1e-9)
        assertEquals(310.0, pose[2], 1e-9)
        val back = PiperPose.eulerToQuat(Math.toRadians(pose[3]), Math.toRadians(pose[4]), Math.toRadians(pose[5]))
        val want = PiperPose.eulerToQuat(Math.toRadians(180.0), Math.toRadians(30.0), 0.0)
        assertEquals(1.0, kotlin.math.abs(back.x * want.x + back.y * want.y + back.z * want.z + back.w * want.w), 1e-9)
        assertEquals(62, body["speed"]!!.jsonPrimitive.int, "5 + 95 x 0.6")
        assertTrue(c.renew())
        c.release()
        assertTrue("/api/heartbeat" in fake.paths() && "/api/release" in fake.paths())
    }

    @Test
    fun `motors that are off get enabled and the servo is sent again`() {
        val target = assertNotNull(arm.state().tcpPose)
        arm.servo(target, 0.5)
        assertEquals(listOf("/api/servo", "/api/state", "/api/enable", "/api/servo"), fake.paths().drop(1))
    }

    @Test
    fun `a busy driver or a tripped IK guard is a refusal with the driver's words`() {
        fake.enabled = true
        val target = assertNotNull(arm.state().tcpPose)
        fake.servoReply = 409 to """{"error":"busy","owner":"panel"}"""
        val busy = assertFailsWith<ArmRefusedException> { arm.servo(target, 0.5) }
        assertEquals("busy: \"panel\" is controlling the arm", busy.message)
        fake.servoReply = 409 to """{"error":"ik guard: J4 swung 80 deg in 0.30 s","guard":true}"""
        val guard = assertFailsWith<ArmRefusedException> { arm.servo(target, 0.5) }
        assertEquals("ik guard: J4 swung 80 deg in 0.30 s", guard.message)
    }

    @Test
    fun `acquire refuses while another client holds the arm`() {
        fake.owner = "panel"
        val e = assertFailsWith<ArmRefusedException> { arm.acquire() }
        assertTrue("panel" in e.message!!)
        assertTrue("/api/enable" !in fake.paths(), "nothing is switched on for someone else's arm")
    }

    @Test
    fun `gripper opening maps onto 0 to 70 mm and effort onto thousandths of a newton metre`() {
        fake.enabled = true
        arm.state()
        arm.gripper(0.5, 1.0)
        val body = fake.last("/api/gripper")
        assertEquals(35.0, body["mm"]!!.jsonPrimitive.double, 1e-9)
        assertEquals(1000, body["effort"]!!.jsonPrimitive.int)
        arm.stop()
        assertEquals("handteleop", fake.last("/api/stop")["client"]!!.jsonPrimitive.content)
    }

    @Test
    fun `an unreachable driver is a refusal at engage, not a crash`() {
        val dead = DriverArm("http://127.0.0.1:1", log = Logger.getLogger("test"))
        assertFailsWith<ArmRefusedException> { dead.acquire() }
    }
}
