# Writing a Nervus system app

From an empty folder to a system app running on a Nervus OS device.

Every API signature, DSL field, permission ID and enum value in this guide was checked against
the source of the repository it comes from, not written from memory. If that source changes,
this guide has to change with it, so each item names where it came from.

---

## 0. First, which kind are you writing?

| | System app (this guide) | Ordinary app |
|---|---|---|
| Where it lives | `/usr/lib/nervus/system-packages/`, ships with the image | `/var/lib/nervus/packages/`, installed at run time (App Store or `nervusctl install`) |
| Signing role | `platform-systemapp` | `developer` |
| Trust it gets | `platform` | `ordinary` (**a dynamic install is always ordinary**, whatever it is signed with) |
| Permissions it can get | Including the `MinTrust=Platform` ones | Only `MinTrust=Ordinary` ones |
| Build output | A directory tree (`nspkgImageTree`) | A compressed `.nspkg` (`nspkg`) |
| How it's installed | Image flash / OTA | App Store, or `nervusctl install` |

**What decides it is where the package was installed from, not its signature.** A package signed
`platform-systemapp` that goes through dynamic install still only gets `ordinary`
(`nervud/internal/pkgregistry/arbitrate.go`). There's no "sign an ordinary app with the platform
key to escalate" route.

The packages in this repository (`nervus.launcher`, `nervus.settings`, `nervus.filemanager`,
`nervus.permissionui`, `nervus.store`) are all system apps and good templates.

---

## 1. Adding a system app

Say you want a system monitor called `nervus.monitor`.

### 1.1 Create the folder

```sh
cd nervus-system-software
mkdir -p monitor/src/main/kotlin/com/nervus/monitor
```

### 1.2 Register it in the composite build

Add a line at the end of `settings.gradle.kts`:

```kotlin
include(":monitor")
```

### 1.3 `monitor/build.gradle.kts`

```kotlin
plugins {
    alias(libs.plugins.kotlin.jvm)
    alias(libs.plugins.kotlin.compose)
    alias(libs.plugins.compose)
    id("com.nervus.packaging")
}

kotlin { jvmToolchain(21) }

// The target ABI comes from the root project's -Pnervus.abi; see the top of build.gradle.kts.
// [You must read it]; don't use compose.desktop.currentOs, which is the build machine's platform
val nervusAbi: String = rootProject.extra["nervusAbi"] as String

dependencies {
    // ui-common passes on the SDK, coroutines, compose desktop and material3 as api,
    // so don't declare those again (diverging versions turn into NoSuchMethodError at run time)
    implementation(project(":ui-common"))

    // [Required if you call interfaces]. Interface payloads are protobuf, and the generated classes'
    // parents GeneratedMessage / MessageOrBuilder come from protobuf-java, which the SDK only uses as an
    // implementation dependency and doesn't pass on. Without it you get a string of
    //   Cannot access 'com.google.protobuf.GeneratedMessage' which is a
    //   supertype of '...ListResult'
    // A UI-only module that calls no interfaces (like filemanager) doesn't need this line
    implementation(libs.protobuf.java)
}

nspkg {
    packageId = "nervus.monitor"
    label = "Monitor"
    version = project.version.toString()
    versionCode = 1L
    minNervusApi = 1
    targetNervusApi = 1
    supportedAbis = listOf(nervusAbi)

    runtimeDeps.minJavaRelease = 21

    // See §6. Leaving one out means PERMISSION_DENIED at run time, not a build error
    permissions = listOf("perm.pkg.install", "perm.pkg.query")

    components.app("main") {
        mainClass = "com.nervus.monitor.MonitorKt"
        runtime = "jvm"
        launchMode = "manual"
        interfaces = listOf("nervus.interface.pkg.manager")
        limits.memoryMaxMb = 384
        limits.tasksMax = 128
    }

    signing.keyFile = rootProject.file("signing/platform-systemapp.pem")
}
```

### 1.4 `Monitor.kt`

