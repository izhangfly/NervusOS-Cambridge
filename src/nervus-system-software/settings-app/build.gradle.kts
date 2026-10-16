plugins {
    alias(libs.plugins.kotlin.jvm)
    alias(libs.plugins.kotlin.compose)
    alias(libs.plugins.compose)
    id("com.nervus.packaging")
}

kotlin { jvmToolchain(21) }

val nervusAbi: String = rootProject.extra["nervusAbi"] as String

dependencies {
    implementation(project(":ui-common"))
    implementation(libs.protobuf.java)
}

nspkg {
    // package_id must [never change]: the kernel's isProtectedComponent in pkgregistry/lifecycle.go
    // puts "nervus.settings/main" on the cannot-be-disabled list.
    // The component that offers the disable UI must not be disablable, or the system loses its way to repair itself.
    // Renaming silently voids that protection, with no error anywhere
    packageId = "nervus.settings"
    label = "Settings"
    version = project.version.toString()
    versionCode = 1L
    minNervusApi = 1
    targetNervusApi = 1
    supportedAbis = listOf(nervusAbi)

    runtimeDeps.minJavaRelease = 21

    // The pkgmanager interface guards Resolve and LIST with perm.pkg.query; UNINSTALL /
    // SET_COMPONENT_ENABLED also need perm.pkg.install, checked each time the method is routed.
    // Settings both shows the list and makes those changes, so it declares both.
    //
    // perm.authority.power: restart/power off on the Power page. MinTrust=Platform (this package is
    // signed platform-systemapp, which is enough). Deliberately [not] perm.authority.reboot:
    // that one is a reboot(2) hard reset, only for platform-release, and belongs to fault recovery.
    //
    // [Deliberately no perm.system.launch.] The permission-manager entry now goes through permission.ui's
    // OpenManager, so it's no longer needed: resolving that interface makes the kernel start
    // permissionui on demand, and OpenManager's required_permission is perm.pkg.query,
    // which is already listed above.
    //
    // The difference is worth noting: perm.system.launch doesn't restrict the target (the kernel's
    // handleLaunchComponent only checks the permission), so holding it can start any component on the machine,
    // whereas Resolve only reaches "the component that exports this interface". Same result, far narrower
    // grant, and it can pass an argument (which package's permissions to show).
    //
    // [Why Settings doesn't host permission management itself]: changing USER_CONSENT grants needs
    // perm.permission.admin, which requires the platform-release signing role, and this module signs
    // platform-systemapp. That is intended, not an oversight (see permissionui's
    // build.gradle.kts), so Settings only links across.
    permissions = listOf(
        "perm.pkg.install",
        "perm.pkg.query",
        "perm.authority.power",
    )

    // The component ID must be main: the protected list says "nervus.settings/main"
    components.app("main") {
        mainClass = "com.nervus.settings.SettingsKt"
        runtime = "jvm"
        // manual: the desktop starts it explicitly with LaunchComponent
        launchMode = "manual"
        // The Power page uses the built-in power interface. The kernel doesn't read this field yet (Resolve only
        // checks perm.Allowed); it's filled in so the manifest truthfully says what the component calls
        interfaces = listOf("nervus.interface.pkg.manager", "nervus.interface.power")
        limits.memoryMaxMb = 512
        limits.tasksMax = 256
    }

    signing.keyFile = rootProject.file("signing/platform-systemapp.pem")
}
