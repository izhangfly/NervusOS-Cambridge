plugins {
    alias(libs.plugins.kotlin.jvm)
    alias(libs.plugins.kotlin.compose)
    alias(libs.plugins.compose)
    id("com.nervus.packaging")
}

group = "com.example"
version = "1.0.0"

dependencies {
    implementation("com.nervus.sdk:nervus-app-sdk:0.1.0")
    implementation(libs.kotlinx.coroutines.core)
    implementation(compose.desktop.currentOs)
    implementation(compose.material3)
    testImplementation(libs.kotlin.test)
}

kotlin {
    jvmToolchain(21)
}

compose.desktop {
    application {
        mainClass = "com.example.hello.app.HelloAppKt"
    }
}

nspkg {
    packageId = "com.example.hello"
    label = "Hello World Example"
    version = project.version.toString()
    versionCode = 1L
    minNervusApi = 1
    targetNervusApi = 1
    supportedAbis = listOf("linux-x86_64")

    runtimeDeps.minJavaRelease = 21

    components.app("hello_app") {
        mainClass = "com.example.hello.app.HelloAppKt"
        runtime = "jvm"
        launchMode = "manual"
        interfaces = listOf("com.example.hello.Greeter")
        // TODO(provider-toolchain): add exports + provider { fromProto(...) } once
        // the user-app providergen CLI exists (nervus-ipc issue #xxx).
        // Until then schemaHash stays ByteArray(0) and RegisterEndpoint will be
        // rejected by the schema-hash check — app is not deployable as-is.
    }

    signing.keyFile = file("signing/private.pem")
    signing.role = "developer"
}
