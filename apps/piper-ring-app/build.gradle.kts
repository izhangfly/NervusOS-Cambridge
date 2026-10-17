plugins {
    alias(libs.plugins.kotlin.jvm)
    alias(libs.plugins.kotlin.compose)
    alias(libs.plugins.compose)
    id("com.nervus.packaging")
}

group = "com.nervus"
version = "0.1.0"

dependencies {
    implementation("com.nervus.sdk:nervus-app-sdk:0.1.0")          // NervusApp + Port A + Manipulator@1 proto
    implementation("com.zilo:nring:0.4.1")                         // Kotlin ring SDK (protocol/codec, via includeBuild)
    implementation(libs.protobuf.java)                             // proto runtime (Manipulator messages)
    implementation(libs.kotlinx.coroutines.core)
    implementation(compose.desktop.currentOs)
    implementation(compose.material3)
    implementation("org.jetbrains.skiko:skiko-awt-runtime-linux-arm64:0.8.18")   // RDK S100 target

    testImplementation(kotlin("test"))
}

kotlin { jvmToolchain(21) }

tasks.test {
    useJUnitPlatform()
    testLogging { showStandardStreams = true; events("passed", "failed") }
}

compose.desktop {
    application { mainClass = "com.nervus.piperring.PiperRingAppKt" }
}

nspkg {
    packageId = "com.nervus.piperring"
    label = "Piper Ring Control"
    version = project.version.toString()
    versionCode = 1L
    icon = "assets/icon.png"
    minNervusApi = 1
    targetNervusApi = 1
    supportedAbis = listOf("linux-arm64")
    runtimeDeps.minJavaRelease = 21

    permissions = listOf("nervus.permission.manipulator.control")

    signing.keyFile = file("keys/signing-key.pem")
    signing.role = "developer"

    components.app("piper_ring") {
        mainClass = "com.nervus.piperring.PiperRingAppKt"
        runtime = "jvm"
        launchMode = "manual"
        interfaces = listOf("nervus.interface.manipulator.arm")
    }
}
