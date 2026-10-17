# NervusOS-Cambridge

NervusOS running on an ordinary Ubuntu 24.04 laptop, driving an AgileX PiPER robot arm over USB-CAN, with apps that
let you drive the arm with your hand and teach it tasks by demonstration.

NervusOS is a robot operating system: a Go kernel (`nervud`) that owns the hardware, checks permissions and hands
out control leases, plus system services, a desktop, an App Store and an SDK for writing apps. Upstream it targets
RK3588 boards that boot straight into it. This repository runs it in **hosted mode**: NervusOS opens fullscreen
from the Ubuntu app grid like any other program, and quitting it puts you back on the Ubuntu desktop with nothing
changed. GDM, the login screen and the default boot target are never touched.

It is also the one repository for the whole project: the Nervus-OS team's repositories, the Piper-ACT project,
the robot lab scripts and the planning documents all live here as folders (see the layout below), in English.

What works today, on a Dell Latitude (i7-8650U, Intel graphics, no GPU):

- The NervusOS desktop, Settings, Files and the App Store, fullscreen at 1920×1080; Exit NervusOS returns to Ubuntu.
- Installing signed `.nspkg` apps from the App Store, with the permission consent dialog.
- **Robot Arm**: live joint telemetry, per-joint sliders, Cartesian jog, gripper, home, recover and e-stop.
- **Hand Teleop**: the laptop camera tracks your arm and fingers (YOLO26 pose + hand landmarks on the CPU) and the
  arm follows your hand; a pinch closes the gripper. Frames come from the camera service and commands go through
  the kernel under a control lease, to the arm's Provider (`piperd`).
- **ACT Studio**: record demonstrations by moving the arm by hand, train an ACT policy on a Mac, run it, correct it,
  and post-train on the corrections (see [act/README.md](act/README.md)).

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
                 ├── system services: pkgmanagerd, camerad, safetyrecoveryd,
                 │                    piperd (Manipulator@1 Provider for arm.main)
                 └── hardware layer: nervus-piper.service (PiPER driver, 127.0.0.1:8770)
                                     nervus-act.service   (ACT engine,   127.0.0.1:8795)
                                          │  piper_sdk / SocketCAN
                                          ▼
                                     can0 (candleLight USB-CAN, 1 Mbit/s) → PiPER arm

ACT engine ⇄ ~/Shared/NervusACT ⇄ (Syncthing: LAN, or the relay on the server) ⇄ Mac worker (trains on MPS)
```

## Repository layout

| Path | What it is |
|---|---|
| `src/` | The NervusOS sources this machine builds, forked from the team repositories at the commits in `src/UPSTREAM.lock`, with the changes listed below |
| `src/nervud/` | The kernel (Go) |
| `src/nervus-ipc/` | Protocol schemas, generated Go/Java types, the Go SDK and registry |
| `src/nervus-system-server/` | Go system services: package manager, camera, safety recovery, `piperd` |
| `src/nervus-system-software/` | Kotlin system apps: desktop, settings, files, permission UI, App Store |
| `src/nervus-app-sdk/`, `src/nervus-packaging/` | The app SDK and the Gradle plugin that builds and signs `.nspkg` packages |
| `src/robot-arm/`, `src/hand-teleop/`, `src/act-studio/` | The three robot apps |
| `src/nervus-app-example/`, `src/nervus-deploy/`, `src/nring/` | The SDK sample, the upstream release builder, the NRing BLE ring SDK |
| `hosted/` | What makes hosted mode work: `nervus-session`, the installer, systemd units and drop-ins, udev, polkit, Openbox, desktop entries |
| `hardware/piper/` | The PiPER driver and its HTTP API ([README](hardware/piper/README.md)) |
| `act/` | The ACT system: the engine on the Dell, the worker on the Mac, tests ([README](act/README.md)) |
| `store/catalog.ini` | What the App Store offers |
| `team/` | The Nervus-OS team repositories as they are upstream now, where they differ from `src/`: the restarted nervud and nervus-ipc, nervud_old, Nervus-Console, the Gazebo motion provider, and the team's planning repository ([README](team/README.md)) |
| `legacy/piper-act/` | The Piper-ACT project (Windows Dell + Mac) that ACT here grew out of, complete |
| `robots/` | The robot lab before the Dell: GO2 dogs, RDK S100, NERO arm, PiPER calibration (historical) |
| `apps/` | App projects from the RDK era: the Limo "Alien Wandering" app and the Zilo ring apps |
| `docs/` | Setting up a fresh machine ([SETUP](docs/SETUP.md)), onboarding guides (`docs/onboarding/`), posters |
| `server/` | What runs on the relay server: the private Syncthing relay and discovery server, the training dashboard |
| `scripts/` | `build.sh` (release tarball, apps, catalog), `push.sh` and `secret-scan.py` (how this repository is pushed), `translate.py` (how the Chinese was translated) |

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
sudo nervusctl install out/store/packages/com.nervus.act-1.0.1.nspkg perm.camera.capture,perm.manipulator.control
```

