plugins {
    alias(libs.plugins.kotlin.jvm)
    alias(libs.plugins.kotlin.compose)
    alias(libs.plugins.compose)
}

kotlin { jvmToolchain(21) }

// The target ABI decides which platform's skiko is pulled in (Compose's renderer ships native libraries).
// Not compose.desktop.currentOs: that is the build machine's platform, so a cross build would put the
// x86_64 skiko into an arm64 package, and you'd only find out on the device with UnsatisfiedLinkError
val nervusAbi: String = rootProject.extra["nervusAbi"] as String

dependencies {
    // api rather than implementation: the apps downstream use SDK types directly (NervusApp,
    // NervusTheme, ProvidedInterface) and shouldn't have to declare them again
    api("com.nervus.sdk:nervus-app-sdk:0.1.0")
    api(libs.kotlinx.coroutines.core)
    api(
        when (nervusAbi) {
            "linux-arm64" -> compose.desktop.linux_arm64
            "linux-x86_64" -> compose.desktop.linux_x64
            else -> throw GradleException("no compose desktop artifact for abi '$nervusAbi'")
        }
    )
    api(compose.material3)
    testImplementation(libs.kotlin.test)
    // DON'T add compose.materialIconsExtended: that one jar is 37 MB
    // (47% of a 79 MB package) of vector definitions for the entire Material icon set.
    // Compose Desktop has no R8/ProGuard-style shrinking, so adding it ships all of it.
    //
    // The Material Symbols paths the system UI uses live in NervusIcons.kt, so only
    // the dozen or so icons actually drawn end up in the package.
}