```kotlin
package com.nervus.monitor

import androidx.compose.material3.Text
import androidx.compose.ui.unit.dp
import com.nervus.sdk.component.ComponentConfig
import com.nervus.sdk.component.InterfaceRequirement
import com.nervus.sdk.component.NervusApp
import com.nervus.sdk.ui.attachComposeDesktop
import java.util.logging.Logger
import kotlin.system.exitProcess

class Monitor(config: ComponentConfig) : NervusApp(config) {

    override val requiredInterfaces: List<InterfaceRequirement> = listOf(
        InterfaceRequirement(id = "nervus.interface.pkg.manager", isRequired = false)
    )

    // call() is protected in NervusApp and the UI code lives outside the class,
    // so wrap every call you need in a public method here. See §4.3
    fun packageCount(): Int =
        ListResult.parseFrom(
            call(
                interfaceId = "nervus.interface.pkg.manager",
                methodId = 3, // LIST
                payload = ListRequest.getDefaultInstance().toByteArray(),
            )
        ).packagesCount
}

fun main() {
    val log = Logger.getLogger("monitor")
    // componentId must match the component ID in the manifest, see §3.3
    val monitor = Monitor(ComponentConfig(componentId = "main"))

    try {
        monitor.start()
    } catch (e: Exception) {
        log.severe("monitor: cannot reach control plane: ${e.message}")
        exitProcess(1)
    }
    Runtime.getRuntime().addShutdownHook(Thread { monitor.close() })

    monitor.attachComposeDesktop(
        title = "Monitor",
        width = 900.dp,
        height = 600.dp,
        onDisconnect = {
            log.severe("monitor: control plane lost, exiting")
            exitProcess(1)
        },
    ) {
        Text("${monitor.packageCount()} packages installed")
    }
}
```

### 1.5 Package it

```sh
./gradlew :monitor:nspkgImageTree -Pnervus.abi=linux-arm64
```

The output lands in `monitor/build/outputs/image-tree/nervus.monitor/` with `manifest.json`,
`manifest.sig` and `lib/*.jar`. `nervus-deploy/build-release.sh` collects every
`*/build/outputs/image-tree/*/` into the release, so **the build script needs no changes**.

---

## 2. The run-time model: how your code gets started

Once this section makes sense, the rules after it stop looking arbitrary.

### 2.1 Who starts you

At boot nervud scans `/usr/lib/nervus/system-packages/*/manifest.json`, verifies signatures, checks
digests and assigns UIDs, then uses `launch_mode` to decide when to start each component:

| launch_mode | When it starts | Allowed for |
|---|---|---|
| `always-on` | At boot, restarted if it crashes | **services only** |
| `on-demand` | When someone resolves an interface it exports | apps and services |
| `manual` | Only when someone calls `LaunchComponent` on it | **apps only** |

The kernel enforces this: an `app` can't be `always-on` and a `service` can't be `manual`
(`ErrLaunchModeTypeMismatch` in `pkgregistry/manifest.go`).

So **anything with a UI that should start at boot has to be split**: an `always-on` service that
wakes it, and one or more `manual` apps that are the UI. `nervus.launcher` does exactly that
(`launcherd` wakes `desktop` and `navigation`).

### 2.2 Every component is its own process

nervud starts each component through systemd's D-Bus API as a **transient unit**:

```
nervus-<package_id>-<component_id>.service
```

It runs under a UID that belongs to its package (20000–59999, one per package, stable across reboots,
never reused) inside a full sandbox. Your `main()` runs in there.

Two components of the same package are **two processes with the same UID**.

### 2.3 The JVM is shared

Components with `runtime = "jvm"` are started by the platform JRE (`/usr/lib/nervus/jre`), so
**apps don't carry their own JVM**. Your package holds only your jars and dependencies.

The Compose Desktop runtime (including the 30 MB skiko native library) **is still one copy per package**,
so a system app with a UI is about 41 MB. A shared runtime is v2 work (see §12).

### 2.4 The handshake checks identity

When a component connects to `/run/nervus/nervud.sock` it reports a `declared_component_id`, and nervud
cross-checks PID → cgroup → unit name. **If that doesn't match, it disconnects with `UNAUTHENTICATED`.**

The consequence: **you can't run a component on its own and connect it to the control plane**.
During development, iterate on the UI with fake data in plain Compose (§8.3), or debug inside an
installed system.

---

## 3. Packages and components

### 3.1 package_id

`validPackageID` in `pkgregistry/manifest.go`:

- Dot-separated, 1–8 segments, at most 128 characters in total
- Each segment uses only `[A-Za-z0-9_-]` and can't start or end with `-` or `_`
- The `nervus.` prefix for system apps is a convention, not a rule

It is also a **directory name** (`/var/lib/nervus/package-data/<package_id>`) and the name of the
**registry file**, which is why the character set is stricter than it looks.

### 3.2 component_id

`validComponentID`: one segment, up to 64 characters, same character set.

**The convention is `main`**: `AppIdentity.launchComponentOf()` in `ui-common` returns `"main"` when
there's no built-in mapping, and the desktop uses it to decide which component to start when an icon is
tapped. If your app component isn't called `main`, add an entry to the builtin mapping in `AppIdentity`,
or the desktop can't open it.

### 3.2.1 Making a new app look right on the desktop

`PackageInfo` doesn't project **label / icon / component list** yet (see §12), so the desktop relies on
the convention tables in `ui-common/AppIdentity.kt`. After adding a package, change these as needed:

