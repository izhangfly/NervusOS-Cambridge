package com.nervus.filemanager

import androidx.compose.foundation.background
import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.PaddingValues
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.material3.AlertDialog
import androidx.compose.material3.Card
import androidx.compose.material3.CardDefaults
import androidx.compose.material3.FilledTonalButton
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.material3.ListItem
import androidx.compose.material3.ListItemDefaults
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.Scaffold
import androidx.compose.material3.Surface
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.clip
import androidx.compose.ui.graphics.vector.ImageVector
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import com.nervus.sdk.ui.NervusBackHandler
import com.nervus.sysui.NervusIcons
import com.nervus.sysui.rememberPolled
import java.nio.file.Path

@Composable
fun FileManagerScreen(storageReady: Boolean) {
    var dir by remember { mutableStateOf(UserStorage.ROOT) }
    var error by remember { mutableStateOf<String?>(null) }
    var pendingDelete by remember { mutableStateOf<UserStorage.Entry?>(null) }
    var renaming by remember { mutableStateOf<UserStorage.Entry?>(null) }
    var creatingFolder by remember { mutableStateOf(false) }
    // Bumped after every change so rememberPolled refetches at once. Polling is the fallback (other apps
    // may write to the same shared folder), but your own changes shouldn't wait for the next round
    var revision by remember { mutableStateOf(0) }

    NervusBackHandler(
        enabled = creatingFolder ||
            renaming != null ||
            pendingDelete != null ||
            error != null ||
            dir != UserStorage.ROOT,
    ) {
        when {
            creatingFolder -> creatingFolder = false
            renaming != null -> renaming = null
            pendingDelete != null -> pendingDelete = null
            error != null -> error = null
            dir != UserStorage.ROOT -> dir = dir.parent ?: UserStorage.ROOT
        }
    }

    val entries by rememberPolled<List<UserStorage.Entry>>(
        initial = emptyList(),
        intervalMs = 2000,
        // When the folder changes, or we just changed its contents, refetch now instead of waiting a round
        key = dir to revision,
    ) {
        if (!storageReady) emptyList() else UserStorage.list(dir)
    }

    fun act(block: () -> Unit) {
        error = null
        try {
            block()
            revision++
        } catch (e: Exception) {
            // Shown as-is: names like FileAlreadyExistsException and AccessDeniedException already
            // say what went wrong; rewriting them as "operation failed" loses information
            error = "${e::class.simpleName}: ${e.message}"
        }
    }

    Scaffold(
        containerColor = MaterialTheme.colorScheme.background,
        topBar = {
            Surface(color = MaterialTheme.colorScheme.surface, tonalElevation = 2.dp) {
                Row(
                    Modifier.fillMaxWidth().padding(horizontal = 24.dp, vertical = 14.dp),
                    verticalAlignment = Alignment.CenterVertically,
                    horizontalArrangement = Arrangement.SpaceBetween,
                ) {
                    Row(
                        verticalAlignment = Alignment.CenterVertically,
                        horizontalArrangement = Arrangement.spacedBy(14.dp),
                    ) {
                        Surface(
                            color = MaterialTheme.colorScheme.primaryContainer,
                            shape = RoundedCornerShape(14.dp),
                        ) {
                            Icon(
                                imageVector = NervusIcons.Folder,
                                contentDescription = null,
                                tint = MaterialTheme.colorScheme.onPrimaryContainer,
                                modifier = Modifier.padding(9.dp).size(24.dp),
                            )
                        }
                        Column(verticalArrangement = Arrangement.spacedBy(2.dp)) {
                            Text(
                                "Files",
                                style = MaterialTheme.typography.titleLarge,
                                fontWeight = FontWeight.SemiBold,
                            )
                            Breadcrumb(dir) { dir = it }
                        }
                    }
                    FilledTonalButton(
                        onClick = { creatingFolder = true },
                        enabled = storageReady,
                    ) {
                        Icon(
                            imageVector = NervusIcons.CreateFolder,
                            contentDescription = null,
                            modifier = Modifier.size(20.dp),
                        )
                        Spacer(Modifier.width(8.dp))
                        Text("New folder")
                    }
                }
            }
        },
    ) { pad ->
        Box(Modifier.fillMaxSize().padding(pad)) {
            when {
                !storageReady -> Message(
                    "The user folder isn't writable",
                    "${UserStorage.ROOT}\n" +
                        "Check two things: that the manifest declares perm.storage.user, " +
                        "and that nervud's preflight created this folder (01777).",
                    NervusIcons.Info,
                )

                entries.value.isEmpty() && entries.error != null ->
                    Message("Can't read this folder", entries.error, NervusIcons.Info)

                entries.value.isEmpty() ->
                    Message("Nothing here yet", "Create a folder with the button at the top right", NervusIcons.Folder)

                else -> LazyColumn(
                    modifier = Modifier.fillMaxSize(),
                    contentPadding = PaddingValues(16.dp),
                    verticalArrangement = Arrangement.spacedBy(8.dp),
                ) {
                    items(entries.value, key = { it.path.toString() }) { entry ->
                        Card(
                            colors = CardDefaults.cardColors(
                                containerColor = MaterialTheme.colorScheme.surface,
                            ),
                            elevation = CardDefaults.cardElevation(defaultElevation = 1.dp),
                            shape = RoundedCornerShape(18.dp),
                        ) {
                            ListItem(
                                leadingContent = { TypeGlyph(entry.isDirectory) },
                                headlineContent = {
                                    Text(
                                        entry.name,
                                        maxLines = 1,
                                        overflow = TextOverflow.Ellipsis,
                                    )
                                },
                                supportingContent = {
                                    Text(
                                        if (entry.isDirectory) "Folder"
                                        else UserStorage.humanSize(entry.sizeBytes),
                                        style = MaterialTheme.typography.bodySmall,
                                    )
                                },
                                trailingContent = {
                                    Row(horizontalArrangement = Arrangement.spacedBy(2.dp)) {
                                        IconButton(onClick = { renaming = entry }) {
                                            Icon(
                                                imageVector = NervusIcons.Edit,
                                                contentDescription = "Rename ${entry.name}",
                                            )
                                        }
                                        IconButton(onClick = { pendingDelete = entry }) {
                                            Icon(
                                                imageVector = NervusIcons.Delete,
                                                contentDescription = "Delete ${entry.name}",
                                                tint = MaterialTheme.colorScheme.error,
                                            )
                                        }
                                    }
                                },
                                colors = ListItemDefaults.colors(
                                    containerColor = MaterialTheme.colorScheme.surface,
                                ),
                                modifier = Modifier.clickable(enabled = entry.isDirectory) {
                                    dir = entry.path
                                },
                            )
                        }
                    }
                }
            }

            error?.let { msg ->
                Surface(
                    color = MaterialTheme.colorScheme.errorContainer,
                    shape = MaterialTheme.shapes.medium,
                    modifier = Modifier.align(Alignment.BottomCenter).padding(16.dp).fillMaxWidth(),
                ) {
                    Text(
                        msg,
                        color = MaterialTheme.colorScheme.onErrorContainer,
                        style = MaterialTheme.typography.bodyMedium,
                        modifier = Modifier.padding(16.dp),
                    )
                }
            }
        }
    }

    if (creatingFolder) {
        NameDialog(
            title = "New folder",
            initial = "",
            confirmLabel = "Create",
            onDismiss = { creatingFolder = false },
            onConfirm = { name ->
                creatingFolder = false
                act { UserStorage.createDirectory(dir, name) }
            },
        )
    }

    renaming?.let { entry ->
        NameDialog(
            title = "Rename",
            initial = entry.name,
            confirmLabel = "OK",
            onDismiss = { renaming = null },
            onConfirm = { name ->
                renaming = null
                act { UserStorage.rename(entry.path, name) }
            },
        )
    }

    pendingDelete?.let { entry ->
        AlertDialog(
            onDismissRequest = { pendingDelete = null },
            title = { Text("Delete ${entry.name}?") },
            text = {
                Text(
                    if (entry.isDirectory) "This folder and everything in it will be deleted. This can't be undone."
                    else "This can't be undone."
                )
            },
            confirmButton = {
                TextButton(onClick = {
                    pendingDelete = null
                    act { UserStorage.delete(entry.path) }
                }) { Text("Delete") }
            },
            dismissButton = {
                TextButton(onClick = { pendingDelete = null }) { Text("Cancel") }
            },
        )
    }
}

