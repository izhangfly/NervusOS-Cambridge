# nervus-system-software

The built-in system apps of Nervus OS, written in Kotlin with Compose Desktop and Material 3.

The build produces **system image packages** that ship with the image. They are installed in
`/usr/lib/nervus/system-packages/`, next to the system services from `nervus-system-server`.

| Package | Components | What it does |
|---|---|---|
| `nervus.launcher` | `desktop` (app)<br>`navigation` (app)<br>`launcherd` (service, always-on) | The desktop, the navigation bar on the right, and the resident service that starts them |
| `nervus.settings` | `main` (app) | Installed apps, power, about, developer options |
| `nervus.filemanager` | `main` (app) | File manager for the shared user folder |
| `nervus.permissionui` | `main` (app, on-demand) | Runtime permission manager and the permission request dialog |
| `nervus.store` | `main` (app) | The App Store: browse the catalog, install, open and uninstall packages |

**To write a new system app, read [DEVELOPING.md](DEVELOPING.md).**

## One window at a time, and system navigation

The image's Openbox maximises ordinary app windows to the screen minus 80 px on the right, and pins
the borderless window titled `Nervus Navigation` to that right-hand strip. `launcherd` keeps the
desktop and the navigation bar running.

- Back sends `Escape` to the foreground window. `attachComposeDesktop` passes it to the most recently
  registered, enabled `NervusBackHandler`; if the app has nothing to go back to, the window-level
  fallback runs.
- Home makes sure `nervus.launcher/desktop` is running, then activates the desktop window titled `Nervus`.
- An app's window title should stay equal to `AppIdentity.displayName(packageId)`, so that tapping the
  desktop icon again brings the running window forward instead of seeming to do nothing.
- Back and Home only switch windows or app state; they never end processes. nervud still owns
  component lifecycles.

On a dedicated device, `nervus-deploy/install-nervus.sh` sets up X11 and the window manager, and
`nervus-deploy/fix-display.sh` repairs them in the field. On Ubuntu in hosted mode, the same Openbox
layout runs inside a Xephyr window instead (see `hosted/` at the top of NervusOS-Cambridge).

## The App Store (`nervus.store`)

The store reads a catalog shipped in the read-only image area:

```
/usr/share/nervus/store/catalog.ini
/usr/share/nervus/store/packages/<file>.nspkg
```

Installing is three steps. The store copies the `.nspkg` into `/var/lib/nervus/user-data/.store/`
(the only place pkgmanagerd accepts packages from), calls INSPECT so the kernel verifies the package
and lists the sensitive permissions it asks for, shows those on a consent screen, then calls INSTALL
with the accepted permissions and the manifest digest from INSPECT.

INSTALL is a needs_user_confirmation method, and the kernel only accepts it from a caller holding
`perm.permission.admin`. That permission requires the `platform-release` signing role, so the store
is signed like `permissionui`, not like the other system apps.

---

## Building

Needs JDK 21. The target ABI defaults to `linux-arm64` (real hardware).

```sh
# After a first clone on WSL
sed -i 's/\r$//' gradlew

# Signing keys (copied from the deploy key directory)
mkdir -p signing
cp <keys>/platform-systemapp.pem <keys>/platform-release.pem signing/

./gradlew nspkgImageTree                            # arm64 device
./gradlew nspkgImageTree -Pnervus.abi=linux-x86_64  # x86_64 PC (Ubuntu, WSL)
```

Output: `<module>/build/outputs/image-tree/<package_id>/`, containing `manifest.json`,
`manifest.sig` and `lib/*.jar`.

The full release is built by `nervus-deploy/build-release.sh` (in NervusOS-Cambridge, run
`scripts/build.sh`), which collects all of the above.

## Layout

```text
nervus-system-software/
├── settings.gradle.kts   includeBuild(../nervus-app-sdk, ../nervus-packaging)
├── build.gradle.kts      ABI parameter + skiko de-duplication
├── ui-common/            shared: polling, app identity convention, power interface, icons
├── launcher/
├── settings-app/
├── filemanager/
├── permissionui/
├── store/
└── signing/              platform-systemapp.pem, platform-release.pem (never committed)
```

The SDK and the packaging plugin come in as a **composite build** rather than Maven dependencies:
the three repositories evolve together, and publishing a version for every one-line SDK change
would get in the way.

## Related repositories

| Repository | What it is |
|---|---|
| `nervud` | The kernel: permission catalog, interface catalog, sandbox, package install |
| `nervus-ipc` | The control-plane protocol (protobuf), generated for four languages |
| `nervus-app-sdk` | Kotlin SDK: `NervusApp` / `NervusService` / Compose wiring |
| `nervus-packaging` | Gradle plugin and signing library |
| `nervus-system-server` | System services (Go): pkgmanagerd, safety recovery, camerad |
| `nervus-deploy` | Release build and install scripts |
