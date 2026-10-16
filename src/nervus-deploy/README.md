# Nervus OS device deployment guide

How to install Nervus OS on a device. The target is an RK3588-class Linux + systemd board; the
same release also builds for x86_64 (`linux-x86_64`), which is how NervusOS-Cambridge runs it on
an Ubuntu laptop.

> **On an Ubuntu desktop, don't run `install-nervus.sh` by itself with a terminal attached.** Its
> display step offers to disable the existing display manager and boot into multi-user.target so the
> Nervus desktop can own the screen. NervusOS-Cambridge's `hosted/install-hosted.sh` runs it
> non-interactively (which keeps the Ubuntu desktop) and then sets up hosted mode instead.

## What's in this directory

**Nothing here is spare.** Apart from `out/`, every item is a link in the release chain, and
deleting or moving any of them breaks the build or the install.

### Source files (don't delete)

| File | When it's used | What breaks without it |
|---|---|---|
| `build-release.sh` | Builds the release on the build machine | — |
| `install-nervus.sh` | Installs on the target; **also packed into the release** | The release has no installer |
| `nervud.service` | The kernel's systemd unit, installed to `/etc/systemd/system/` on the target. **The unit name must not change** (components' transient units use `BindsTo=nervud.service`) | The build fails on the spot; even if installed, the kernel can't start |
| `journald-nervus.conf` | journald persistence and size, installed to `/etc/systemd/journald.conf.d/10-nervus.conf` | Logs stay in tmpfs and **everything before a reboot is lost** (see §2.5) |
| `provision-rootfs.sh` | Lays out the directory tree. `install-nervus.sh` runs it; it can also be used on its own for offline image builds | The install fails halfway |
| `fix-display.sh` | Display session repair, shipped to the target's `scripts/` | With a black screen the target only has ssh, and no repair tool at hand |
| `check-display-sync.sh` | Before release, checks that the display config embedded in the two scripts above matches | A mismatched version ships (symptom: black screen after install, fixed by running fix-display once by hand) |
| `README.md` | This file | — |

### Directories

| Directory | What it is | Can it be deleted |
|---|---|---|
| `out/` | **Build output** (`nervus-os-*.tar.gz`) | ✅ wipe freely |
| `.jre-cache/` | Temurin JRE downloaded for cross builds (about 50 MB) | ✅ downloads again |
| `keys/` | Ed25519 signing private keys | ❌ **deleting them voids the machine's trust levels**, and installed devices can no longer receive upgrades they can verify |

> Output used to land in the root of this directory, mixed with the source files, so it looked like
> a junk drawer. Someone actually tidied everything into `old/` and the build broke on the spot.
> Output now always goes to `out/`, and before assembling, `build-release.sh` checks that the
> source files above are all present and explains what each missing one is for.

---

## 1. Target prerequisites

| Item | Requirement | Why |
|---|---|---|
| Architecture | arm64 (aarch64), or x86_64 with `TARGET_ABI=linux-x86_64` | The ABI token is written into every manifest |
| init | **systemd** | nervud has PID 1 start every component over D-Bus; no systemd means no sandbox and no cgroup limits |
| D-Bus | System bus available | Same as above. `dbus.service` is a hard dependency |
| Kernel | Linux ≥ 3.15 | Package install uses `renameat2` with `RENAME_NOREPLACE` / `RENAME_EXCHANGE` |
| Capability | `CAP_SYS_NICE` | Sets `SCHED_FIFO`/`SCHED_RR` for real-time components. **Without it nervud exits in production mode** |
| Filesystems | `/var/lib` and `/usr` may be on different partitions | But everything under `/var/lib/nervus` must be on **one filesystem** (see the warning in §2) |

> ⚠️ **No Go toolchain, no source and no network are needed on the target.**
> The kernel and system services are pre-built static binaries (`CGO_ENABLED=0`).
> Digests and signatures must be computed at build time; the target has nothing to recompute them with.

---

## 2. The full directory layout

This table is authoritative. nervud's `preflight` checks every row at start-up
(`internal/preflight/preflight.go`). **A mismatch in the read-only area is fatal and nervud refuses to start.**

### Read-only image area (follows OTA, not writable at run time)

| Path | Mode | Contents |
|---|---|---|
| `/usr/libexec/nervus/nervud` | `0755`, root, **not group/other writable** | **The kernel binary** |
| `/usr/lib/nervus/system-packages/` | `0755` | **System services and system apps**, one directory per package |
| `/usr/lib/nervus/jre/` | `0755` | The platform JRE; for `runtime: "jvm"` components, `ExecStart` must be **exactly** `/usr/lib/nervus/jre/bin/java` |
| `/usr/share/nervus/trust/` | `0755` | The trust root bundle |

### nervud's own writable area

| Path | Mode | Contents |
|---|---|---|
| `/var/lib/nervus/` | `0755` | Root |
| `/var/lib/nervus/registry/` | `0700` | Per-package record JSON + the UID allocator. **Only nervud can read it** |
| `/var/lib/nervus/packages/` | `0755` | Code of **dynamically installed** packages, `<pkg>/<version>/` |
| `/var/lib/nervus/package-data/` | `0755` | **Root** of each package's private data directory; the kernel creates `<pkg>/` as `0700` owned by the package UID |
| `/var/lib/nervus/staging/` | `0711` | Install staging. **0711, not 0700**: the install service must be able to pass through to its own `stage-*` without being able to list what's inside |
| `/var/lib/nervus/jvm-cache/` | `0755` | JVM cache |
| `/var/lib/nervus/user-data/` | `01777` | **The user documents area shared across packages.** Sticky and world-writable, the same shape as `/tmp`: anyone can put things in, but **only delete their own**. Only packages declaring `perm.storage.user` get it inside their sandbox |