| Situation | What to change |
|---|---|
| The app component isn't `main` | Add `"your.package" to "yourComponent"` to `builtinComponents` |
| You want a proper display name | Add an entry to `builtinNames`; otherwise `prettify` is used (`nervus.monitor` → `Monitor`) |
| It's a background-only service that shouldn't show on the desktop | Add it to `headlessPackages` |
| You want an icon | Add a case to `iconForPackage` in `NervusIcons.kt` |

It runs without these changes, just with an ugly name or an icon that won't open. The manifest `icon`
field isn't used by the desktop yet.

### 3.3 componentId must match on both sides

The value in `ComponentConfig(componentId = "main")` must equal the component ID in the manifest.
If not, the handshake check fails: the component exits with `UNAUTHENTICATED` as soon as it starts,
over and over.

### 3.4 Protected components

The kernel refuses to disable these (`isProtectedComponent` in `pkgregistry/lifecycle.go`):

```
nervus.pkgmanagerd/main      the install channel
nervus.settings/main         the component that offers the disable UI can't be disabled
nervus.permissionui/main     the permission consent channel
nervus.sessiond/main
nervus.safety.recovery/main  safety recovery
```

**That's also why the package ID and component ID of `nervus.settings` must never change**: the list is
hard-coded strings.

---

## 4. The SDK

Depend on `com.nervus.sdk:nervus-app-sdk`; in this repository `ui-common` passes it on as `api`.

### 4.1 Two base classes

| | `NervusApp` | `NervusService` |
|---|---|---|
| Role | **Consumes** interfaces | **Provides** interfaces |
| Must implement | `requiredInterfaces` | `providedInterfaces` |
| May also | `providedInterfaces` (optional) | — |

Both extend `Component` and have `start()` / `close()` / `isActive()`. `attachComposeDesktop` hangs off
`Component`, so **either kind can have a UI**.

### 4.2 Members of `NervusApp`

```kotlin
abstract class NervusApp(config: ComponentConfig = ComponentConfig()) : Component(config) {
    protected abstract val requiredInterfaces: List<InterfaceRequirement>
    protected open val providedInterfaces: List<ProvidedInterface> = emptyList()

    protected fun call(
        interfaceId: String,
        methodId: Int,
        payload: ByteArray = ByteArray(0),
        timeoutSeconds: Long = 30,
    ): ByteArray

    protected fun callOperation(   // for methods declared returns_operation
        interfaceId: String,
        methodId: Int,
        payload: ByteArray = ByteArray(0),
        timeoutSeconds: Long = 300,
    ): OperationOutcome

    protected fun launchComponent(
        packageId: String,
        componentId: String,
        timeoutSeconds: Long = 30,
    ): Boolean   // true = it was already running before this call

    protected fun resolveNow(
        interfaceId: String,
        minMajor: Int = 1,
        maxMajor: Int = 1,
        timeoutSeconds: Long = 30,
    ): Long

    protected open fun dispatch(/* when someone calls an interface you export */)
}
```

`InterfaceRequirement`:

```kotlin
InterfaceRequirement(
    id: String,
    minMajor: Int = 1,
    maxMajor: Int = 1,
    resourceType: String = "",   // empty = defaults to nervus.resource.motion.base/main
    resourceRole: String = "",
    isRequired: Boolean = true,  // false = a failed resolve is only a WARNING; the component still starts
)
```

`ComponentConfig`:

```kotlin
ComponentConfig(
    socketPath: String = "/run/nervus/nervud.sock",
    sdkName: String = "nervus-app-sdk",
    sdkVersion: String = "0.1.0",
    handshakeTimeoutMs: Long = 5000,
    autoReconnect: Boolean = true,
    maxReconnectAttempts: Int = 5,
    componentId: String = "",    // required, see §3.3
)
```

### 4.3 `call` is protected, so wrap it

UI code lives outside the class and can't reach `protected fun call`. So **wrap each call you need in a
public method on the component**, and do the protobuf decoding there too:

```kotlin
fun listPackages(): List<PackageInfo> =
    ListResult.parseFrom(
        call(PkgManager.INTERFACE_ID, PkgManager.LIST, ListRequest.getDefaultInstance().toByteArray())
    ).packagesList
```

That isn't working around encapsulation. It's deliberate: calls sit in one place and the UI only sees
methods that mean something. All the existing apps are shaped this way.

### 4.4 Why not the `use<T>()` dynamic proxy

The SDK has a reflection-based `InterfaceProxy`, but in bytecode a Kotlin `suspend fun` returns `Object`
and a `CompletableFuture<T>` returns `CompletableFuture`. **The type argument isn't available at run time**,
so the proxy falls back to returning raw `ByteArray`, and the caller gets a `ClassCastException` on the cast.

System interface payloads are all protobuf, and writing `XxxResult.parseFrom(call(...))` is both precise
and readable. **Use `call` in new code.**

