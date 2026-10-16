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
    // and ui-common's AppIdentity both name "nervus.permissionui". The component that provides
    // the permission UI must not be disablable, or users lose the only place to take back sensitive permissions,
    // and nothing reports an error
    packageId = "nervus.permissionui"
    label = "Permissions"
    version = project.version.toString()
    versionCode = 1L
    // Depends on nervus.interface.permission.admin, a built-in interface that exists from API level 2
    minNervusApi = 2
    targetNervusApi = 2
    supportedAbis = listOf(nervusAbi)

    runtimeDeps.minJavaRelease = 21

    // perm.permission.admin: read and write each package's runtime USER_CONSENT grants.
    //
    // It is SYSTEM_ONLY + MinTrust=Platform + [the platform-release signing role].
    // That last part separates this module from settings-app / launcher / filemanager:
    // those sign platform-systemapp, which is Platform trust but not enough for this permission.
    //
    // [That's the design, not a configuration slip.] If "the permission to grant permissions" sat in Settings,
    // "who can give any app the camera and motion control" would be tied to a large package that
    // changes often. Android makes PermissionController a separate APK
    // for the same reason. Settings links here instead of hosting it.
    //
    // [This is the only one.] This package deliberately [holds no] install permissions.
    //
    // Showing and confirming permissions at install time belongs to the package installer, which is a different package
    // (nervus.store in NervusOS Cambridge). Same reasoning as above: "can grant permissions" and "can install anything" are two
    // powerful abilities, and putting them in one process creates a target that can do both. That's exactly why Android
    // ships PermissionController and PackageInstaller as two separate APKs.
    // perm.service.register is [the bar for exporting an interface], unrelated to the above:
    // on RegisterEndpoint the kernel picks the permission ID from the export's visibility and decides
    // (public → perm.service.register, package → .private, see step 3 in
    // endpoint/register.go). This package exports permission.ui with visibility
    // public, so it must hold it.
    //
    // The symptom of leaving it out is hard to guess: the process starts, connects to the control plane, then RegisterEndpoint
    // is refused (missing permission perm.service.register) → the SDK throws → the process exits
    // → the kernel restarts it → refused again, until the breaker trips. The caller meanwhile sees
    // RESOLVE_ENDPOINT_REASON_INTERFACE_NOT_FOUND ("nobody registered this interface"),
    // an error that points nowhere near permissions.
    permissions = listOf("perm.permission.admin", "perm.service.register")

    // Provider contract artifacts (provider.binpb + schemas.binpb).
    //
    // A package that exports interfaces [must] ship them: for "exports but no provider" the kernel's
    // loadRequiredProviderArtifacts returns ErrProviderArtifactsRequired, and the symptom is that
    // the package is quarantined during the boot scan while logging nothing itself.
    //
    // They aren't generated here: both byte streams must be Go's deterministic protobuf encoding,
    // which protobuf-java doesn't guarantee to match byte for byte. nervus-ipc's registry/providerkit
    // generates and commits them, they ship inside the nervus-app-sdk jar, and this only copies them into the package.
    provider { fromClasspath("nervus.permissionui") }

    components.app("main") {
        mainClass = "com.nervus.permissionui.PermissionUiKt"
        runtime = "jvm"
        // on-demand: the kernel starts it when Settings (or another app) resolves this interface.
        //
        // [Must be on-demand, not manual]: Resolve can only start on-demand
        // components. With manual, Settings resolving permission.ui would fail at "the component isn't running",
        // which is exactly the path that "jump from Settings to one package's permissions" needs.
        launchMode = "on-demand"
        // [Deliberately no idle_timeout_sec.]
        //
        // It was once 300 seconds, which was wrong: the kernel's idle timeout only looks at "are there active calls", and a
        // screen the user is looking at [has no active calls]: they're reading permission descriptions, which makes no IPC.
        // The timeout reaped the process along with its window, so the screen vanished in front of the user, and the
        // endpoint cached by Settings went dead, so the next "Open permission manager" got NOT_FOUND.
        //
        // In other words, an idle timeout suits on-demand services with no UI, not components that hold a window:
        // "idle" means "nobody's using it" for the former and "the user is reading" for the latter.
        //
        // The process doesn't stay resident forever because of this: closing the window makes attachComposeDesktop call app.close()
        // and exit (see onCloseRequest in DesktopCompose). After a permission request is answered the screen
        // hides its own window and the process idles, holding one JVM, so the next request reuses it and
        // skips a Compose cold start.
        //
        // [If a timeout ever comes back], the kernel must first count "has a foreground window" as active,
        // or any value will only make this bug harder to reproduce.
        // Exports the permission manager interface (OpenManager).
        //
        // visibility = public: callers are other packages such as Settings, never part of this same
        // package. private is only visible to components in the same package, so nobody could resolve it.
        //
        // This must match the interface declared in provider.binpb [exactly]: the catalog's
        // addArtifacts closes both ways. Something in the descriptor but not in exports, or the
        // reverse, makes the whole Catalog build fail (not just this package being quarantined).
        exports.register("nervus.interface.permission.ui") { visibility = "public" }
        // The kernel doesn't read this field yet (Resolve only checks perm.Allowed); it's filled in so the manifest
        // truthfully says what the component calls
        interfaces = listOf("nervus.interface.permission.admin")
        limits.memoryMaxMb = 512
        limits.tasksMax = 256
    }

    // [The line that differs from the other modules in this repository.] See the permissions note above:
    // platform-release is required to get perm.permission.admin.
    // deploy/build-release.sh copies this key into signing/
    signing.role = "platform-release"
    signing.keyFile = rootProject.file("signing/platform-release.pem")
}