### Logs and audit

| Path | Mode | Contents |
|---|---|---|
| `/var/log/journal/` | `2755` (setgid) | **journald's persistent directory.** Its existence is the on/off switch; see §2.5 |
| `/var/lib/nervus/audit/` | `0700` | **The security audit hash chain.** Written and rotated by nervud itself; separate from the journal |

### Runtime (tmpfs, recreated every boot)

| Path | Mode | Contents |
|---|---|---|
| `/run/nervus/` | `01755` (sticky) | Home of the two sockets. Created by `nervud.service`'s `RuntimeDirectory`; **don't create it on the rootfs** |
| `/run/nervus/nervud.sock` | — | **App control plane.** Only accepts app-range UIDs (20000–59999) |
| `/run/nervus/nervud-admin.sock` | `0660` | **Admin channel.** root + the permitted install service UID |

> ⚠️ **`/var/lib/nervus/staging` and `/var/lib/nervus/packages` must be on the same filesystem**
>
> Committing an install is `renameat2(staging → packages/<pkg>/<version>)`. A cross-filesystem rename
> fails with `EXDEV`, and the symptom is "install always fails", with nothing pointing at the partitioning.
>
> Keeping both under `/var/lib/nervus` makes this true by default. **Don't** mount staging separately
> on tmpfs or another partition.

> ⚠️ **The sticky bit on `/run/nervus` is a security setting, not a habit**
>
> `/run` is tmpfs and doesn't support `chattr +i`. The equivalent protection is a sticky parent:
> non-root processes can't create or unlink entries in it, so the sockets can't be deleted or swapped.
> Preflight checks the exact value `01755`.

---

## 2.5 Logs and audit

The machine keeps **two** records with different purposes and guarantees. Mixing them up sends you
to the wrong place when something goes wrong.

| | Ops log | Security audit |
|---|---|---|
| Answers | **What actually happened** (timeouts, stack traces, start failures) | **Who was allowed or refused what, and when** |
| Written by | nervud + every component's stdout/stderr | The nervud kernel (`internal/audit`) |
| Stored in | journald → `/var/log/journal/` | `/var/lib/nervus/audit/audit.jsonl` |
| Read with | `journalctl -u nervud` | `nervusctl audit-verify` |
| Tamper-evident | ❌ | ✅ SHA-256 hash chain |
| Can be dropped by level filters / rate limits | ✅ yes | ❌ no |
| **Lost on power failure, at most** | **5 minutes** | **1 second** |

The audit is **also** mirrored into the journal, so `journalctl` shows audit entries too. That copy has
no guarantees; tamper evidence and retention come from the file in `/var/lib/nervus/audit`.

### Audit: a 1-second window, and losses are visible

The audit doesn't go through journald. nervud handles its own writes with a **batched, denials-first** policy:

