plugins {
    alias(libs.plugins.kotlin.jvm)
    alias(libs.plugins.kotlin.compose)
    alias(libs.plugins.kotlin.serialization)
    alias(libs.plugins.compose)
    id("com.nervus.packaging")
}

group = "com.nervus.robotarm"
version = "1.0.0"

// Target ABI of the package. Defaults to the Dell (x86_64); pass -Pnervus.abi=linux-arm64 for RK3588/RDK boards
val nervusAbi: String = (findProperty("nervus.abi") as String?) ?: "linux-x86_64"

dependencies {
    implementation("com.nervus.sdk:nervus-app-sdk:0.1.0")
    implementation(libs.kotlinx.coroutines.core)
    implementation(libs.kotlinx.serialization.json)
    implementation(compose.desktop.currentOs)
    implementation(compose.material3)
    implementation(
        when (nervusAbi) {
            "linux-arm64" -> "org.jetbrains.skiko:skiko-awt-runtime-linux-arm64:0.8.18"
            else -> "org.jetbrains.skiko:skiko-awt-runtime-linux-x64:0.8.18"
        }
    )
}

kotlin {
    jvmToolchain(21)
    sourceSets.all {
        languageSettings.optIn("androidx.compose.material3.ExperimentalMaterial3Api")
    }
}

compose.desktop {
    application {
        mainClass = "com.nervus.robotarm.app.RobotArmAppKt"
    }
}

nspkg {
    packageId = "com.nervus.robotarm"
    label = "Robot Arm"
    version = project.version.toString()
    versionCode = 1L
    minNervusApi = 1
    targetNervusApi = 1
    supportedAbis = listOf(nervusAbi)

    runtimeDeps.minJavaRelease = 21

    // No permissions: the app only talks to the PiPER driver that NervusOS runs
    // as its hardware layer (nervus-piper.service, HTTP on 127.0.0.1:8770).
    // An earlier version shipped its own always-on service that started the
    // driver from the package data directory; the driver now belongs to the
    // platform, so there is one owner of can0 whichever app is installed.
    permissions = emptyList()

    components.app("main") {
        mainClass = "com.nervus.robotarm.app.RobotArmAppKt"
        runtime = "jvm"
        launchMode = "manual"
    }

    signing.keyFile = file("signing/developer.pem")
    signing.role = "developer"
}
