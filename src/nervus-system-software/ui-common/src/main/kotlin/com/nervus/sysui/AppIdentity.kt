package com.nervus.sysui

/**
 * Works out from a package_id what the UI shows and what to resolve when it's opened.
 *
 * ## Why a convention instead of reading the manifest
 *
 * The launcher's app list comes from LIST on `nervus.interface.pkg.manager`, and its
 * `PackageInfo` currently has **only** package_id / version / trust / source /
 * granted_permissions / disabled_components: no label, no icon,
 * and nothing saying "which component of this package can be opened, and for which interface".
 *
 * The kernel has that information (the manifest's label and components[].exports); it just isn't
 * projected along `adminwire.PackageInfo` → `pkg_manager.proto` → pkgmanagerd.
 * Adding it means changing three repositories at once, and pkgmanagerd was being developed in parallel,
 * so v1 uses a convention and leaves the cross-repository change until the interfaces settle.
 *
 * ### What to do once it's projected (three steps, in order)
 *
 * 1. `nervud/internal/adminwire/adminwire.go`: add `Label string` and
 *    `Apps []AppEntry{ComponentID, Label, Interface}` to `PackageInfo`,
 *    filled from the manifest in the projection function in `admin/handlers.go`.
 * 2. `nervus-ipc/proto/nervus/interface/pkgmanager/v1/pkg_manager.proto`:
 *    add the matching fields to `PackageInfo`, then `buf generate`.
 * 3. `nervus-system-server/pkgmanagerd`: forward the new fields from List.
 *
 * After that, delete [displayName] and [launchComponentOf] and read the real values from the server.
 * **The trouble with a convention is that it can't express exceptions**: when a package has two app
 * components, the `main` default is bound to pick the wrong one for one of them.
 */
object AppIdentity {

    /**
     * Which component to start when an app is opened.
     *
     * `LaunchComponent` needs an explicit component_id (it doesn't infer "empty = the only app
     * component"), and pkgmanager's `PackageInfo` doesn't project the component list yet,
     * so for now: look up built-in apps in a table, and use `main` for everything else.
     *
     * `main` as the default has a basis: the manifests of `nervus-app-example` and `pkgmanagerd`
     * both use it, so it's the de facto convention. But **a convention can't express exceptions**: with two
     * app components it picks wrong, which is exactly why PackageInfo should project the component list
     * (steps in the class comment above).
     */
    fun launchComponentOf(packageId: String): String =
        builtinComponents[packageId] ?: DEFAULT_COMPONENT

    private const val DEFAULT_COMPONENT = "main"

    private val builtinComponents = mapOf(
        "nervus.launcher" to "desktop",
        "com.example.hello" to "hello_app",
    )

    /** Display names for built-in apps. Third-party apps fall back to [prettify] */
    private val builtinNames = mapOf(
        "nervus.launcher" to "Desktop",
        "nervus.settings" to "Settings",
        "nervus.filemanager" to "Files",
        "nervus.pkgmanagerd" to "Package service",
        "nervus.sessiond" to "Session service",
        "nervus.permissionui" to "Permissions",
        "nervus.safety.recovery" to "Safety recovery",
        "nervus.store" to "App Store",
        "com.nervus.robotarm" to "Robot Arm",
        "com.nervus.handteleop" to "Hand Teleop",
        "com.nervus.act" to "ACT Studio",
        "com.example.hello" to "Nervus Hello World",
    )

    fun displayName(packageId: String): String =
        builtinNames[packageId] ?: prettify(packageId)

    /**
     * `com.example.hello_world` → `Hello World`
     *
     * The last segment because the prefix is a namespace, not a name; swapping `_`/`-` for spaces and capitalising
     * is the least ugly display possible without a label.
     */
    private fun prettify(packageId: String): String =
        packageId.substringAfterLast('.')
            .replace('_', ' ')
            .replace('-', ' ')
            .split(' ')
            .filter { it.isNotEmpty() }
            .joinToString(" ") { w -> w.replaceFirstChar { it.uppercase() } }
            .ifEmpty { packageId }

    /**
     * Whether a package should stay out of the launcher.
     *
     * Background-only services (pkgmanagerd, sessiond...) have no UI; listed, they'd do nothing when tapped.
     * An explicit list rather than a guess like "package_id ends in d", because
     * a wrong guess leaves the user tapping an icon that never opens.
     */
    fun isHeadless(packageId: String): Boolean = packageId in headlessPackages

    private val headlessPackages = setOf(
        "nervus.pkgmanagerd",
        "nervus.sessiond",
        "nervus.safety.recovery",
        // A camera service with no UI
        "nervus.camerad",
        "nervus.piperd",
        // The desktop shouldn't appear on itself
        "nervus.launcher",
    )
}