- A timer fsyncs every **1 second**, **only when there are unwritten records** (an idle machine never
  fsyncs, so the SD card isn't worn for nothing)
- Records with `denied=true` are fsynced **immediately**, at most once a second
- Before rotation and at shutdown, fsync **unconditionally**

Not fsyncing every record is deliberate, because it would backfire: one fsync on an SD card takes
milliseconds to tens of milliseconds, so per-record fsync would drop the audit writer to tens of records
a second, the queue would overflow in a burst and records would be lost. Trying not to lose any would lose some.

> ⚠️ **"The chain is intact" is not "nothing is missing"**
>
> If a power cut loses the last few records, **the remaining chain is fully consistent**: sequence
> numbers continuous, prev links matching, hashes correct. With only a hash chain, the verifier would
> pass a truncated file. That's not security, it's false certainty.
>
> So a clean shutdown writes a closing `audit.Closed`. If the next start finds the chain doesn't end with
> it, it adds `audit.UncleanShutdown`, writing "up to 1 second of records may be missing here" into the
> chain. `nervusctl audit-verify` lists every such place.

The three anomalies have their own names on the chain and can't be confused:

| Record | Means |
|---|---|
| `audit.ChainStarted` | The file is new, or was deleted and recreated; sequence numbers restart at 1 |
| `audit.UncleanShutdown` | Power loss or KILL: the chain isn't broken, but the tail may be missing records |
| `audit.ChainGap` | The queue was full, and **exactly how many** were lost is known |

Size: 16 MiB per file × 5 files = **80 MiB cap**, rotated by nervud itself, independent of journald.

### Logs: 500 MB, a 5-minute window

Configured in `journald-nervus.conf`, installed as `/etc/systemd/journald.conf.d/10-nervus.conf`:

| Setting | Value | Why |
|---|---|---|
| `Storage` | `persistent` | On disk. **Must be explicit**; see the trap below |
| `SystemMaxUse` | `500M` | Not the default "10% of the filesystem, up to 4 G", which would make a bigger card wear faster automatically |
| `SystemKeepFree` | `500M` | A second limit; the smaller wins, so even a small card doesn't fill up |
| `SystemMaxFileSize` | `64M` | 500 MB in about 8 files. Too big and cleanup drops a large slice at once; too small and `journalctl` slows down |
| `MaxRetentionSec` | `1month` | Older logs are no use for debugging; reclaim by age first |
| `SyncIntervalSec` | `5min` | The systemd default. Power loss costs at most 5 minutes of **ops logs** |
| `RateLimitBurst` | `20000`/30s | The default of 10000 **silently drops** the rest |

`nervud.service` additionally has `LogRateLimitIntervalSec=0`, exempting the kernel itself **completely**
from rate limiting: the kernel floods the log exactly when something is wrong. Only the TCB gets this;
rate limiting isn't switched off globally, or one noisy component could flush all 500 MB in minutes and
push everyone else's history out.

> ⚠️ **`SyncIntervalSec=5min` doesn't affect the audit**
>
> The audit goes to `/var/lib/nervus/audit` with its own 1-second window, independent of journald.
> Lowering the 5 minutes here only buys SD card write amplification, not audit durability.
>
> Also, journald syncs `CRIT`/`ALERT`/`EMERG` **immediately and unconditionally**, so the messages that
> really matter never wait 5 minutes.

> ⚠️ **On Raspberry Pi OS, creating the directory isn't enough**
>
> Raspberry Pi OS bookworm images after 2025-05-13 set `Storage=volatile` **explicitly** in
> `/etc/systemd/journald.conf`, and the `/var/log/journal` directory only matters for `Storage=auto`;
> the explicit value overrides auto.
>
> That's why the configuration is a `journald.conf.d/` drop-in: drop-ins take precedence over the main
> file. It also avoids editing the distribution's own file and clashing on upgrades.

**Check it really took effect** (`install-nervus.sh` does this once automatically):

```sh
journalctl --header | grep journal      # should show /var/log/journal
journalctl --disk-usage                 # current usage
```

The symptom of logs not persisting is "everything's fine until the day you need them": by then the
machine has rebooted, they're gone, and nothing points at the configuration. So check after every install.

> `uninstall-nervus.sh` removes the drop-in but **doesn't delete `/var/log/journal`**: that holds the
> whole machine's system log, not just Nervus's. To clear it, use `journalctl --vacuum-size`.

---

## 3. Where the three pieces go

### 3.1 The kernel, `nervud`

```sh
install -D -m 0755 -o 0 -g 0 nervud "$ROOT/usr/libexec/nervus/nervud"
install -D -m 0644 -o 0 -g 0 nervud.service "$ROOT/etc/systemd/system/nervud.service"
```

**`/usr/libexec`, not `/usr/bin`**: it isn't a command for people to type; systemd starts it.
In `/usr/bin` it would show up in users' `PATH` completion.

> ⚠️ **The unit must be named `nervud.service`**
>
> Every Package component's transient unit has `BindsTo=nervud.service` (owner-death: when nervud dies
> its components stop). Renaming it makes `StartTransientUnit` fail with
> `Unit nervud.service not found`, and **no component can start**.

> ⚠️ **Don't run the bare binary**
>
> Same reason: the unit `BindsTo` points at must really exist. On a dev machine use
> `systemd-run --unit=nervud --collect ...`.

### 3.2 The ops CLI, `nervusctl`

```sh
install -D -m 0755 -o 0 -g 0 nervusctl "$ROOT/usr/bin/nervusctl"
```

This one **is** for people, so it goes in `/usr/bin`. It connects to the admin channel (root-only) to
list packages, install and remove packages by hand, and start or stop components. Preflight doesn't
check it: the kernel works fine without it, you just can't manage it from the command line.

### 3.3 System services and system apps

**Both go in the same place**, as "system image packages":

```
/usr/lib/nervus/system-packages/<package_id>/
├── bin/<binary>        or lib/<app>.jar
├── manifest.json       digests filled in
└── manifest.sig        signature
```

**The directory name must equal `package_id`.** The kernel identifies packages by their manifest
content and the directory is just a container, but a mismatch sends people debugging to the wrong package.

**No version subdirectory.** System packages follow whole-image OTA, so several versions never coexist.
That's different from dynamically installed packages (`/var/lib/nervus/packages/<pkg>/<version>/`);
the kernel has two separate scan paths for them.

At start-up the kernel globs `/usr/lib/nervus/system-packages/*/manifest.json`.

#### System services vs system apps: the difference is the signing role, not the location

| | **System services** (`nervus-system-server`) | **System apps** (`nervus-system-software`) |
|---|---|---|
| Location | `/usr/lib/nervus/system-packages/<pkg>/` | **Same** |
| Signing role | `platform-release` | **`platform-systemapp`** (permissionui and store: `platform-release`) |
| Component `type` | `service` | `app` |
| `launch_mode` | `always-on` allowed | **`always-on` not allowed** (the kernel refuses with `ErrLaunchModeTypeMismatch`); use `on-demand` or `manual` |
| Typical content | Package install, safety recovery, permission providers | Settings, launcher, diagnostics UI |

The line between the two roles is **how far their permissions reach**. `platform-release` is the
"kernel and core system services" tier and can get permissions only it may hold, such as
`perm.safety.rearm` and `perm.authority.reboot`. `platform-systemapp` is "system apps shipped with the
image": Platform trust, but not the permissions with `RequireSignerRole: "platform-release"`.

#### The system apps

`nervus-system-software` produces these system image packages, placed in `system-packages/` next to the
system services:

