plugins {
    // Automatic Java toolchain provisioning.
    //
    // Without it, jvmTarget in libs.versions.toml degrades into "whichever JDK the build machine happens
    // to have"; with only JDK 21 locally, jvmToolchain(17) fails outright:
    //   Cannot find a Java installation ... matching {languageVersion=17}
    //   Toolchain download repositories have not been configured.
    //
    // With it, Gradle downloads and caches the specified JDK version on demand, so build results no longer depend on
    // differences between machines. For protocol artefacts distributed long term, that reproducibility is necessary.
    id("org.gradle.toolchains.foojay-resolver-convention") version "1.0.0"
}

rootProject.name = "nervus-ipc-jvm"

// Dependency repositories are declared here in one place, rather than scattered across each build.gradle.kts.
//
// FAIL_ON_PROJECT_REPOS makes any subproject that adds its own repositories fail the build.
// For dependencies entering the TCB, "which repository the code comes from" must be one reviewable fact,
// and no submodule can quietly add another source.
dependencyResolutionManagement {
    repositoriesMode = RepositoriesMode.FAIL_ON_PROJECT_REPOS
    repositories {
        mavenCentral()
    }
}

// protocol: the Java + Kotlin types buf generate produces (committed to the repository; see the root README)
include("protocol")

// sdk: the hand-written Kotlin Client / ServiceHost, which also provides a Java API (§10.10).
// Not landed yet; uncomment it when it does.
// include("sdk")
