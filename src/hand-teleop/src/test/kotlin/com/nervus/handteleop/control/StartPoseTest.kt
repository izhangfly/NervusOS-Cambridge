package com.nervus.handteleop.control

import java.io.File
import java.nio.file.Files
import java.nio.file.attribute.PosixFilePermissions
import java.util.logging.Logger
import kotlin.test.Test
import kotlin.test.assertEquals
import kotlin.test.assertNotNull
import kotlin.test.assertNull
import kotlin.test.assertTrue

class StartPoseTest {
    private val log = Logger.getLogger("test")

    @Test
    fun `forward kinematics matches what the arm reported at READY and at joint zero`() {
        // legacy/piper-act/docs/HARDWARE.md: end pose measured on the arm.
        val ready = PiperKinematics.endPose(PiperKinematics.READY_POSE)
        listOf(138.0, 0.0, 418.0).forEachIndexed { i, v -> assertEquals(v, ready[i], 0.5, "READY axis $i") }
        assertEquals(65.0, ready[4], 0.5)
        assertEquals(180.0, kotlin.math.abs(ready[3]), 0.5)
        val zero = PiperKinematics.endPose(List(6) { 0.0 })
        listOf(56.0, 0.0, 213.0).forEachIndexed { i, v -> assertEquals(v, zero[i], 0.5, "zero axis $i") }
        assertEquals(85.0, zero[4], 0.5)
    }

    @Test
    fun `the READY pose is inside the workspace and the folded arm is not`() {
        val ready = PiperKinematics.tcp(PiperKinematics.READY_POSE)
        assertTrue(ready in Teleop.WORKSPACE, "READY at $ready")
        assertNull(StartPoseStore.problem(PiperKinematics.READY_POSE))
        val folded = PiperKinematics.tcp(List(6) { 0.0 })
        assertTrue(folded !in Teleop.WORKSPACE, "folded at $folded")
        assertNotNull(StartPoseStore.problem(List(6) { 0.0 }))
        assertNotNull(StartPoseStore.problem(listOf(0.0, 45.0, -60.0, 0.0, 90.0, 0.0)), "J5 past its limit")
        assertNotNull(StartPoseStore.problem(listOf(0.0, 45.0, -60.0)), "three joints")
    }

    @Test
    fun `a start pose is saved 0644, survives a restart, and resets`() {
        val dir = Files.createTempDirectory("ht").toFile()
        val f = File(dir, "nervus/hand-teleop.json")
        val store = StartPoseStore(f, log)
        assertTrue(store.isDefault)
        val mine = listOf(0.0, 60.0, -60.0, 0.0, 30.0, 0.0) // TCP about (223, 0, 392) mm
        assertNull(store.save(mine))
        assertEquals(mine, store.joints)
        assertEquals("rw-r--r--", PosixFilePermissions.toString(Files.getPosixFilePermissions(f.toPath())))
        assertEquals(mine, StartPoseStore(f, log).joints, "read back after a restart")

        assertNotNull(store.save(List(6) { 0.0 }), "outside the workspace: refused")
        assertEquals(mine, store.joints)

        assertNull(store.reset())
        assertTrue(store.isDefault)
        assertTrue(StartPoseStore(f, log).isDefault)
        dir.deleteRecursively()
    }

    @Test
    fun `a bad saved pose is ignored`() {
        val dir = Files.createTempDirectory("ht").toFile()
        val f = File(dir, "hand-teleop.json")
        f.writeText("""{"start_pose_deg":[0,0,0,0,0,0]}""")
        assertTrue(StartPoseStore(f, log).isDefault, "outside the workspace")
        f.writeText("not json")
        assertTrue(StartPoseStore(f, log).isDefault, "malformed")
        dir.deleteRecursively()
    }
}