| package_id | Components | Permissions | Notes |
|---|---|---|---|
| `nervus.launcher` | `desktop` (app/`manual`)<br>`navigation` (app/`manual`)<br>`launcherd` (service/`always-on`) | `perm.system.launch`<br>`perm.pkg.query`<br>`perm.authority.power` | The desktop, the navigation bar on the right, and the resident service that wakes them |
| `nervus.settings` | `main` (app/`manual`) | `perm.pkg.install` `perm.pkg.query`<br>`perm.authority.power` | Installed apps / power / about / developer options |
| `nervus.filemanager` | `main` (app/`manual`) | `perm.storage.user` | File manager for the shared user folder |
| `nervus.permissionui` | `main` (app/`on-demand`) | `perm.permission.admin` `perm.service.register` | Runtime permission manager and request dialog |
| `nervus.store` | `main` (app/`manual`) | `perm.permission.admin` `perm.pkg.install` `perm.pkg.query` `perm.storage.user` `perm.system.launch` | App Store: catalog, consent screen, install / open / uninstall |

Each package is about 43 MB (it carries the Compose Desktop runtime).

#### Power (restart / power off)

Both the desktop status bar and Settings' Power page can trigger it, through the **built-in endpoint**
`nervus.interface.power` (`nervud/internal/power/builtin.go`).

Built-in rather than a new Envelope body because of `envelope.proto`'s rule: "anything that isn't one of
the eight things (establish a connection / discover endpoints / make a call / return a result / cancel /
subscribe / push events / keep alive) should be a method on some Interface". Power off is none of those,
yet only the kernel can do it (the Authority Gate does the work), so it takes the same built-in shape as
`safety.control`.

In hosted mode (environment variable `NERVUS_HOSTED_POWER_HOOK` set on nervud), both methods are routed
to that hook instead: power off ends the NervusOS session, reboot restarts NervusOS.

**Don't mix up the two restart paths**:

| | `perm.authority.reboot` | `perm.authority.power` |
|---|---|---|
| Action | `reboot(2)` hard reset | systemd's full `shutdown.target` |
| Components get SIGTERM | No | Yes |
| Filesystems unmount cleanly | No (only `sync`) | Yes |
| Signing role | `platform-release` only | Any Platform trust |
| Use | Fault recovery (the system is no longer trustworthy; leave fast) | The button the user presses |

The method IDs (`REBOOT=1` / `POWER_OFF=2`) **have no proto behind them**. The only definition is in
`nervud/internal/power/builtin.go`, mirrored on the Kotlin side in
`nervus-system-software/ui-common/.../PowerControl.kt`. Change one and you must change the other, or the
symptom is "I pressed it and nothing happened" (the call is routed to a method that doesn't exist and
gets `NOT_FOUND`). Once v2 adds `power_control.proto`, both sides should take the values from the generated enum.

> **The package ID and component ID of `nervus.settings` must not change.** The kernel's
> `isProtectedComponent` in `pkgregistry/lifecycle.go` puts `nervus.settings/main` on the
> cannot-be-disabled list: the component that offers the disable UI can't be disabled. Renaming it
> voids that protection silently.

**The boot path** (the one structural difference between system apps and system services):

```
service.Manager.Start walks every package and starts only launch_mode == always-on
   └─ nervus.launcher/launcherd  ✅ starts
      nervus.launcher/desktop    ⏸ manual, left alone
      nervus.launcher/navigation ⏸ manual, left alone
                  ↓
the ipc module starts listening (module 12, after service at 8)
                  ↓
launcherd connects to the control plane → LaunchComponent(nervus.launcher, desktop/navigation)
                  ↓
the kernel starts the desktop and navigation bar → tap an icon → LaunchComponent starts Settings / Files
```

**Why the desktop can't start itself at boot**: the kernel refuses `always-on` for `app`
(`ErrLaunchModeTypeMismatch`). So an `always-on` *service* has to wake it. That's the whole reason
`launcherd` exists.

**`LaunchComponent` (Envelope body 80/81)** is the control-plane message added for this. Before it, the
only way the kernel started a component was `endpoint.Resolve` starting an on-demand provider, so
"start an app" had to pretend to be "resolve an interface", every app had to export a placeholder
interface it didn't need, and the audit couldn't tell the two apart.

It needs `perm.system.launch` (`MinTrust: Platform`): a launcher installed by a third party can't get it,
the same pattern as Android's privileged permissions.

#### The sandbox of GUI components

**For `type: "app"` components the kernel also wires up X11** (`service/supervise.go`):

| Item | Value | Why |
|---|---|---|
| `BindReadOnlyPaths` | `/tmp/.X11-unix` | `PrivateTmp=true` gives components a private `/tmp`, and X11 client libraries **hard-code** `/tmp/.X11-unix/X<n>`. Without the bind, every GUI component fails with "Can't connect to X11 window server", and the error doesn't look sandbox-related |
| `DISPLAY` | Passed through from nervud, default `:0` | External screens, WSLg, Xwayland and the hosted Xephyr display all use different values; hard-coding one would bake a deployment shape into the kernel |
| `XAUTHORITY` | Only if nervud's environment has it | The cookie file usually lives under a home directory that `ProtectHome=yes` hides, so the file itself is bind-mounted too |

The bind mount applies **after** `PrivateTmp`, so `/tmp` isolation between components still holds. That's
far more precise than switching `PrivateTmp` off.

Xorg doesn't manage windows itself. `nervus-window-manager.service` starts Openbox before nervud:
ordinary apps are maximised with no border, 80 px is reserved on the right of the screen, and the system
navigation window titled `Nervus Navigation` is pinned there and kept on top. The navigation bar's Back
sends `Escape` to the foreground window, consumed by the app's `NervusBackHandler`; Home starts or
activates the desktop without ending any app process.