### 4.5 Never call `subscribe`

The SDK has `NervusApp.subscribe()` and `SubscriptionManager`, but **the kernel doesn't implement
Subscribe**: `handleReady` in `ipc/conn.go` sends `Subscribe` / `Unsubscribe` / `Cancel` straight to
`unsupported(env)`, which **closes the connection**.

The symptom is "the connection dropped for no reason", with nothing pointing at subscriptions.

**Any UI that refreshes must poll**, using `rememberPolled` from `ui-common` (§8.1).

---

## 5. Calling system interfaces

### 5.1 Three steps

1. Declare it in `requiredInterfaces`; the SDK resolves it at start-up
2. Declare the permission the interface needs under `permissions` in the manifest (§6)
3. Call it with `call(interfaceId, methodId, payload)`

### 5.2 Existing interfaces

| interface_id | Permission needed | Provided by | method_id defined in |
|---|---|---|---|
| `nervus.interface.pkg.manager` | `perm.pkg.query` (LIST, INSPECT); `perm.pkg.install` for changes | `nervus.pkgmanagerd` | `pkg_manager.proto` in `nervus-ipc` |
| `nervus.interface.safety.control` | `perm.safety.rearm` | **built into the kernel** | `safety_control.proto` |
| `nervus.interface.power` | `perm.authority.power` | **built into the kernel** | hand-written constants, see below |
| `nervus.interface.motion.base` | `perm.motion.control` | OEM Provider | `motion.proto` |
| `nervus.interface.manipulator.arm` | `perm.manipulator.control` | OEM Provider | `manipulator.proto` |

The single source of truth for those thresholds is the kernel's catalog (`nervud/internal/catalog/`).

> ⚠️ **An interface_id is not the proto's directory name.**
> The proto for `nervus.interface.pkg.manager` lives in `nervus/interface/pkgmanager/v1/`, with
> **no dot** in the middle. Copying the directory name as `nervus.interface.pkgmanager` fails to resolve,
> and the only symptom is `failed to resolve optional interface`, which doesn't look like a typo.
> This has bitten us on real hardware.

### 5.3 Install and other user-confirmed methods

Methods marked `needs_user_confirmation` in the proto (pkg_manager INSTALL, UNINSTALL,
SET_COMPONENT_ENABLED) are only accepted from a caller holding `perm.permission.admin`, which means
the platform-release signing role. The App Store (`store/`) is the example: it shows its own consent
screen, then calls INSTALL through `callOperation`, since INSTALL is declared `returns_operation`.

### 5.4 Built-in interfaces

`nervus.interface.safety.control` and `nervus.interface.power` are implemented by nervud itself
(`endpoint/builtin.go`). Callers use the **standard** Resolve + call and can't tell whether the other
end is the kernel or an external Provider.

The method IDs of `nervus.interface.power` have **no proto behind them**. The only definition is in
`nervud/internal/power/builtin.go`, mirrored on the Kotlin side in `ui-common/.../PowerControl.kt`. Use
`PowerControl` rather than copying the numbers again. In hosted mode (NervusOS as a session on Ubuntu),
the kernel turns power off into "exit NervusOS" and reboot into "restart NervusOS"; `PowerControl.hosted`
tells the UI which wording to use.

### 5.5 Starting another app

```kotlin
fun launch(packageId: String, componentId: String) {
    val alreadyRunning = launchComponent(packageId, componentId)
}
```

This uses the Envelope's `LaunchComponent` (body 80) and needs `perm.system.launch`
(`MinTrust=Platform`, so only system apps can use it).

Usually get the component ID from `AppIdentity.launchComponentOf(packageId)`.

---

## 6. Permissions

### 6.1 The full catalog

Source of truth: the kernel's permission catalog (`nervud/internal/catalog/bootstrap.go`).

| Permission ID | MinTrust | Granted | Meaning |
|---|---|---|---|
| `perm.diagnostics.read` | Ordinary | install | Read diagnostics |
| `perm.service.register.private` | Ordinary | install | Register interfaces visible only inside the package |
| `perm.service.register` | **OEM** | install | Register public interfaces |
| `perm.storage.user` | Ordinary | install | The shared user folder `/var/lib/nervus/user-data` |
| `perm.pkg.query` | Ordinary | install | List installed packages, inspect a package |
| `perm.pkg.install` | Ordinary | **user** | Install / uninstall packages |
| `perm.camera.capture` | Ordinary | user | Camera |
| `perm.motion.control` | Ordinary | user | Base motion |
| `perm.manipulator.control` | Ordinary | user | Robot arm |
| `perm.bluetooth.admin` | Ordinary | user | Bluetooth management |
| `perm.network.admin` | Ordinary | user | Network management |
| `perm.safety.observe` | **OEM** | install | Read safety state |
| `perm.system.launch` | **Platform** | install | Start other components |
| `perm.platform.control` | **Platform** | signature | Platform control (placeholder) |
| `perm.authority.power` | **Platform** | signature | Orderly restart / power off |
| `perm.authority.reboot` | **Platform** | signature | `reboot(2)` hard reset, **platform-release only** |
| `perm.safety.rearm` | **Platform** | signature | Clear a safety stop latch, **platform-release only** |
| `perm.permission.admin` | **Platform** | system only | Manage other apps' runtime grants, **platform-release only** |