`sudo hosted/uninstall-hosted.sh` removes hosted mode again.

## The PiPER arm

Plug the arm's USB-CAN adapter in and power the arm on. The udev rule brings `can0` up at 1 Mbit/s whenever the
adapter appears, and `nervus-piper.service` starts and stops together with NervusOS. Only the driver touches the CAN
bus. Apps reach it two ways: through the kernel (`piperd` serves `Manipulator@1` on resource `arm.main`, so leases,
permissions and the deadman apply) or, for the Robot Arm app and the ACT engine, through its loopback API. The driver
lets one client control the arm at a time and refuses the others with "busy".

The motors start disabled. A client that stops sending (heartbeat, servo goals or streamed joints) stops the arm
within half a second to 1.5 s, and the arm holds position rather than going limp. Both loopback APIs have no
authentication and listen on `127.0.0.1` only. Keep it that way.

## Cameras

The Dell's built-in camera is `cam.front` for NervusOS apps (Hand Teleop). The two robot cameras belong to the ACT
engine: the Orbbec Dabai DC1 on the wrist and a Logitech C270 for the scene. Plug the C270 into a **USB-A** port: on
the USB-C/Thunderbolt side the JHL6540's USB keeps resetting it and it delivers no frames (ACT Studio says so).

## Working on this repository

Read [CLAUDE.md](CLAUDE.md). In short: this repository is the only place NervusOS work is pushed, every change is
pushed with `scripts/push.sh` (which refuses to commit anything `scripts/secret-scan.py` flags: keys, tokens,
passwords, Chinese text, the relay server's address), and the server's address is never written into a file.

`scripts/build.sh` creates Ed25519 keys in `keys/` on the first build and copies the developer key into each app's
`signing/` folder. Neither location is committed. Self-generated keys are fine for a development device and wrong
for a product.

## Changes from upstream

Upstream pins are in [src/UPSTREAM.lock](src/UPSTREAM.lock). On top of them:

**Hosted mode**
- `nervud/internal/power/hosted.go` (new): with `NERVUS_HOSTED_POWER_HOOK` set, power off and reboot end or restart
  the NervusOS session through the hook instead of shutting down the laptop; the power menu says "Exit NervusOS".
- `hosted/libexec/hosted-power`: closes the Xephyr display as the session's owner (`setpriv`), because nervud's
  sandbox has no `CAP_KILL`; `nervus-session` also closes it if nervud stops. Before this, Exit NervusOS left a black
  screen with a cursor.
- Cameras get fixed defaults on plug-in, and device Providers (camerad) get the `video` group
  (`NERVUS_DEVICE_GROUPS`, applied as `SupplementaryGroups` to components with device access).

**Kernel fixes**
- A long-task method whose Provider answers synchronously now closes its operation, including when it fails.
  Before, an App Store install succeeded but the caller waited for the operation until DeadlineExceeded.
- Every lease-gated command refreshes the caller's control lease, so a HUMAN lease's 300 ms deadman really is a
  deadman for a client that keeps commanding.
- `pkgmanagerd` reports failures as status codes (Providers may not send error details), and the Store explains
  each code.

**The arm in NervusOS**
- `Manipulator@1` gained `SERVO_TO_POSE`, a lease-gated, unconfirmed streaming target for teleoperation.
- `piperd` (new system service): the `Manipulator@1` Provider for `arm.main`, on top of the PiPER driver.
- App SDK: control leases (`acquireControl`), typed call errors, and a shared-memory ring consumer for camera frames.

**English only**
- Every comment, log line, error message, UI string and document is in English, in `src/` and in the imported
  repositories alike. The protos were regenerated after translation; the golden vectors are byte-identical.

**Apps and store**
- The App Store app, Robot Arm, Hand Teleop and ACT Studio; the Gradle wrappers download from
  `services.gradle.org` instead of a mirror in mainland China.
- `nervud/go.mod` and `nervus-system-server/go.mod` use the local `src/nervus-ipc` through a `replace`, so kernel and
  protocol changes always build together.
