plugins {
    alias(libs.plugins.kotlin.jvm)
    alias(libs.plugins.kotlin.compose)
    alias(libs.plugins.compose)
    id("com.nervus.packaging")
}

group = "com.nervus.handteleop"
version = "1.2.0"

// Target ABI of the package. Defaults to the Dell (x86_64); pass -Pnervus.abi=linux-arm64 for RK3588 boards
val nervusAbi: String = (findProperty("nervus.abi") as String?) ?: "linux-x86_64"

// The ONNX Runtime jar is 139 MB: native libraries for every OS plus their debug symbols (a 358 MB
// Windows .pdb alone, uncompressed). The package only needs this device's library, so repackage the
// jar with just that one; ORT finds it at the same resource path.
val ortVersion = "com.microsoft.onnxruntime:onnxruntime:1.22.0"
val ortFull: Configuration by configurations.creating { isTransitive = false }
val ortNativeDir = if (nervusAbi == "linux-arm64") "linux-aarch64" else "linux-x64"
val slimOrt = tasks.register<Jar>("slimOnnxRuntime") {
    archiveFileName.set("onnxruntime-1.22.0-$ortNativeDir.jar")
    destinationDirectory.set(layout.buildDirectory.dir("ort"))
    from({ ortFull.map { zipTree(it) } }) {
        exclude("ai/onnxruntime/native/**")
        exclude("META-INF/*.SF", "META-INF/*.RSA", "META-INF/*.DSA")
    }
    from({ ortFull.map { zipTree(it) } }) {
        include("ai/onnxruntime/native/$ortNativeDir/**")
    }
    duplicatesStrategy = DuplicatesStrategy.EXCLUDE
}

dependencies {
    implementation("com.nervus.sdk:nervus-app-sdk:0.1.0")
    // The generated Camera@1 / Manipulator@1 messages come with the SDK; the runtime they need is
    // declared here because the SDK only uses it internally.
    implementation("com.google.protobuf:protobuf-java:4.29.3")
    implementation(libs.kotlinx.coroutines.core)
    // Standalone mode talks JSON to the PiPER driver (parsed as JSON trees: no compiler plugin needed).
    implementation(libs.kotlinx.serialization.json)
    implementation(compose.desktop.currentOs)
    implementation(compose.material3)
    implementation(
        when (nervusAbi) {
            "linux-arm64" -> "org.jetbrains.skiko:skiko-awt-runtime-linux-arm64:0.8.18"
            else -> "org.jetbrains.skiko:skiko-awt-runtime-linux-x64:0.8.18"
        }
    )
    // YOLO pose + hand landmarks run on the CPU through ONNX Runtime. Compiled against the full jar,
    // but packaged as the slim copy below.
    compileOnly(ortVersion)
    ortFull(ortVersion)
    implementation(files(slimOrt))
    testImplementation(libs.kotlin.test)
    // TeleopUiTest renders the real screen off-screen (Skiko) and clicks its buttons.
    testImplementation(compose.desktop.uiTestJUnit4)
}

kotlin {
    jvmToolchain(21)
    sourceSets.all {
        languageSettings.optIn("androidx.compose.material3.ExperimentalMaterial3Api")
    }
}

tasks.test {
    useJUnitPlatform()
}

compose.desktop {
    application {
        mainClass = "com.nervus.handteleop.app.HandTeleopAppKt"
        // `./gradlew createDistributable`: a self-contained desktop app with its own trimmed Java runtime,
        // for running Hand Teleop with --standalone from a desktop icon (no NervusOS needed). The models
        // ship inside the app jar as resources.
        nativeDistributions {
            packageName = "hand-teleop"
            packageVersion = project.version.toString()
            description = "Drive the PiPER arm with your hand"
            // Beyond Compose's defaults (from suggestRuntimeModules): the driver's HTTP client, and what
            // coroutines, protobuf and ONNX Runtime reach for.
            modules("java.instrument", "java.net.http", "jdk.unsupported", "java.management")
        }
    }
}

nspkg {
    packageId = "com.nervus.handteleop"
    label = "Hand Teleop"
    version = project.version.toString()
    versionCode = 4L
    minNervusApi = 1
    targetNervusApi = 1
    supportedAbis = listOf(nervusAbi)

    runtimeDeps.minJavaRelease = 21

    // Both go through NervusOS, not around it: frames come from nervus.camerad over the Transfer
    // data plane (perm.camera.capture), and the arm is driven through Manipulator@1 under a
    // ControlLease (perm.manipulator.control). Both are USER_CONSENT, so the App Store asks first.
    permissions = listOf(
        "perm.camera.capture",
        "perm.manipulator.control",
    )

    components.app("main") {
        mainClass = "com.nervus.handteleop.app.HandTeleopAppKt"
        runtime = "jvm"
        launchMode = "manual"
        interfaces = listOf(
            "nervus.interface.camera",
            "nervus.interface.manipulator.arm",
        )
    }

    signing.keyFile = file("signing/developer.pem")
    signing.role = "developer"
}