The installer installs `openbox`, `wmctrl` and `xdotool`. `fix-display.sh` also repairs the Xorg and window
manager configuration. Two commands confirm what's actually in control:

```sh
systemctl status nervus-window-manager.service
DISPLAY=:0 XAUTHORITY=/run/nervus-x11/xauth wmctrl -m
```

`fix-display.sh` also prints the framebuffer and active output geometry from `xrandr --current` and the
renderer from `glxinfo -B`. A renderer containing `llvmpipe` / `softpipe` / `swrast` means software
rendering; a hardware name such as Mali/Panfrost means OpenGL really reaches the GPU.

> ⚠️ **The platform JRE must include the `java.desktop` module**
>
> ```sh
> /usr/lib/nervus/jre/bin/java --list-modules | grep java.desktop
> ```
>
> A JRE trimmed with `jlink` easily misses it, and without it Compose **can't run a single line**.
> Add `--add-modules java.desktop` when running jlink.

#### The shared user folder

The file manager and any app declaring `perm.storage.user` share `/var/lib/nervus/user-data`
(`01777` sticky).

Without that permission an app **can't write anywhere** under `ProtectSystem=strict`: the UI opens, but
it can't even create a folder.

> **v1 trade-off**: sticky only stops "deleting other people's files", not "reading" them. It's the shared
> external storage Android had before scoped storage. Per-package isolation needs a GID per package plus
> `SupplementaryGroups`; that's v2.

### 3.4 The trust root

```sh
install -D -m 0644 -o 0 -g 0 trust-bundle.json "$ROOT/usr/share/nervus/trust/bundle.json"
```

The trust bundle records which key_ids may take which roles. **It is itself signed by the platform root
key**, and the platform root public key is embedded in the nervud binary.

> ⚠️ **Development builds have no embedded platform root**
>
> A development nervud prints:
>
> ```
> WARN pkgregistry: trust store unavailable; non-Ordinary trust disabled
>      err="trust bundle signature invalid: no embedded platform root (dev build)"
> ```
>
> As a result **no `platform`/`oem` role signature verifies**, and every system package drops to
> `ordinary`. That's normal on a dev machine; **on a production image it means the build is
> misconfigured** and trust levels are broken for the whole machine.

#### key_id is the sha256 of the **raw 32-byte** public key

```
key_id = "sha256:" + hex(sha256(raw 32-byte ed25519 pubkey))
```

Three places must compute the same value. If any of them uses the wrong bytes, verification fails with
`signer key not trusted for its role`:

| Place | How it computes it |
|---|---|
| The kernel's `keyIDOf` in `pkgregistry/signature.go` | `sha256(ed25519.PublicKey)`, which in Go is the raw 32 bytes |
| `key_id()` in `build-release.sh` (written into the bundle) | `openssl pkey -pubout -outform DER \| tail -c 32 \| sha256sum` |
| `ManifestSigner` in `signing-lib` (written into manifest.sig) | Strips the X.509 SPKI down to the raw 32 bytes first, then sha256 |

> ⚠️ The third one bit us once: JCA's `PublicKey.getEncoded()` gives the **X.509
> SubjectPublicKeyInfo DER** (44 bytes for Ed25519 = a 12-byte prefix + the 32-byte key), and hashing
> that directly gives a key_id that never matches the bundle.
>
> The symptom is deceptive: the packages **install and run**, just with `trust=ordinary`, because
> `scanSystemImage` fails closed to Ordinary on a failed signature check instead of skipping the package.
> At the time, the system services signed `platform-release` (the Go `sysmanifest` hashes the raw key)
> were `platform` and the system apps signed `platform-systemapp` were all `ordinary`: half right, half
> wrong, easily mistaken for a problem with the trust root itself.
>
> If `nervusctl list` shows **some** `platform` and some `ordinary`, search nervud's journal for
> `signature not verified` first; that warning carries the real err.

---

## 4. Building (cross-compiling on the build machine)

### 4.1 The kernel

```sh
cd repo/nervud
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 \
  go build -trimpath -ldflags "-s -w" -o out/nervud .
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 \
  go build -trimpath -ldflags "-s -w" -o out/nervusctl ./cmd/nervusctl
```

`CGO_ENABLED=0` is required: the target's glibc may not match the build machine's, and a missing library
in a dynamically linked binary shows up as "the process exits as soon as it starts", with systemd only
reporting the exit code.

### 4.2 System services

```sh
cd repo/nervus-system-server
scripts/build-image-tree.sh ./out signing/platform-release.pem linux-arm64
```

One step: cross-compile → compute digests → fill the manifest → Ed25519 signature. The output is a
directory tree that can be copied straight into `system-packages/`.

### 4.3 System apps (Kotlin / Compose Desktop)

```sh
cd repo/nervus-system-software

# The target is arm64 by default; for an x86_64 machine use -Pnervus.abi=linux-x86_64
./gradlew nspkgImageTree
```

The output is in each subproject's `build/outputs/image-tree/<package_id>/`, the same shape as the
system services' `build-image-tree.sh` output (manifest.json + manifest.sig + payload).

