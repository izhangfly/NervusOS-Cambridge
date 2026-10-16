# NervusOS-Cambridge

NervusOS running on an ordinary Ubuntu 24.04 laptop, driving an AgileX PiPER robot arm over USB-CAN.

NervusOS is a robot operating system: a Go kernel (`nervud`) that owns the hardware, checks permissions and
hands out control leases, plus system services, a desktop, an App Store and an SDK for writing apps. Upstream
it targets RK3588 boards that boot straight into it. This repository packages it in **hosted mode**: NervusOS
opens fullscreen from the Ubuntu app grid like any other program, and quitting it puts you back on the Ubuntu
desktop with nothing changed. GDM, the login screen and the default boot target are never touched.

What works today, on a Dell Latitude (i7-8650U, Intel graphics, no GPU):

- The NervusOS desktop, Settings, Files and the App Store, fullscreen at 1920×1080.
- Installing signed `.nspkg` apps from the App Store, including the permission consent dialog.
- The **Robot Arm** app: live joint telemetry, per-joint sliders, Cartesian jog, gripper, home, recover and e-stop.
- Open and quit from the dock, the app grid, `Ctrl+Alt+N` / `Ctrl+Alt+Q`, or the NervusOS power menu.

## How it fits together

```text
Ubuntu 24.04 desktop (GNOME, untouched)
└── Xephyr window on display :7  ← nervus-session start / stop
    └── Openbox (single-window layout, 80 px nav bar)
        └── NervusOS desktop, Store, apps      (JVM + Compose, sandboxed systemd units)
                 │  protobuf IPC over /run/nervus/nervud.sock
                 ▼
            nervud (kernel)  → permissions, control leases, operations, audit, safety latch
                 │
                 ├── system services: pkgmanagerd, camerad, safetyrecoveryd
                 └── hardware layer: nervus-piper.service (piper_webctl.py, 127.0.0.1:8770)
                                          │  piper_sdk / SocketCAN
                                          ▼
                                     can0 (candleLight USB-CAN, 1 Mbit/s) → PiPER arm
```

## Repository layout

| Path | What it is |
|---|---|
| `src/` | The NervusOS sources, vendored at the upstream commits in `src/UPSTREAM.lock`, with the changes listed below |
| `src/nervud/` | The kernel (Go) |
| `src/nervus-ipc/` | Protocol schemas, generated Go/Java types, the Go SDK and registry |
| `src/nervus-system-server/` | Go system services: package manager, camera, safety recovery |
| `src/nervus-system-software/` | Kotlin system apps: desktop, settings, files, permission UI, App Store |
| `src/nervus-app-sdk/`, `src/nervus-packaging/` | The app SDK and the Gradle plugin that builds and signs `.nspkg` packages |
| `src/robot-arm/` | The Robot Arm app |
| `src/nervus-app-example/` | The SDK sample app (Hello World) |
| `src/nervus-deploy/` | Upstream release builder and installer |
| `src/nring/` | The NRing BLE ring SDK (not used by the demo yet) |
| `hosted/` | Everything that makes hosted mode work: `nervus-session`, the installer, systemd drop-ins, udev, polkit, Openbox config, desktop entries |
| `hardware/piper/` | The PiPER driver and its HTTP API (see [hardware/piper/README.md](hardware/piper/README.md)) |
| `store/catalog.ini` | What the App Store offers |
| `scripts/build.sh` | Builds the release tarball, the store apps and the catalog |
| `docs/SETUP.md` | Setting up a fresh Ubuntu machine, step by step |

## Quick start

On the Ubuntu machine, after installing Go and JDK 21 as described in [docs/SETUP.md](docs/SETUP.md):

```sh
scripts/build.sh                 # about 10 minutes the first time
sudo hosted/install-hosted.sh    # safe to re-run; updates in place
```

Then click **NervusOS** in the app grid or the dock. To get back to Ubuntu, use **Quit NervusOS**, `Ctrl+Alt+Q`,
or Power → Exit inside NervusOS. Over SSH:

```sh
nervus-session start             # needs a logged-in desktop session on the machine
nervus-session status
nervus-session stop
```

`sudo hosted/uninstall-hosted.sh` removes hosted mode again.

## The PiPER arm

Plug the arm's USB-CAN adapter in and power the arm on. The udev rule brings `can0` up at 1 Mbit/s whenever the
adapter appears, and `nervus-piper.service` starts and stops together with NervusOS. Only the driver touches the CAN
bus; apps go through its loopback API, so installing or removing apps never fights over `can0`.

The motors start disabled, and the driver stops the arm if the app's heartbeat stops for 1.5 s. The API has no
authentication and listens on `127.0.0.1` only. Keep it that way.

## Signing keys

`scripts/build.sh` creates Ed25519 keys in `keys/` on the first build and copies the developer key into each app's
`signing/` folder. Neither location is committed (see `.gitignore`). Self-generated keys are fine for a development
device and wrong for a product.

## Changes from upstream

Upstream pins are in [src/UPSTREAM.lock](src/UPSTREAM.lock). On top of them:

**Hosted mode**
- `nervud/internal/power/hosted.go` (new): when `NERVUS_HOSTED_POWER_HOOK` is set, power off and reboot end or
  restart the NervusOS session through the hook instead of shutting down the laptop. `main.go` picks this gate.
- `nervus-system-software/ui-common/PowerControl.kt` and `settings-app/PowerPage.kt`: the power menu says
  "Exit NervusOS" in hosted mode.
- `nervus-system-server/camerad/main.go`: the default board profile path points at the system package root.

**Kernel fixes**
- `nervud/internal/ipc/operation_wire.go`, `conn.go`: a long-task method whose Provider answers synchronously
  (OK or a failure instead of ACCEPTED) now closes its operation. Before this, an App Store install succeeded but
  the caller still waited for the operation and failed with DeadlineExceeded. Covered by two new e2e tests.
- `nervud/internal/audit/file_test.go`: the denied/throttle tests no longer race the background fsync.

**English only**
- Every comment, log line, error message, UI string and document is in English. The built-in permission catalog
  (`nervud/internal/catalog/bootstrap.go`) carries English text only, and `nervus-ipc/registry/bundle.go` now requires
  `en` UI text and treats `zh_cn` as optional.
- Protos were regenerated after the comment translation (`buf generate`, protoc-gen-go v1.36.11, protoc 29.3); the
  golden vectors are byte-identical.

**Apps and store**
- `nervus-system-software/store/` (new): the App Store app, reading `/usr/share/nervus/store/catalog.ini`.
- `robot-arm`: builds for the host ABI (`-Pnervus.abi`), signs with the developer key, and drops the unused
  `ArmServer` service; it talks to the PiPER driver directly.
- `nervus-app-example`: the Hello app gained a `main()` entry point; the unused sample service was removed.
- Gradle wrappers download from `services.gradle.org` instead of a mirror in mainland China.

**Build wiring**
- `nervud/go.mod` and `nervus-system-server/go.mod` use the local `src/nervus-ipc` through a `replace`, so kernel
  and protocol changes always build together.
