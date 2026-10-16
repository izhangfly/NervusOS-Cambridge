pluginManagement {
    repositories {
        // Mirror for mainland China: Aliyun's Gradle plugin repository
        maven("https://maven.aliyun.com/repository/gradle-plugin")
        maven("https://maven.aliyun.com/repository/public")
        gradlePluginPortal()
        mavenCentral()
    }
}

dependencyResolutionManagement {
    repositories {
        // Mirror for mainland China: Aliyun's public repository (central + jcenter combined)
        maven("https://maven.aliyun.com/repository/public")
        maven("https://maven.aliyun.com/repository/central")
        mavenCentral()
    }
}

rootProject.name = "nring"
