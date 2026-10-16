package com.nervus.store

import kotlin.test.Test
import kotlin.test.assertEquals
import kotlin.test.assertTrue

class CatalogTest {

    @Test
    fun parsesSectionsAndDefaults() {
        val entries = Catalog.parse(
            """
            # comment
            [com.nervus.robotarm]
            name = Robot Arm
            file = com.nervus.robotarm-1.0.0.nspkg
            version = 1.0.0
            version_code = 3
            summary = Drive the PiPER arm

            [com.example.hello]
            file = com.example.hello-1.0.0.nspkg
            """.trimIndent()
        )
        assertEquals(2, entries.size)
        val arm = entries[0]
        assertEquals("com.nervus.robotarm", arm.packageId)
        assertEquals("Robot Arm", arm.name)
        assertEquals(3L, arm.versionCode)
        assertEquals("main", arm.launchComponent)
        assertEquals("com.example.hello", entries[1].name)
    }

    @Test
    fun rejectsFilesOutsidePackagesDir() {
        val entries = Catalog.parse(
            """
            [evil.one]
            file = ../../etc/passwd
            [evil.two]
            file = sub/dir.nspkg
            [no.file]
            name = nothing
            """.trimIndent()
        )
        assertTrue(entries.isEmpty())
    }
}