A system app (signed `platform-systemapp`, so `platform` trust) can have everything except the
platform-release-only ones. Those carry `RequireSignerRole: "platform-release"` and are only for the
kernel, core system services, the permission UI and the App Store.

### 6.2 V1 today: asking is getting

`permission.V1GrantAll = true`: there's no user confirmation at run time, and `GrantUser`
permissions are granted directly.

**You still have to declare them in the manifest.** `permission.Allowed` skips the runtime grant state,
not the check against the installed set. Leave one out and you get `PERMISSION_DENIED` at run time,
**with no build error**.

This is the easiest trap to fall into: the code is right, the interface ID is right, one line is missing
from the manifest, the UI shows nothing, and the log has a single warning.

### 6.3 Restart vs power off: choose the right one

| | `perm.authority.reboot` | `perm.authority.power` |
|---|---|---|
| Action | `reboot(2)` | systemd `shutdown.target` |
| Components get SIGTERM | No | Yes |
| Filesystems unmount cleanly | No | Yes |
| Who can have it | platform-release only | any Platform trust |
| Use | Fault recovery | **The button the user presses** |

For a power button in a system app, use `perm.authority.power` and reuse `PowerControl` +
`PowerConfirmDialog` from `ui-common`.

---

## 7. The sandbox: what you can touch

A component's transient unit carries these (`nervud/internal/authority/systemd/props.go`):

```
NoNewPrivileges=yes
ProtectSystem=strict          the whole filesystem is read-only
ProtectHome=yes               /home and /root are not accessible at all
PrivateTmp=yes                a private /tmp
PrivateDevices=yes            no device nodes (system image packages can open this up)
DevicePolicy=closed
SystemCallFilter=@system-service
RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6
```

### 7.1 Where you can write

`readWritePaths` in `service/supervise.go`:

| Path | Condition |
|---|---|
| `/var/lib/nervus/package-data/<package_id>` | Always. **Your private data directory** |
| `/var/lib/nervus/user-data` | If you declared `perm.storage.user` |
| The staging root | Only `nervus.pkgmanagerd` |

**Nowhere else is writable**, even if the owner and mode look right: `ProtectSystem=strict` is a
separate, independent barrier.

### 7.2 The JVM temp directory is taken care of

For `runtime=jvm` components the kernel injects:

```
-Djava.io.tmpdir=<private data directory>
-Duser.home=<private data directory>
```

**Compose depends on this**: skiko extracts a 30 MB `.so` from its jar and dlopens it, and the default
tmpdir isn't writable under strict.

### 7.3 Graphics

For `type = "app"` components the kernel automatically:

