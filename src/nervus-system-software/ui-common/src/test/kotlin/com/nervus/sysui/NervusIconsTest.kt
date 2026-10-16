package com.nervus.sysui

import androidx.compose.ui.graphics.vector.ImageVector
import kotlin.test.Test
import kotlin.test.assertEquals
import kotlin.test.assertSame

class NervusIconsTest {
    @Test
    fun allMaterialSymbolPathsBuild() {
        val icons: List<ImageVector> = listOf(
            NervusIcons.Apps,
            NervusIcons.Settings,
            NervusIcons.Folder,
            NervusIcons.File,
            NervusIcons.Power,
            NervusIcons.Restart,
            NervusIcons.Info,
            NervusIcons.DeveloperMode,
            NervusIcons.Edit,
            NervusIcons.Delete,
            NervusIcons.CreateFolder,
            NervusIcons.ArrowBack,
            NervusIcons.Home,
            NervusIcons.ChevronRight,
            NervusIcons.Shield,
        )

        assertEquals(15, icons.size)
        assertEquals(icons.size, icons.map(ImageVector::name).distinct().size)
    }

    @Test
    fun builtInPackagesUseStableIcons() {
        assertSame(NervusIcons.Settings, iconForPackage("nervus.settings"))
        assertSame(NervusIcons.Folder, iconForPackage("nervus.filemanager"))
        assertSame(NervusIcons.Apps, iconForPackage("example.third.party"))
    }
}
