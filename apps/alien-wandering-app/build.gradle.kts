plugins {
    alias(libs.plugins.kotlin.jvm)
    alias(libs.plugins.kotlin.compose)
    alias(libs.plugins.compose)
    id("com.nervus.packaging")            // provides the `nspkg` task
}

group = "com.limo"
version = "0.1.0"

dependencies {
    implementation("com.nervus.sdk:nervus-app-sdk:0.1.0")          // NervusApp + Port A + proto (via includeBuild)
    implementation(libs.protobuf.java)                              // proto runtime (BaseMotion messages; SDK hides it as `implementation`)
    implementation(libs.kotlinx.coroutines.core)
    implementation(compose.desktop.currentOs)                       // Compose Desktop (build-OS Skiko)
    implementation(compose.material3)
    // Target device is the RDK S100 = linux-arm64 → bundle its Skiko native lib too,
    // so the same .nspkg runs on the board (per KOTLIN_APP_DEV_GUIDE §2.4).
    implementation("org.jetbrains.skiko:skiko-awt-runtime-linux-arm64:0.8.18")
}

kotlin { jvmToolchain(21) }

compose.desktop {
    application {
        mainClass = "com.limo.alien.AlienWanderingAppKt"
    }
}

// .nspkg packaging — produces build/outputs/nspkg/com.limo.alien-0.1.0.nspkg
nspkg {
    packageId = "com.limo.alien"
    label = "Alien Wandering — Limo Pro"
    version = project.version.toString()
    versionCode = 1L
    icon = "assets/icon.png"
    minNervusApi = 1
    targetNervusApi = 1
    supportedAbis = listOf("linux-arm64")          // RDK S100 target ABI
    runtimeDeps.minJavaRelease = 21

    permissions = listOf(
        "nervus.permission.motion.control",
        "nervus.permission.perception.read",
    )

    // Ed25519 signing key (PKCS#8 PEM). Dev key generated with:
    //   openssl genpkey -algorithm ed25519 -out keys/signing-key.pem
    signing.keyFile = file("keys/signing-key.pem")
    signing.role = "developer"

    components.app("alien_wandering") {
        mainClass = "com.limo.alien.AlienWanderingAppKt"
        runtime = "jvm"
        launchMode = "manual"
        interfaces = listOf("nervus.interface.motion.base")
    }
}
