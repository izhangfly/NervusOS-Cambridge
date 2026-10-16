package com.nervus.filemanager

import java.io.IOException
import java.nio.file.Files
import java.nio.file.Path
import java.nio.file.Paths
import java.nio.file.StandardCopyOption
import kotlin.io.path.name

/**
 * File operations in the shared user documents area.
 *
 * ## Scope
 *
 * Everything is confined to [ROOT] = `/var/lib/nervus/user-data`. That's more than a product
 * decision; it's how the sandbox works: `ProtectSystem=strict` makes the whole filesystem read-only, and only packages that
 * declare `perm.storage.user` get this one writable directory (the kernel's
 * `service.readWritePaths`). Writing anywhere else throws AccessDeniedException.
 *
 * ## Why check paths here as well
 *
 * The sandbox already blocks writes outside, but **reads** aren't blocked: under `ProtectSystem=strict` the whole
 * filesystem is still readable, and a path built up to `../../etc/shadow` can be opened. So
 * [resolveInside] limits "what this app is willing to show". It adds to the kernel's enforced isolation;
 * it doesn't replace it.
 *
 * ## A known cost of the permission model
 *
 * The directory is sticky (01777, like `/tmp`): any package with `perm.storage.user`
 * can read other packages' files, just not delete them. That's the shared external storage Android had before
 * scoped storage. Real per-package isolation needs a GID per package plus SupplementaryGroups; that's v2.
 */
object UserStorage {

    val ROOT: Path = Paths.get("/var/lib/nervus/user-data")

    /** Display model for a directory entry. Holds nothing beyond the Path; every listing reads afresh */
    data class Entry(
        val path: Path,
        val name: String,
        val isDirectory: Boolean,
        val sizeBytes: Long,
    )

    /**
     * Resolves a relative segment safely under [base].
     *
     * `normalize()` folds away `..`, and the result must still start with [ROOT]. A `startsWith`
     * without normalizing first can't stop `a/../../etc`.
     *
     * [Path.startsWith] rather than a string prefix: the latter lets
     * `/var/lib/nervus/user-data-evil` pass a check for `/var/lib/nervus/user-data`,
     * while the Path version compares whole path segments and doesn't have that problem.
     */
    fun resolveInside(base: Path, segment: String): Path {
        val resolved = base.resolve(segment).normalize()
        require(resolved.startsWith(ROOT)) { "Path escapes the user folder: $segment" }
        return resolved
    }

    /** Makes sure the root exists. On a dev machine where preflight never ran it may be missing */
    fun ensureRoot(): Boolean =
        try {
            Files.createDirectories(ROOT)
            Files.isWritable(ROOT)
        } catch (e: IOException) {
            false
        }

    fun list(dir: Path): List<Entry> {
        require(dir.normalize().startsWith(ROOT)) { "Folder escapes the user folder: $dir" }
        Files.newDirectoryStream(dir).use { stream ->
            return stream.map { p ->
                val isDir = Files.isDirectory(p)
                Entry(
                    path = p,
                    name = p.name,
                    isDirectory = isDir,
                    // There's no cheap definition of a directory's size (counting recursively would freeze the UI on big folders),
                    // so return -1 and let the UI show "—", rather than something like 4096
                    // that reads as "this folder holds 4 KB"
                    sizeBytes = if (isDir) -1L else runCatching { Files.size(p) }.getOrDefault(-1L),
                )
            }.sortedWith(compareByDescending<Entry> { it.isDirectory }.thenBy { it.name.lowercase() })
        }
    }

    fun createDirectory(parent: Path, name: String): Path {
        val target = resolveInside(parent, name)
        // createDirectory, not createDirectories: the latter succeeds silently on an existing folder,
        // so the user presses New and nothing happens, yet it looks like success
        return Files.createDirectory(target)
    }

    fun rename(target: Path, newName: String) {
        require(target.normalize().startsWith(ROOT)) { "Path escapes the user folder" }
        val parent = target.parent ?: throw IOException("Can't rename the root folder")
        val dest = resolveInside(parent, newName)
        // No REPLACE_EXISTING: renaming onto an existing name would silently overwrite that file,
        // which is data loss. Let it fail with FileAlreadyExistsException and show that in the UI
        Files.move(target, dest, StandardCopyOption.ATOMIC_MOVE)
    }

    /** Recursive delete; non-empty folders are removed completely. The UI confirms before calling this */
    fun delete(target: Path) {
        require(target.normalize().startsWith(ROOT)) { "Path escapes the user folder" }
        require(target != ROOT) { "Can't delete the user folder itself" }
        if (Files.isDirectory(target)) {
            Files.walk(target).use { stream ->
                // Reverse order: children first, then the folder itself
                stream.sorted(Comparator.reverseOrder()).forEach { Files.deleteIfExists(it) }
            }
        } else {
            Files.deleteIfExists(target)
        }
    }

    /** For the breadcrumb: splits an absolute path into segments relative to [ROOT] */
    fun breadcrumb(dir: Path): List<Pair<String, Path>> {
        val rel = ROOT.relativize(dir.normalize())
        val out = mutableListOf("Files" to ROOT)
        var cur = ROOT
        for (seg in rel) {
            if (seg.toString().isEmpty()) continue
            cur = cur.resolve(seg)
            out += seg.toString() to cur
        }
        return out
    }

    fun humanSize(bytes: Long): String = when {
        bytes < 0 -> "—"
        bytes < 1024 -> "$bytes B"
        bytes < 1024 * 1024 -> "%.1f KB".format(bytes / 1024.0)
        bytes < 1024L * 1024 * 1024 -> "%.1f MB".format(bytes / (1024.0 * 1024))
        else -> "%.1f GB".format(bytes / (1024.0 * 1024 * 1024))
    }
}
