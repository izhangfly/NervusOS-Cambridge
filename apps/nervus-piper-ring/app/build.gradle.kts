plugins {
    kotlin("jvm") version "2.0.20"
    application
}

repositories { mavenCentral() }

dependencies {
    // App framework (Component / NervusApp / use<>() / Port A IPC). Via composite build.
    implementation("com.nervus.sdk:nervus-app-sdk")

    // Generated protobuf messages: io.github.nervusos.iface.manipulator.v1.* and
    // io.github.nervusos.ipc.v1.* (Envelope, AcquireControl, ...).
    // VERIFY: the exact module coordinates from nervus-ipc/jvm. Placeholder below;
    // replace with the real artifact or an includeBuild dependency.
    // implementation("io.github.nervusos:nervus-ipc-jvm-protocol")
    implementation("com.google.protobuf:protobuf-java:3.25.5")

    implementation("org.jetbrains.kotlinx:kotlinx-coroutines-core:1.8.1")
}

application {
    // Thin-JAR Main-Class for the .nspkg entry (packaging/manifest.json -> entry).
    mainClass.set("com.nervus.piperring.PiperRingAppKt")
}

kotlin { jvmToolchain(17) }
