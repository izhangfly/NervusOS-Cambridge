# Nervus App Example

A Nervus OS Hello World built with Compose Desktop. It's the smallest complete third-party app: a
manifest, `.nspkg` packaging and signing, and a `NervusApp` with a Compose window.

## Layout

```
nervus-app-example/
├── build.gradle.kts              # build config + nspkg packaging DSL
├── settings.gradle.kts           # includeBuild nervus-app-sdk / nervus-packaging
├── signing/                      # signing key (generate your own; never committed)
│   └── private.pem
└── src/main/kotlin/com/example/hello/
    ├── Greeter.kt                # @Interface definition
    └── app/
        └── HelloApp.kt           # NervusApp + Compose UI
```

## Setup

```bash
# 1. Clone the SDK and packaging next to this project
git clone https://github.com/Nervus-OS/nervus-app-sdk.git
git clone https://github.com/Nervus-OS/nervus-packaging.git

# 2. Generate an Ed25519 signing key
mkdir -p signing
openssl genpkey -algorithm ed25519 -out signing/private.pem

# 3. Build and package
./gradlew nspkg

# Output: build/outputs/nspkg/com.example.hello-1.0.0.nspkg
```

In NervusOS-Cambridge, `scripts/build.sh apps` does all of this and puts the package in the App Store
catalog.

## Dependencies

- JDK 21+
- nervus-app-sdk (component base classes, IPC, UI)
- nervus-packaging (signing-lib + the Gradle packaging plugin)

## Notes

The app defines a `Greeter` interface with `@Interface` and calls it from its Compose window. For now
the Greeter runs in-process: exporting it through the kernel needs a provider schema hash, and the
toolchain that generates `provider.binpb` / `schemas.binpb` for user apps doesn't exist yet. Without
those, the kernel refuses `RegisterEndpoint` with "schema hash does not match". Once nervus-ipc has a
general providergen CLI, add `exports` and `provider { fromProto(...) }` to the manifest and move the
Greeter back behind the interface.
