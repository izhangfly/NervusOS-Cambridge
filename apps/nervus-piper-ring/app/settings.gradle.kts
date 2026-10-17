rootProject.name = "nervus-piper-ring"

// Composite build: the app framework. nervus-app-sdk itself requires nervus-ipc as a
// SIBLING directory (its build.gradle.kts syncs the .proto types), so keep the layout:
//   ~/nervus-app-sdk  ~/nervus-ipc  ~/nervus-piper-ring
includeBuild("../../../src/nervus-app-sdk")
// VERIFY: how the generated JVM proto (io.github.nervusos.*) is exposed as a module.
// If nervus-ipc/jvm is its own Gradle build, add: includeBuild("../../../src/nervus-ipc/jvm")