- bind-mounts `/tmp/.X11-unix` into the private `/tmp`
- injects `DISPLAY` and `XAUTHORITY` (taken from nervud's own environment)
- bind-mounts the cookie file as well

`type = "service"` components **get none of this**, and Compose can't start. If it has a UI, make it an `app`.

### 7.4 What you can't read

`/var/lib/nervus/registry` (the kernel's records) is `InaccessiblePaths`, so no component can read it.
To find out what's installed, use the pkgmanager interface; don't try to read the files.

---

## 8. UI

### 8.1 `rememberPolled`, the one way to refresh

```kotlin
val apps by rememberPolled<List<PackageInfo>>(
    initial = emptyList(),
    intervalMs = 3000,
    key = currentDir,        // a new key restarts the coroutine
) {
    desktop.listApps()       // runs on Dispatchers.IO for you
}

apps.value      // the last successful value
apps.loading    // fetching now
apps.error      // the last failure; a failure [doesn't clear] the previous value
```

> ⚠️ **Don't drop `key`.** Inside it's `LaunchedEffect(intervalMs, key)`. With only `intervalMs`, a
> change to a variable the closure captured (say, the current folder) doesn't restart the coroutine,
> and the UI keeps showing old data. The file manager's folder navigation broke exactly this way.

### 8.2 Material 3

`attachComposeDesktop` already wraps you in `NervusTheme` (a `MaterialTheme` with light and dark
colour schemes), so **use `MaterialTheme.colorScheme` / `typography` directly** without another wrapper.

> ⚠️ **Don't add `compose.materialIconsExtended`.** That one jar is 37 MB (47% of a package), and Compose
> Desktop has no R8-style shrinking, so all of it ships. Take icons from `Icons.Default` in `material3`,
> or copy the vector data of the few unusual ones you need into the project (see `NervusIcons.kt`).

### 8.3 Running the UI without the kernel

The fastest loop: keep a `main()` that only runs Compose and feeds it fake data.

```kotlin
fun main() = application {
    Window(onCloseRequest = ::exitApplication) {
        NervusTheme { MonitorScreen(fakeData()) }
    }
}
```

Because of the handshake check in §2.4, **there's no other way to run a component on its own on a dev machine**.

### 8.4 Hooking into the system Back button

The navigation bar on the right sends `Escape` to the foreground X11 window. Apps using
`attachComposeDesktop` don't listen to the keyboard themselves; they register a handler for the current page:

```kotlin
NervusBackHandler(enabled = dialogOpen || currentDirectory != rootDirectory) {
    when {
        dialogOpen -> dialogOpen = false
        currentDirectory != rootDirectory -> currentDirectory = currentDirectory.parent
    }
}
```

Handlers can nest within one window; the most recently registered `enabled` one wins. When a page has
nothing to go back to, the root fallback in `attachComposeDesktop(onUnhandledBack = { ... })` decides.
The built-in apps use it to hide their root window and reveal the one underneath; the desktop simply
consumes Back.

When an app is started again from the desktop, nervud reports `alreadyRunning`, and the desktop then
activates the existing window by its title. That's why a built-in app's title must stay equal to
`AppIdentity.displayName(packageId)`. Don't implement Back or Home by exiting the process; nervud owns
process lifecycles.

---

## 9. Build, sign, deploy

### 9.1 Two tasks

| Task | Output | Signing role |
|---|---|---|
| `nspkgImageTree` | Directory tree in `build/outputs/image-tree/<pkg>/` | `platform-systemapp` (default) |
| `nspkg` | Compressed `.nspkg` | `developer` |

System apps use the first. `nspkgImageTree` **refuses the `developer` role**: the wrong role would install
with trust dropped to `ordinary`, a symptom far from its cause, so it fails at build time instead.

### 9.2 ABI

```sh
./gradlew nspkgImageTree                            # linux-arm64 (default, device)
./gradlew nspkgImageTree -Pnervus.abi=linux-x86_64  # x86_64 PC (Ubuntu, WSL)
```

It decides both `supported_abis` (which the kernel matches against the host ABI on install) and which
platform's skiko is pulled in. **Don't use `compose.desktop.currentOs`**: that's the build machine's
platform, and in a cross build it quietly adds native libraries for the wrong architecture. The package
builds and installs, then fails with `UnsatisfiedLinkError` on the device.

The kernel accepts only three tokens: `linux-arm64` / `linux-armv7` / `linux-x86_64`. Android NDK names
(`arm64-v8a`) and bare CPU names (`aarch64`) are rejected.

### 9.3 Signing keys

`signing/platform-systemapp.pem`, copied in by `nervus-deploy/build-release.sh` from its key directory.
For a manual local build, copy one in yourself.

**The exceptions are `permissionui` and `store`**, which sign `platform-release` (change both
`signing.role` and `signing.keyFile`; the build script copies both keys into `signing/`). They need
`perm.permission.admin`, whose definition pins the `platform-release` signing role, which systemapp
can't satisfy (see nervud `internal/catalog/bootstrap.go`).

This isn't about which app matters more. It keeps "the power to give any app the camera and motion
control" apart from large packages that change often. **New modules still default to systemapp**;
using release needs a clear reason why nothing else will do.

The key_id is the sha256 of the **raw 32-byte public key**:

```
key_id = "sha256:" + hex(sha256(raw 32-byte ed25519 pubkey))
```

Not the sha256 of the X.509 SPKI (44 bytes). Getting it wrong means the package installs and runs, just
with `trust=ordinary`, because `scanSystemImage` fails closed to Ordinary on a bad signature rather than
skipping the package. This has happened on real hardware.

### 9.4 Into the release

`nervus-deploy/build-release.sh` globs `*/build/outputs/image-tree/*/` and takes all of it, so
**new modules need no script changes**.

### 9.5 Onto a device

```sh
cd nervus-deploy && ./build-release.sh
scp out/nervus-os-linux-arm64-*.tar.gz <board>:/tmp/
# on the board
tar -xzf /tmp/nervus-os-*.tar.gz -C /tmp
sudo /tmp/nervus-release/install-nervus.sh /tmp/nervus-os-*.tar.gz
```

Details in `nervus-deploy/README.md`. On an Ubuntu laptop, use NervusOS-Cambridge's
`scripts/build.sh` and `hosted/install-hosted.sh` instead.

---

## 10. Debugging

### 10.1 Where the logs are

**Component logs aren't in nervud's journal**; each has its own:

```sh
journalctl -u 'nervus-<package_id>-<component_id>.service' -n 50 -f
```

The kernel's decisions are on the nervud side:

```sh
journalctl -u nervud -b | grep -i 'ResolveEndpoint\|LaunchComponent'
```

The audit entries include `denied` and the exact permission ID, which **tells you far more than the
app's one warning**.

### 10.2 Package state

```sh
sudo nervusctl list          # trust / source / disabled
systemctl list-units 'nervus-*'
```

### 10.3 By symptom

| Symptom | Most likely cause |
|---|---|
| Component exits with `UNAUTHENTICATED` as soon as it starts | `ComponentConfig.componentId` doesn't match the manifest |
| `failed to resolve optional interface` | Misspelt interface_id, or the manifest doesn't declare the permission |
| Blank UI with no error | Same as above; the failed resolve was swallowed by `isRequired=false` |
| `UnsatisfiedLinkError` / skiko | `-Pnervus.abi` doesn't match the target |
| `NoClassDefFoundError: java/awt/*` | The JRE is a `-headless` build without `java.desktop` |
| `Can't connect to X11 window server` | The component's `type` is `service`, X isn't running, or the X cookie isn't readable |
| `read-only file system` on write | The target isn't in `readWritePaths` (§7.1) |
| `trust=ordinary` where it should be platform | Wrong signing role, or wrong key_id algorithm (§9.3) |
| Component `203/EXEC` | `entry`/`mainClass` doesn't match reality |
| Component `226/NAMESPACE` | The private data directory root doesn't exist |
| Changed the DSL but the manifest didn't change | Can't happen any more: `nspkgImageTree` is never UP-TO-DATE |

### 10.4 Two WSL traps

```sh
# gradlew has CRLF line endings and won't run under WSL ("cannot execute: required file not found")
sed -i 's/\r$//' gradlew

# JDK 21 from sdkman isn't on the default PATH
export JAVA_HOME=~/.sdkman/candidates/java/current
export PATH=$JAVA_HOME/bin:$PATH
```

`.gitattributes` already has `gradlew text eol=lf`, so fresh clones don't hit the first one.

---

## 11. Manifest DSL reference

The `nspkg { }` block from the `com.nervus.packaging` plugin.

### Package level

| Field | Type | Required | Notes |
|---|---|---|---|
| `packageId` | String | ✓ | §3.1 |
| `label` | String | ✓ | Display name |
| `labels` | Map<String,String> | | Display names per language |
| `icon` | String | | Path inside the package; **must be covered by digests** |
| `version` | String | ✓ | Only `[A-Za-z0-9._+-]`, at most 64 (used as a directory name) |
| `versionCode` | Long | ✓ | A monotonic integer used to compare upgrades |
| `minNervusApi` | Int | ✓ | Currently 1 or 2 |
| `targetNervusApi` | Int | ✓ | Currently 1 or 2 |
| `supportedAbis` | List<String> | ✓ | §9.2 |
| `permissions` | List<String> | | §6 |
| `runtimeDeps.minJavaRelease` | Int? | | 21 for JVM components |
| `usesFeatures.register(id) { required }` | | | Hardware feature declarations |
| `signing.keyFile` | File | ✓ | §9.3 |
| `signing.role` | String | | Image trees default to `platform-systemapp` |

### Component level

`components.app(id) { }` / `components.service(id) { }`

| Field | Default | Notes |
|---|---|---|
| `mainClass` | — | JVM entry class. Kotlin's `Foo.kt` → `FooKt` |
| `entry` | for JVM always `lib/<component id>.jar` | **Only needed for `runtime="native"`**; for JVM the plugin overwrites it |
| `runtime` | `"jvm"` | `jvm` / `native` |
| `nativeLibDir` | null | Path inside the package |
| `launchMode` | app → `manual`<br>service → `on-demand` | §2.1 |
| `criticality` | null | `optional` / `required` / `vital` |
| `disableable` | null | Whether the user may disable it |
| `interfaces` | null | Interfaces this component **calls** (not enforced by the kernel yet, but keep it truthful) |
| `idleTimeoutSec` | null | How long an idle on-demand component lives before being reaped |
| `limits.memoryMaxMb` | null | |
| `limits.cpuQuotaPercent` | null | |
| `limits.tasksMax` | null | |
| `exports.register(id) { visibility }` | | Interfaces this component **provides**: `package` / `public` |

Only interfaces declared under `exports` can be registered with `RegisterEndpoint`; **at run time, any
interface the manifest doesn't declare is refused**. Visibility has two levels:

| visibility | Who can resolve it | Permission needed |
|---|---|---|
| `package` | Only other components of the same package | `perm.service.register.private` (Ordinary) |
| `public` | Any package (subject to the interface's own permission) | `perm.service.register` (**MinTrust=OEM**) |

Trust is ordered `Ordinary < OEM < Platform`, and the test is `trust < entry.MinTrust`
(`permission/intersect.go`), so **Platform-trust system apps can get `perm.service.register`** and offer
public interfaces. Ordinary apps can't; they can only offer `package` visibility.

A package that exports interfaces must also ship provider artifacts (`provider { fromClasspath(...) }`),
or the kernel quarantines it with `ErrProviderArtifactsRequired`.

> A new public interface also has to be registered in the kernel's catalog.
> **A missing entry fails open, not closed**: when `Lookup` misses, the threshold is the empty string,
> meaning none, and any Ordinary app can resolve your interface. `pkg.manager` and `manipulator.arm`
> were both exposed this way for a while.

---

## 12. Known v1 limitations

Worth knowing before you write code:

| Limitation | What it means for you |
|---|---|
| Subscribe not implemented | Every UI must poll (§4.5) |
| `V1GrantAll = true` | Asking is getting, but the manifest must still declare everything (§6.2) |
| `PackageInfo` doesn't project label/icon/component list | The desktop and Settings guess display names and launch components by convention (`AppIdentity`). A package with two app components gets the wrong one |
| Each package carries its own Compose runtime | A package with a UI is about 41 MB. A shared runtime needs a `platform_runtime` field in `runtime_deps` and the kernel assembling the classpath; that's v2 |
| An empty selector binds the motion base by default | `DefaultRegistry` hard-codes `base.main` at compile time, so resolves succeed; once it becomes a real resource registry, devices without a base will fail to resolve |
| `nervus.interface.power` has no proto | Method IDs are hand-written constants on both sides; change one and you must change the other (§5.4) |

---

## 13. Examples to copy

Copying is faster than reading:

| What you want to do | Look at |
|---|---|
| The simplest app with a UI | `filemanager/`: one component, one permission |
| Calling system interfaces with multi-page navigation | `settings-app/`: NavigationRail, the pkgmanager methods, power |
| An app plus an always-on service | `launcher/`: `desktop` / `navigation` + `launcherd`, and how to wake several UIs |
| A long-running operation with a consent screen | `store/`: INSPECT, consent, `callOperation(INSTALL)` |
| File operations inside the sandbox | `filemanager/UserStorage.kt`: path normalisation and escape checks |
| Polling and state | `ui-common/Polling.kt` |

---

## 14. Checklist for a new system app

Go through it in order. The items marked ✗ **raise no build error**; on the device they show up as
"blank UI" or "pressing it does nothing", the most time-consuming kind of problem.

**Build wiring**

- [ ] `include(":your-module")` in `settings.gradle.kts`
- [ ] `build.gradle.kts` reads `rootProject.extra["nervusAbi"]`, not `currentOs`
- [ ] The only dependency is `implementation(project(":ui-common"))` (plus protobuf-java if you call interfaces)
- [ ] `signing.keyFile` points to `signing/platform-systemapp.pem`
      (exceptions: `permissionui` and `store`, see §9.3)

**Manifest**

- [ ] `packageId` is valid (dot-separated, `[A-Za-z0-9_-]`)
- [ ] Component `type` and `launchMode` fit together (app ≠ always-on, service ≠ manual)
- [ ] ✗ **The permission for every interface you call is listed under `permissions`** (§6.2)
- [ ] `interfaces` truthfully lists the interfaces you call
- [ ] If you provide an interface, `exports.register` is there, provider artifacts are packaged, and the
      kernel catalog has an entry

**Code**

- [ ] ✗ **`ComponentConfig(componentId = ...)` matches the manifest's component ID** (§3.3)
- [ ] ✗ **interface_id is copied from the kernel catalog, not from the proto directory name** (§5.2)
- [ ] `call()` is wrapped in public methods so the UI can reach it (§4.3)
- [ ] No `subscribe()` calls (§4.5)
- [ ] `rememberPolled` gets a `key` (§8.1)
- [ ] Writes only go to the private data directory or `user-data` (§7.1)
- [ ] Components with a UI are `app`, not `service` (§7.3)

**Desktop appearance**

- [ ] If the component isn't `main`, `AppIdentity.builtinComponents` has a mapping
- [ ] `AppIdentity.builtinNames` has a display name (optional), and the window title matches it

**Verification**

- [ ] `./gradlew :your-module:nspkgImageTree -Pnervus.abi=linux-arm64` passes
- [ ] `permissions` and `interfaces` in `manifest.json` are what you intended
      (`grep -o '"permissions":\[[^]]*\]' .../manifest.json`)
- [ ] After install, `sudo nervusctl list` shows `trust=platform`
- [ ] `journalctl -u nervud -b | grep ResolveEndpoint` shows no denied entries