@Composable
private fun Breadcrumb(dir: Path, onNavigate: (Path) -> Unit) {
    Row(verticalAlignment = Alignment.CenterVertically) {
        UserStorage.breadcrumb(dir).forEachIndexed { i, (label, path) ->
            if (i > 0) {
                Icon(
                    imageVector = NervusIcons.ChevronRight,
                    contentDescription = null,
                    tint = MaterialTheme.colorScheme.onSurfaceVariant,
                    modifier = Modifier.size(16.dp),
                )
            }
            if (i == 0) {
                Icon(
                    imageVector = NervusIcons.Home,
                    contentDescription = null,
                    tint = MaterialTheme.colorScheme.onSurfaceVariant,
                    modifier = Modifier.size(16.dp),
                )
                Spacer(Modifier.width(4.dp))
            }
            Text(
                label,
                style = MaterialTheme.typography.bodySmall,
                fontWeight = if (path == dir) FontWeight.SemiBold else FontWeight.Normal,
                color = MaterialTheme.colorScheme.onSurfaceVariant,
                modifier = Modifier.clickable(enabled = path != dir) { onNavigate(path) },
            )
        }
    }
}

@Composable
private fun TypeGlyph(isDirectory: Boolean) {
    Box(
        Modifier.size(44.dp).clip(RoundedCornerShape(13.dp)).background(
            if (isDirectory) MaterialTheme.colorScheme.primaryContainer
            else MaterialTheme.colorScheme.tertiaryContainer
        ),
        contentAlignment = Alignment.Center,
    ) {
        Icon(
            imageVector = if (isDirectory) NervusIcons.Folder else NervusIcons.File,
            contentDescription = if (isDirectory) "Folder" else "File",
            tint = if (isDirectory) MaterialTheme.colorScheme.onPrimaryContainer
            else MaterialTheme.colorScheme.onTertiaryContainer,
            modifier = Modifier.size(24.dp),
        )
    }
}