> ⚠️ **The ABI must match the target explicitly**
>
> Compose's renderer, skiko, has **platform-specific** native libraries. `nervus.abi` decides which
> platform's skiko is pulled in, and is also written to the manifest's `supported_abis` (which the kernel
> matches against the host ABI on install).
>
> **Don't** use `compose.desktop.currentOs`: that's the **build machine's** platform. Cross-building an
> arm64 package on x86_64, it quietly adds the x86_64 skiko; the package builds and installs, and only
> fails with `UnsatisfiedLinkError` on the device.

> ⚠️ **`gradlew` must have LF line endings**
>
> A Windows checkout gives a CRLF `gradlew`, and running it under Linux/WSL reports
> `./gradlew: cannot execute: required file not found`. That looks like "file missing", but it's a `\r`
> at the end of the shebang line. The repository's `.gitattributes` pins `gradlew text eol=lf`; for an old
> checkout run `sed -i 's/\r$//' gradlew` first.

Signing key (`platform-systemapp` role):

```sh
openssl genpkey -algorithm ed25519 -out signing/platform-systemapp.pem
```

### 4.4 All three repositories must point at the same nervus-ipc commit

```sh
repo/nervud/scripts/sync-ipc.sh              <commit>
repo/nervus-system-server/scripts/sync-ipc.sh <commit>
```

A version split fails in a way that's especially hard to find: **both sides compile and pass their tests,
and it only breaks at run time on the target.**

> ⚠️ **One commit apart is enough**
>
> An interface's `schema_hash` covers the whole FileDescriptorSet, **including every recursive import**.
> Touch `nervus/ipc/v1/method_registry.proto` once and **every interface's** hash changes, even ones whose
> own proto didn't change a character.
>
> The result is that a Provider's `RegisterEndpoint` is refused:
>
> ```
> err="interface schema hash does not match the catalog"
> ```
>
> And the symptom gets **passed along**: the Provider can't register → the caller's Resolve takes the
> on-demand branch → it gets a `RESOURCE_NOT_FOUND` pointing at the selector. The debugging direction is
> exactly backwards.
>
> Step ⓪ of `build-release.sh` now checks the ipc versions of nervud and system-server and dies on a
> mismatch. When they match it prints `✓ nervus-ipc versions match: v0.0.0-…`. With a local
> `IPC_DIR`, both are built against that same directory and the check is skipped.

---

## 5. Deploying into an image

```sh
# 1. Lay out directories
sudo ./provision-rootfs.sh /mnt/target-rootfs

# 2. Kernel + CLI + unit
ROOT=/mnt/target-rootfs
sudo install -D -m 0755 repo/nervud/out/nervud     "$ROOT/usr/libexec/nervus/nervud"
sudo install -D -m 0755 repo/nervud/out/nervusctl  "$ROOT/usr/bin/nervusctl"
sudo install -D -m 0644 nervud.service             "$ROOT/etc/systemd/system/nervud.service"

# 2b. Log retention (without it a reboot loses all logs, see §2.5)
sudo install -D -m 0644 journald-nervus.conf \
  "$ROOT/etc/systemd/journald.conf.d/10-nervus.conf"

# 3. System services
sudo cp -r repo/nervus-system-server/out/* "$ROOT/usr/lib/nervus/system-packages/"

# 3b. System apps (desktop / settings / files / permissions / store)
for p in launcher settings-app filemanager permissionui store; do
  sudo cp -r repo/nervus-system-software/$p/build/outputs/image-tree/* \
    "$ROOT/usr/lib/nervus/system-packages/"
done

# 4. Trust root
sudo install -D -m 0644 trust-bundle.json "$ROOT/usr/share/nervus/trust/bundle.json"

# 5. Platform JRE (only needed if any component is runtime=jvm)
sudo cp -r jre-arm64/* "$ROOT/usr/lib/nervus/jre/"

# 6. Start at boot
sudo ln -sf /etc/systemd/system/nervud.service \
  "$ROOT/etc/systemd/system/multi-user.target.wants/nervud.service"
```

> **Ownership must be root.** If the image is built as non-root, use `fakeroot` or force `uid=0 gid=0`
> in the `mkfs`/packing step. With the wrong owner, preflight decides the read-only area has been
> tampered with and nervud refuses to start.

---

## 6. Verifying the first boot

```sh
systemctl status nervud
journalctl -u nervud -n 100 --no-pager
```

### What you should see, item by item

