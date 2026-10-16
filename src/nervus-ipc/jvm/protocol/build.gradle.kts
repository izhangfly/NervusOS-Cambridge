import org.gradle.api.tasks.compile.JavaCompile

plugins {
    alias(libs.plugins.kotlin.jvm)
    `java-library`
}

kotlin {
    jvmToolchain(libs.versions.jvmTarget.get().toInt())
}

// Generated code keeps the proto's UTF-8 comments. javac on Windows reads the system code page by default
// (commonly GBK), so it must be pinned explicitly, otherwise the same commit only compiles on Linux.
tasks.withType<JavaCompile>().configureEach {
    options.encoding = "UTF-8"
}

// protobuf-gradle-plugin is deliberately **not** used.
//
// That plugin calls protoc at build time to generate code, requiring every build machine to have protoc installed at
// the same version, the same reason the Go side refuses "generate at build time" (see the root README). Here
// src/main/java is written by `buf generate` and committed to the repository, and Gradle only compiles existing
// source sitting in its default convention directory, with no extra configuration.
//
// The Kotlin plugin is kept: although the generated output is only Java now, this module's golden vectors test
// (src/test/kotlin) is written in Kotlin, and without the plugin the test wouldn't compile.
dependencies {
    // api rather than implementation: the generated message types appear in this module's public signatures
    // (the sdk and Apps hold types such as Envelope and Request directly), so downstream must see the protobuf runtime
    // transitively, or compiling immediately gives "cannot access GeneratedMessage".
    api(libs.protobuf.java)
    // **No longer depends on protobuf-kotlin**: it's the DSL builder runtime, and buf.gen.yaml
    // stopped generating the Kotlin DSL (zero uses in the whole repository; tests all use Java .newBuilder()).

    // The golden vectors test (byte-for-byte Go↔JVM agreement, NRCP §22.6). Test-only dependencies.
    testImplementation(platform(libs.junit.bom))
    testImplementation(libs.junit.jupiter)
    testRuntimeOnly(libs.junit.platform.launcher)
}

// The golden vectors run on the JUnit 5 platform.
tasks.test {
    useJUnitPlatform()
}
