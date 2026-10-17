pluginManagement {
    repositories {
        maven { url = uri("https://maven.aliyun.com/repository/public") }
        maven { url = uri("https://maven.aliyun.com/repository/google") }
        gradlePluginPortal()
        mavenCentral()
    }
    includeBuild("../../src/nervus-packaging")     // the `nspkg` packaging plugin
}

dependencyResolutionManagement {
    repositoriesMode.set(RepositoriesMode.FAIL_ON_PROJECT_REPOS)
    repositories {
        maven { url = uri("https://maven.aliyun.com/repository/public") }
        maven { url = uri("https://maven.aliyun.com/repository/google") }
        mavenCentral()
    }
}

rootProject.name = "piper-ring"

includeBuild("../../src/nervus-app-sdk")           // NervusApp framework + BaseMotion/Manipulator proto
includeBuild("../../src/nring")                    // com.zilo:nring — the Kotlin ring SDK (protocol/codec)
