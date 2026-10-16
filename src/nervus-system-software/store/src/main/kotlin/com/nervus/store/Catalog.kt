package com.nervus.store

import java.nio.file.Files
import java.nio.file.Path
import java.nio.file.Paths

/**
 * One app the store offers.
 *
 * [file] is the .nspkg file name inside the catalog's `packages/` directory.
 * [launchComponent] is the app component the "Open" button starts.
 */
data class CatalogEntry(
    val packageId: String,
    val name: String,
    val file: String,
    val version: String,
    val versionCode: Long,
    val category: String = "",
    val publisher: String = "",
    val summary: String = "",
    val description: String = "",
    val launchComponent: String = "main",
)

/**
 * The store catalog.
 *
 * It lives in the read-only image area so that the store can't be talked into
 * installing something the image didn't ship:
 *
 * ```
 * /usr/share/nervus/store/catalog.ini
 * /usr/share/nervus/store/packages/<file>.nspkg
 * ```
 *
 * The format is a plain INI file, one section per package, so it can be edited
 * by hand on the device and needs no JSON library:
 *
 * ```
 * [com.nervus.robotarm]
 * name = Robot Arm
 * file = com.nervus.robotarm-1.0.0.nspkg
 * version = 1.0.0
 * version_code = 1
 * ```
 *
 * Signatures are not checked here. The kernel verifies every package on
 * Inspect and Install, and the store shows whatever it says.
 */
object Catalog {

    val ROOT: Path = Paths.get("/usr/share/nervus/store")

    fun packagesDir(root: Path = ROOT): Path = root.resolve("packages")

    fun load(root: Path = ROOT): List<CatalogEntry> {
        val file = root.resolve("catalog.ini")
        if (!Files.isRegularFile(file)) return emptyList()
        return parse(Files.readString(file))
    }

    fun parse(text: String): List<CatalogEntry> {
        val sections = linkedMapOf<String, MutableMap<String, String>>()
        var current: MutableMap<String, String>? = null
        for (raw in text.lines()) {
            val line = raw.trim()
            if (line.isEmpty() || line.startsWith("#") || line.startsWith(";")) continue
            if (line.startsWith("[") && line.endsWith("]")) {
                val id = line.substring(1, line.length - 1).trim()
                current = sections.getOrPut(id) { mutableMapOf() }
                continue
            }
            val eq = line.indexOf('=')
            if (eq <= 0 || current == null) continue
            current[line.substring(0, eq).trim().lowercase()] = line.substring(eq + 1).trim()
        }
        return sections.mapNotNull { (id, kv) -> toEntry(id, kv) }
    }

    private fun toEntry(id: String, kv: Map<String, String>): CatalogEntry? {
        val file = kv["file"] ?: return null
        // A catalog entry may only name a file directly inside packages/
        if (file.contains('/') || file.contains("..")) return null
        return CatalogEntry(
            packageId = id,
            name = kv["name"] ?: id,
            file = file,
            version = kv["version"] ?: "",
            versionCode = kv["version_code"]?.toLongOrNull() ?: 0L,
            category = kv["category"] ?: "",
            publisher = kv["publisher"] ?: "",
            summary = kv["summary"] ?: "",
            description = kv["description"] ?: "",
            launchComponent = kv["launch"] ?: "main",
        )
    }
}
