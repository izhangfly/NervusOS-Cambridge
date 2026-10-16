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
    packageId = "nervus.launcher"
    label = "Nervus Desktop"
    version = project.version.toString()
    versionCode = 1L
    minNervusApi = 1
    targetNervusApi = 1
    supportedAbis = listOf(nervusAbi)

    runtimeDeps.minJavaRelease = 21

    // perm.system.launch: needed for the desktop to open other apps and for launcherd to bring up the desktop.
    // MinTrust=Platform, so this package must be signed with a platform role (platform-systemapp, see signing.keyFile)
    //
    // perm.pkg.query: resolve the pkgmanager interface and read the installed-app list.
    // Install, uninstall and enabling/disabling components also need perm.pkg.install at method routing;
    // the launcher makes none of those changes, so it doesn't ask for it.
    //
    // perm.authority.power: the restart/power-off buttons in the status bar. Deliberately [not]
    // perm.authority.reboot: that's a reboot(2) hard reset, reserved for platform-release.
    permissions = listOf(
        "perm.system.launch",
        "perm.pkg.query",
        "perm.authority.power",
    )

    // ---- desktop: the desktop itself ----------------------------------------
    //
    // type app: the kernel then wires up X11 (binds /tmp/.X11-unix into the private /tmp from
    // PrivateTmp and injects DISPLAY). Service components don't get that, and Compose can't start.
    components.app("desktop") {
        mainClass = "com.nervus.launcher.DesktopKt"
        runtime = "jvm"
        // manual: no Resolve should start the desktop as a side effect; only launcherd starts it explicitly.
        // LaunchComponent ignores launch_mode, so manual components can still be started.
        // Before the kernel had that message, manual was a dead state (nothing on the wire could start it)
        launchMode = "manual"
        // The desktop reads the installed-app list; restart/power off in the status bar use the built-in power interface.
        //
        // The kernel [doesn't read this field yet] (Resolve only checks perm.Allowed). It's complete because
        // a manifest should say truthfully what a component calls: when imports are enforced one day,
        // a missing entry means a sudden resolve failure at run time, and nobody will think to check a field that never mattered
        interfaces = listOf("nervus.interface.pkg.manager", "nervus.interface.power")
        limits.memoryMaxMb = 512
        limits.tasksMax = 256
    }

    // ---- navigation: the system back/home bar pinned to the right edge --------
    //
    // A manual app like desktop, started explicitly by launcherd after boot. It exports no interface
    // and holds no app permissions of its own. Back is delivered over X11 to the focused window; Home reuses this
    // package's perm.system.launch to make sure desktop exists, then has the window manager activate it.
    components.app("navigation") {
        mainClass = "com.nervus.launcher.NavigationKt"
        runtime = "jvm"
        launchMode = "manual"
        limits.memoryMaxMb = 192
        limits.tasksMax = 128
    }

    // ---- launcherd: the resident service that wakes the desktop -------------
    //
    // [Not] nervus.sessiond: that's a different system service, for HUMAN/AI control-principal sessions
    //
    // The desktop can't start itself: the kernel refuses always-on apps (ErrLaunchModeTypeMismatch).
    // So an always-on service brings the desktop up after boot.
    //
    // criticality optional rather than vital: a vital component tripping its breaker (5 crashes in 10 s)
    // latches Safety for the whole machine and stops the robot. A desktop that won't start is ugly, not dangerous.
    components.service("launcherd") {
        mainClass = "com.nervus.launcher.LauncherdKt"
        runtime = "jvm"
        launchMode = "always-on"
        criticality = "optional"
        interfaces = listOf("nervus.launcher.app")
        limits.memoryMaxMb = 128
        limits.tasksMax = 32
    }

    signing.keyFile = rootProject.file("signing/platform-systemapp.pem")
}
