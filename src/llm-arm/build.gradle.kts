plugins {
    alias(libs.plugins.kotlin.jvm)
    alias(libs.plugins.kotlin.compose)
    alias(libs.plugins.compose)
    id("com.nervus.packaging")
}

group = "com.nervus.llmarm"
version = "1.0.0"

// Target ABI of the package. Defaults to the Dell (x86_64); pass -Pnervus.abi=linux-arm64 for RK3588 boards
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
    testImplementation(libs.kotlin.test)
}

kotlin {
    jvmToolchain(21)
    sourceSets.all {
        languageSettings.optIn("androidx.compose.material3.ExperimentalMaterial3Api")
    }
}

tasks.test {
    useJUnitPlatform()
    systemProperty("java.awt.headless", "true")
}

compose.desktop {
    application {
        mainClass = "com.nervus.llmarm.app.LlmArmAppKt"
    }
}

nspkg {
    packageId = "com.nervus.llmarm"
    label = "LLM Arm"
    version = project.version.toString()
    versionCode = 1L
    minNervusApi = 1
    targetNervusApi = 1
    supportedAbis = listOf(nervusAbi)

    runtimeDeps.minJavaRelease = 21

    // LLM Arm drives the arm and reads both robot cameras through its service (nervus-llm-arm.service, on
    // loopback), so the App Store asks for the same consent an app driving them through NervusOS needs.
    permissions = listOf(
        "perm.camera.capture",
        "perm.manipulator.control",
    )

    components.app("main") {
        mainClass = "com.nervus.llmarm.app.LlmArmAppKt"
        runtime = "jvm"
        launchMode = "manual"
        interfaces = listOf()
    }

    signing.keyFile = file("signing/developer.pem")
    signing.role = "developer"
}
