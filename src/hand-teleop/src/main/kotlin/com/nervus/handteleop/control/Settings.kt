package com.nervus.handteleop.control

import com.nervus.handteleop.math.ArmCalibrator
import com.nervus.handteleop.math.BodySize
import kotlinx.serialization.json.Json
import kotlinx.serialization.json.JsonElement
import kotlinx.serialization.json.JsonObject
import kotlinx.serialization.json.JsonPrimitive
import kotlinx.serialization.json.doubleOrNull
import java.io.File
import java.nio.file.Files
import java.nio.file.StandardCopyOption
import java.nio.file.attribute.PosixFilePermissions

/**
 * ~/.config/nervus/hand-teleop.json: one JSON object in which each part of the app keeps its own keys
 * (the start pose, the body calibration). Read once; every change rewrites the file whole, atomically,
 * mode 0644. With no file (tests), changes stay in memory.
 */
class SettingsFile(val file: File?) {
    private var values: Map<String, JsonElement> = file?.takeIf { it.exists() }?.let { f ->
        runCatching { (Json.parseToJsonElement(f.readText()) as JsonObject).toMap() }.getOrNull()
    } ?: emptyMap()

    @Synchronized
    operator fun get(key: String): JsonElement? = values[key]

    /** Sets [key] (null removes it) and writes the file. Null when done, else why it couldn't be written. */
    @Synchronized
    fun put(key: String, value: JsonElement?): String? {
        val next = if (value == null) values - key else values + (key to value)
        val f = file
        if (f != null) {
            try {
                f.parentFile?.mkdirs()
                val tmp = File(f.parentFile, f.name + ".tmp")
                tmp.writeText(JsonObject(next).toString() + "\n")
                runCatching { Files.setPosixFilePermissions(tmp.toPath(), PosixFilePermissions.fromString("rw-r--r--")) }
                Files.move(tmp.toPath(), f.toPath(), StandardCopyOption.REPLACE_EXISTING, StandardCopyOption.ATOMIC_MOVE)
            } catch (e: Exception) {
                return "could not save $f: ${e.message}"
            }
        }
        values = next
        return null
    }

    companion object {
        fun defaultFile() = File(System.getProperty("user.home"), ".config/nervus/hand-teleop.json")
    }
}

/** The operator's measured size ("Calibrate arm"), kept in the settings file. */
class BodySizeStore(private val settings: SettingsFile) {
    /** The saved calibration, or null if there is none (or it's implausible). */
    fun load(): BodySize? {
        val arm = (settings[ARM] as? JsonPrimitive)?.doubleOrNull ?: return null
        val sh = (settings[SHOULDERS] as? JsonPrimitive)?.doubleOrNull ?: return null
        if (arm !in ArmCalibrator.ARM_RANGE || sh !in ArmCalibrator.SHOULDER_RANGE) return null
        return BodySize(arm, sh)
    }

    fun save(b: BodySize): String? =
        settings.put(ARM, JsonPrimitive(Math.round(b.armLengthM * 1000) / 1000.0))
            ?: settings.put(SHOULDERS, JsonPrimitive(Math.round(b.shoulderWidthM * 1000) / 1000.0))

    companion object {
        const val ARM = "arm_length_m"
        const val SHOULDERS = "shoulder_width_m"
    }
}
