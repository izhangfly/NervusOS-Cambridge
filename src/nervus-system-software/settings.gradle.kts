rootProject.name = "nervus-system-software"

// The SDK and the packaging plugin are included as sibling directories (same pattern as nervus-app-example).
// A composite build rather than Maven dependencies: the three repositories evolve together, and going
// through a repository would mean publishing a version for every one-line SDK change
val siblings = rootProject.projectDir.parentFile!!
includeBuild(file("$siblings/nervus-app-sdk"))
includeBuild(file("$siblings/nervus-packaging"))

dependencyResolutionManagement {
    repositoriesMode = RepositoriesMode.FAIL_ON_PROJECT_REPOS
    repositories {
        mavenCentral()
        google()
        // Some Compose Multiplatform artifacts are only in the JetBrains repository
        maven("https://maven.pkg.jetbrains.space/public/p/compose/dev")
    }
}

include(":ui-common")
include(":launcher")
include(":settings-app")
include(":filemanager")

// permissionui and store are signed platform-release (the other system apps sign platform-systemapp).
// They need perm.permission.admin, and that permission requires the platform-release signing role.
// See the notes on permissions in permissionui/build.gradle.kts
include(":permissionui")
include(":store")
