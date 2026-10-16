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
}

nspkg {
    packageId = "nervus.filemanager"
    label = "Files"
    version = project.version.toString()
    versionCode = 1L
    minNervusApi = 1
    targetNervusApi = 1
    supportedAbis = listOf(nervusAbi)

    runtimeDeps.minJavaRelease = 21

    // perm.storage.user is where all of the file manager's abilities come from.
    //
    // Without it, the sandbox (ProtectSystem=strict) leaves this app a read-only view of the whole
    // filesystem. It can't write anywhere or even create a folder: the window opens and nothing works.
    // Once it is declared, the kernel adds /var/lib/nervus/user-data to ReadWritePaths
    // (see service.readWritePaths)
    permissions = listOf("perm.storage.user")

    components.app("main") {
        mainClass = "com.nervus.filemanager.FileManagerKt"
        runtime = "jvm"
        launchMode = "manual"
        limits.memoryMaxMb = 384
        limits.tasksMax = 128
    }

    signing.keyFile = rootProject.file("signing/platform-systemapp.pem")
}
