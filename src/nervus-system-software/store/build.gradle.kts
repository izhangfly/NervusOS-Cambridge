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
    testImplementation(libs.kotlin.test)
}


nspkg {
    packageId = "nervus.store"
    label = "App Store"
    version = project.version.toString()
    versionCode = 1L
    minNervusApi = 2
    targetNervusApi = 2
    supportedAbis = listOf(nervusAbi)

    runtimeDeps.minJavaRelease = 21

    // The store is the system's package installer (the role the permission UI
    // leaves to "nervus.packageinstaller"). It shows the permission consent
    // screen itself and then calls INSTALL, which is a needs_user_confirmation
    // method. The kernel only lets callers holding perm.permission.admin make
    // that call, and that permission needs the platform-release signing role.
    //
    // perm.pkg.install / perm.pkg.query: install, uninstall and list packages.
    // perm.storage.user: copy the .nspkg into the shared user-data area, which is
    //   the only place pkgmanagerd accepts packages from (nspkg_relpath).
    // perm.system.launch: the "Open" button.
    permissions = listOf(
        "perm.permission.admin",
        "perm.pkg.install",
        "perm.pkg.query",
        "perm.storage.user",
        "perm.system.launch",
    )

    components.app("main") {
        mainClass = "com.nervus.store.StoreKt"
        runtime = "jvm"
        launchMode = "manual"
        interfaces = listOf("nervus.interface.pkg.manager")
        limits.memoryMaxMb = 512
        limits.tasksMax = 256
    }

    signing.role = "platform-release"
    signing.keyFile = rootProject.file("signing/platform-release.pem")
}