**① Preflight passes** (there is **no** `preflight: SKIPPED` line; that's a development switch)

**② System packages are loaded**

```
pkgregistry: loaded system package  package_id=nervus.pkgmanagerd  trust=platform
```

> `trust=ordinary` means the signature wasn't verified. **On a production image, ordinary means the
> trust root is misconfigured**; see §3.4.

**③ The kernel provisioned the run-time prerequisites**

```
audit action=EnsureAppUser  detail="nervus-app-20000 uid=20000"
audit action=CreatePrivateDataDirectory  detail=/var/lib/nervus/package-data/nervus.pkgmanagerd
pkgregistry: provisioned package runtime prerequisites  ok=N total=N
```

**④ Components are really running**

```sh
systemctl list-units 'nervus-*'
# every always-on component should be active/running
```

**⑤ IPC works**

```
ipc: handshake complete  package=nervus.pkgmanagerd  uid=20000
audit action=endpoint.RegisterEndpoint  denied=false  detail=nervus.interface.pkg.manager
```

**⑥ The ops CLI works**

```sh
nervusctl list
```

**⑦ The desktop is up** (when the system apps are installed)

```sh
# launcherd is always-on and should be active/running
systemctl status nervus-nervus.launcher-launcherd.service

# once it has started the two UIs, desktop and navigation both appear
systemctl status nervus-nervus.launcher-desktop.service
systemctl status nervus-nervus.launcher-navigation.service
```

nervud's journal should show:

```
audit action=ipc.LaunchComponent  subject=nervus.launcher  detail="target=nervus.launcher/desktop"
```

**The desktop process's own log is in its own unit**:

```sh
journalctl -u nervus-nervus.launcher-desktop.service -n 50
```

When the GUI won't start it's nearly always one of these three, and the error tells you which:

| In the log | Cause |
|---|---|
| `Can't connect to X11 window server` | Wrong `DISPLAY`, the X server isn't running, or the cookie isn't readable |
| `NoClassDefFoundError: java/awt/...` | The platform JRE lacks the `java.desktop` module |
| `UnsatisfiedLinkError` mentioning skiko | The package's ABI doesn't match the machine (see §4.3) |

### A one-line health check

```sh
journalctl -u nervud --no-pager | grep -E \
  "level=(ERROR|WARN)|loaded system package|handshake complete|RegisterEndpoint"
```

---

## 7. Troubleshooting

Component logs are **not** in nervud's journal; each has its own:

```sh
journalctl -u 'nervus-<package_id>-<component_id>.service' -n 50
```

### By symptom

| Symptom | Cause | Fix |
|---|---|---|
| nervud exits at start, log mentions `preflight` | Directory/mode/owner mismatch | Go through §2 row by row; the error names the row |
| Exits at start, mentions `CAP_SYS_NICE` | The unit lacks `AmbientCapabilities=CAP_SYS_NICE` | See `nervud.service` |
| `Unit nervud.service not found` | The unit was renamed, or the binary is running bare | The unit must be called `nervud.service` |
| Component `217/USER` | The UID isn't in `/etc/passwd` | The kernel creates it. If it didn't, the boot scan never reached that package; check for `loaded system package` first |
| Component `226/NAMESPACE` | The private data directory doesn't exist | Make sure the **root** `/var/lib/nervus/package-data` exists and is writable; the kernel creates the children |
| Component `203/EXEC` | Executable not found | Check the manifest's `entry` against the real path; system packages have **no version subdirectory** |
| Install fails with `EXDEV` | staging and packages are on different filesystems | See the warning in §2 |
| Install fails with `permission denied` while unpacking | The staging root isn't `0711`, or has the wrong owner | See §2 |
| `trust=ordinary` (production) | The trust root isn't set up | See §3.4 |
| `signer key not trusted for its role` | The signing role doesn't match the bundle | System services use `platform-release`, system apps `platform-systemapp`, third-party apps `developer` |
| Desktop or navigation bar doesn't appear, `launcherd` is running | `LaunchComponent` was refused, or Openbox didn't take over | Check the `ipc.LaunchComponent` audit in nervud's journal, then `systemctl status nervus-window-manager` and `wmctrl -m` |
| The desktop unit keeps restarting | Compose can't start | Check its own unit log against the three causes in §6 ⑦ |
| **Desktop/Settings list no apps at all**, log only says `failed to resolve optional interface` | Wrong interface_id, or the caller's manifest doesn't declare the interface's permission | See "Interface won't resolve" below |
| **After a reboot `journalctl` has nothing from before** | Logs are on tmpfs, not persisted | `journalctl --header \| grep journal` should show `/var/log/journal`. If not, the drop-in isn't installed or the main file's `Storage=volatile` overrides it; see §2.5 |
| A stretch of the log is **completely empty**, fine before and after | journald rate limiting silently dropped it | That component exceeded `RateLimitBurst` within 30 s. nervud itself is exempt (`LogRateLimitIntervalSec=0`), so the kernel log never does this |
| `audit-verify` reports `unclean shutdown(s)` | Power was lost at those points; the tail may be missing records | **Not tampering.** The chain itself is intact; see §2.5. If it's frequent, the machine is losing power uncleanly |
| `audit-verify` reports `records before seq N were rotated out` | Old records were reclaimed by rotation | Normal. The audit cap is 80 MiB, oldest first |
| File manager says "The user folder isn't writable" | Missing `perm.storage.user`, or `/var/lib/nervus/user-data` isn't `01777` | For the former check the manifest, for the latter the preflight log |
| **Local screen completely black, ssh works, `nervus-x11` stuck in `activating auto-restart`** | Xorg crash loop | See "Black screen" below |

### Interface won't resolve

A failed `resolveOptional` leaves just one WARNING and **doesn't say which step failed**. Two causes have
bitten us on real hardware, and both look like "blank desktop, one warning in the log":

1. **Misspelt interface_id.** `nervus.interface.pkgmanager` ≠ `nervus.interface.pkg.manager`. Two
   authoritative values must agree: the kernel's catalog (which sets the threshold) and the provider's
   `interfaceID` constant (`nervus-system-server/pkgmanagerd/main.go`). **The proto's directory name
   (`nervus/interface/pkgmanager/v1/`) is not the interface_id**; don't copy it.

2. **The caller's manifest doesn't declare the required permission.** `Resolve` decides at the
   **interface** level, with the threshold from the kernel's catalog:

   | Interface | Permission needed |
   |---|---|
   | `nervus.interface.pkg.manager` | `perm.pkg.query` (plus `perm.pkg.install` for changes) |
   | `nervus.interface.safety.control` | `perm.safety.rearm` |
   | `nervus.interface.manipulator.arm` | `perm.manipulator.control` |
   | `nervus.interface.motion.base` | `perm.motion.control` |
   | `nervus.interface.power` | `perm.authority.power` |

   `V1GrantAll` only skips the run-time user confirmation; **the permission must still be listed under
   the manifest's `permissions`**, or you get `PERMISSION_DENIED`.

When debugging, search nervud's journal directly:

```sh
journalctl -u nervud -b | grep -i 'ResolveEndpoint'
```

The audit shows `denied` and the exact permission ID, which tells you far more than the app's one warning.

### Black screen (nothing on the local screen, ssh works)

Run `sudo ./fix-display.sh` first. It only rewrites the display session layer, touches no packages and
needs no rebuild, and if it still fails afterwards it prints Xorg's own error lines.

Causes seen on real hardware, most frequent first:

| Cause | Looks like | Notes |
|---|---|---|
| `start-x11` used `vt1` | Completely black screen, not even console text | `getty@tty1` runs on `vt1`. Xorg taking it over clears it, and a crash + `Restart=always` pins the screen black for good. **Use `vt7`**, as every display manager does |
| `/usr/bin/Xorg` is `Xorg.wrap` | `Only console users are allowed to run the X server` in the journal | The `xserver-xorg-legacy` wrapper checks `allowed_users` in `/etc/X11/Xwrapper.config`, defaulting to `console`, and processes started by systemd have no console. The workaround is to exec `/usr/lib/xorg/Xorg` directly and also set `allowed_users` to `anybody` |
| `/dev/dri` is empty | `no screens found` in the Xorg log | The kernel DRM driver isn't loaded, a problem outside X. Vendor boards (RDK / Jetson and the like) usually need the vendor's own display driver package |
| X is up but the desktop can't connect | `Authorization required, but no authorization protocol specified` + `Can't connect to X11 window server` in the component log | The cookie chain is broken; see below |

#### Why the X server runs with `-ac`

A component gets the X cookie through a three-hop chain:

```
nervud's environment has XAUTHORITY (the 10-display.conf drop-in)
  → only then does supervise.go add the cookie file to BindReadOnlyPaths
    → only then is the file in the component's mount namespace
```

If any hop breaks, the symptom is **always the same** `Authorization required, but no authorization
protocol specified`: the component reaches the socket (the `/tmp/.X11-unix` bind worked), just without
credentials, and the log can't tell you which hop broke.

So X starts with `-ac` (access control off). This does **not** expose the display: `-nolisten tcp`
already turns off network listening, so only local processes can connect, and the cookie file is `0644`
and readable machine-wide anyway (the chosen trade-off for a single-user device). Security was already
equivalent to `xhost +local:`; `-ac` just makes that trade-off take effect reliably. `-auth` stays, so
clients with the cookie still connect with credentials as usual.

Xorg's own log is fixed at `/var/log/nervus-x11.log` (the unit's journal only has the wrapper's output;
the real `(EE)` lines are in that file).

### Switches not for production

| Switch | Effect |
|---|---|
| `-dev-skip-preflight` | Skip the image area self-check |
| `-dev-allow-sched-degrade` | Degrade instead of exiting when `CAP_SYS_NICE` is missing |

Both leave a `WARN` in the log. **If they appear in a production image's journal, the unit is wrong.**

---

## 8. Known limitations of this version

Worth knowing before shipping (see "Known gaps" in `nervud/README.md` for details):

| Limitation | Effect |
|---|---|
| `permission.V1GrantAll = true` | Asking is getting; nothing asks the user at run time. Manifests must still declare permissions |
| No channel carries the Safety stop signal | Detection, latching and epoch increments all run, but the stop action can't be sent |
| Every interface binds the motion base resource by default | Without a motion base in the Resource Registry, every Resolve without a selector fails: **a device with no moving base can't install software** |
| Three hard-coded constant tables | Adding a hardware model still means changing the kernel |
| `PackageInfo` doesn't project label / icon / component list | The desktop and Settings guess display names and launch components by convention (`AppIdentity`). A package with two app components gets the wrong one |
| Every system app carries its own Compose runtime | About 43 MB per package. A shared runtime needs a `platform_runtime` field in `runtime_deps` and the kernel assembling the classpath; that's v2 |

---

## Appendix: path cheat sheet

```
/usr/libexec/nervus/nervud              kernel binary
/usr/bin/nervusctl                      ops CLI
/etc/systemd/system/nervud.service      unit (name must not change)
/etc/systemd/journald.conf.d/10-nervus.conf  log persistence and size

/usr/lib/nervus/system-packages/<pkg>/  system services + system apps (no version subdirectory)
/usr/lib/nervus/jre/bin/java            platform JRE
/usr/share/nervus/trust/                trust root bundle

/var/lib/nervus/registry/               records + UID allocator      0700
/var/lib/nervus/packages/<pkg>/<ver>/   dynamically installed code   0755
/var/lib/nervus/package-data/<pkg>/     per-package private data     0700, owned by the package UID
/var/lib/nervus/staging/                install staging              0711
/var/lib/nervus/jvm-cache/              JVM cache                    0755
/var/lib/nervus/user-data/              shared user documents        1777 (sticky)
/var/lib/nervus/audit/audit.jsonl       security audit hash chain    0700, 1-second power-loss window

/var/log/journal/                       ops log, 500 MB              2755, 5-minute power-loss window

/run/nervus/nervud.sock                 app control plane (app UIDs 20000-59999)
/run/nervus/nervud-admin.sock           admin channel (root + install service)
```
