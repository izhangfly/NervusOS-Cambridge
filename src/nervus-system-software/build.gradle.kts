// Shared configuration for the built-in system apps.
//
// The target ABI comes from -Pnervus.abi and defaults to linux-arm64 (real hardware). It decides two things:
//   1. supported_abis in the manifest: the kernel matches it against the host ABI on install
//   2. which platform's skiko is pulled in: Compose Desktop's renderer has platform-specific native libraries
//
// A build parameter rather than compose.desktop.currentOs, because the latter is the BUILD MACHINE's platform.
// Cross-building an arm64 package on x86_64, currentOs quietly adds the x86_64 skiko: the package
// builds and installs, then fails on the device with UnsatisfiedLinkError.
//
//   ./gradlew nspkgImageTree                          → linux-arm64 (device)
//   ./gradlew nspkgImageTree -Pnervus.abi=linux-x86_64 → x86_64 PC (Ubuntu, WSL)
plugins {
    alias(libs.plugins.kotlin.jvm) apply false
    alias(libs.plugins.kotlin.compose) apply false
    alias(libs.plugins.compose) apply false
}

// Matches the canonical ABI tokens in the kernel's pkgregistry/manifest.go.
// It accepts only these three; Android NDK names (arm64-v8a) and bare CPU names (aarch64) are rejected
val supportedAbiTokens = setOf("linux-arm64", "linux-armv7", "linux-x86_64")

val nervusAbi: String = (findProperty("nervus.abi") as String?) ?: "linux-arm64"
require(nervusAbi in supportedAbiTokens) {
    "unknown nervus.abi '$nervusAbi'; expected one of $supportedAbiTokens"
}

// Visible to every subproject
extra["nervusAbi"] = nervusAbi

subprojects {
    group = "com.nervus.system"
    version = "0.1.0"

    // Drop the skiko native libraries for platforms we are NOT targeting.
    //
    // Compose's ui-desktop resolves Gradle variants and brings in a skiko-awt-runtime for the **build machine**,
    // and the target platform one we declare is added on top. The result is that a cross-built
    // package contains both. The x64 one is 13 MB of dead weight on an arm64 device.
    //
    // Worse, it raises no error and doesn't affect running, it just takes space, so it can sit there unnoticed.
    // Measured on a cross build: the arm64 package had 43 jars and the x86_64 one 42; the extra one is this.
    configurations.all {
        val keep = when (nervusAbi) {
            "linux-arm64" -> "skiko-awt-runtime-linux-arm64"
            "linux-x86_64" -> "skiko-awt-runtime-linux-x64"
            else -> null
        }
        if (keep != null) {
            listOf(
                "skiko-awt-runtime-linux-x64",
                "skiko-awt-runtime-linux-arm64",
                "skiko-awt-runtime-macos-x64",
                "skiko-awt-runtime-macos-arm64",
                "skiko-awt-runtime-windows-x64",
            ).filter { it != keep }.forEach { exclude(group = "org.jetbrains.skiko", module = it) }
        }
    }
}