@Composable
private fun NameDialog(
    title: String,
    initial: String,
    confirmLabel: String,
    onDismiss: () -> Unit,
    onConfirm: (String) -> Unit,
) {
    var text by remember { mutableStateOf(initial) }
    // A '/' in the name would make resolve leave the current folder; an empty name resolves to the folder itself.
    // UserStorage blocks both anyway, but blocking them here gives more direct feedback
    val valid = text.isNotBlank() && !text.contains('/') && text != "." && text != ".."

    AlertDialog(
        onDismissRequest = onDismiss,
        title = { Text(title) },
        text = {
            Column {
                OutlinedTextField(
                    value = text,
                    onValueChange = { text = it },
                    singleLine = true,
                    isError = text.isNotEmpty() && !valid,
                )
                if (text.isNotEmpty() && !valid) {
                    Text(
                        "A name can't be empty, contain /, or be . or ..",
                        style = MaterialTheme.typography.bodySmall,
                        color = MaterialTheme.colorScheme.error,
                    )
                }
            }
        },
        confirmButton = {
            TextButton(onClick = { onConfirm(text) }, enabled = valid) { Text(confirmLabel) }
        },
        dismissButton = { TextButton(onClick = onDismiss) { Text("Cancel") } },
    )
}

@Composable
private fun Message(title: String, detail: String?, icon: ImageVector) {
    Box(Modifier.fillMaxSize(), contentAlignment = Alignment.Center) {
        Column(
            horizontalAlignment = Alignment.CenterHorizontally,
            verticalArrangement = Arrangement.spacedBy(8.dp),
        ) {
            Icon(
                imageVector = icon,
                contentDescription = null,
                tint = MaterialTheme.colorScheme.onSurfaceVariant,
                modifier = Modifier.size(52.dp),
            )
            Text(title, style = MaterialTheme.typography.titleMedium)
            detail?.let {
                Text(
                    it,
                    style = MaterialTheme.typography.bodySmall,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                    modifier = Modifier.padding(horizontal = 32.dp),
                )
            }
        }
    }
}
