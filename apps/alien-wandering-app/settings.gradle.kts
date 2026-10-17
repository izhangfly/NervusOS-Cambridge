pluginManagement {
    repositories {
        maven { url = uri("https://maven.aliyun.com/repository/public") }
        maven { url = uri("https://maven.aliyun.com/repository/google") }
        gradlePluginPortal()
        mavenCentral()
    }
    // The .nspkg packaging plugin (id "com.nervus.packaging"), built from source.
    includeBuild("../../src/nervus-packaging")
}

dependencyResolutionManagement {
    repositoriesMode.set(RepositoriesMode.FAIL_ON_PROJECT_REPOS)
    repositories {
        maven { url = uri("https://maven.aliyun.com/repository/public") }
        maven { url = uri("https://maven.aliyun.com/repository/google") }
        mavenCentral()
    }
}

rootProject.name = "alien-wandering"

// The NervusOS app framework (com.nervus.sdk:nervus-app-sdk), built from source.
// It in turn copies ../nervus-ipc's generated JVM proto into its own classes, so the
// Manipulator/BaseMotion protobuf types come transitively from this dependency.
includeBuild("../../src/nervus-app-sdk")
