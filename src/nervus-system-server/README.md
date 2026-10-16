# nervus-system-server

System-Server for Nervus OS: the repository of Nervus OS system services.

## 1. What this repository holds

1. **Capability providers**: hardware or platform capabilities made into interfaces for apps and
   other services to call.
2. **System services that need to stay running in the background.**

A system service is **one binary**, preferably written in Go (everything in this repository is Go).
Once written, it's packaged, signed and installed into the system image.

**Services always talk to the kernel over proto IPC** (`nervus-ipc/sdk`): identity, registration,
resolution, permissions and data-plane authorisation all go over that one line, with no second route.

**Data between services, and between services and apps, uses their own protocols.** The kernel only
proves who you are, whether you're allowed, and how big a pipe you can open; it doesn't look at or parse
what flows through the pipe.

> **Writing a new service?** See **[WRITING-A-SERVICE.md](WRITING-A-SERVICE.md)**: the full process from
> creating the directory to deploying it on a device, with a table of failures by symptom.

## 2. Three lines

```text
Control plane (proto IPC, always through the kernel)
  App / service  ──Envelope──▶  nervud  ──Dispatch──▶  service
              handshake · register · resolve · permissions · leases · data-plane authorisation

Data plane (the kernel only issues authorisation, never reads content)
  service  ◀──Transfer (opaque bytes, the two ends' own protocol)──▶  App / service

File plane (the kernel only manages mounts and ownership)
  /run/nervus/shared/<pkg>/      tmpfs, run-time exchange
  /var/lib/nervus/shared/<pkg>/  disk, persistent
```

## 3. Layout: one directory per service

**Every directory at the root is one system service**, with its code directly inside. The only
exception is `scripts/` (build tools).

```text
nervus-system-server/
├── pkgmanagerd/            ← service
│   ├── main.go             process entry point
│   ├── service.go          business implementation
│   ├── handlers.go         method dispatch
│   ├── adminclient.go      admin channel client
│   ├── unpack.go           .nspkg unpacking + tar-slip guard
│   ├── providergen/        ← produces the Provider contract (see section 4)
│   ├── manifest.json.in    package manifest template
│   └── README.md
├── safetyrecoveryd/        ← service (Client example)
├── camerad/                ← service (multi-endpoint + data-plane example)
│   ├── boardprofile/       board JSON parsing (one parser shared by build time and run time)
│   ├── boards/             board configs, one per board
│   ├── providergen/        ← reads the board JSON and produces resource declarations
│   └── README.md
└── scripts/                build tools, not services
    ├── build-image-tree.sh
    ├── sync-ipc.sh
    └── sysmanifest/        digest computation + manifest filling + Ed25519 signing
```

**Service directory names must not collide**: a name is also the binary name, the Go package path,
and the build target in `build-image-tree.sh`.

### Services can't import each other, and the compiler guarantees it

**Every `.go` file in this repository is `package main`.** Go forbids importing `package main`, so
service A trying to import service B **fails to compile**.

That isn't kept up by convention or code review; the language blocks it. When code needs sharing, put
it in `nervus-ipc` (the home of the protocol and SDK), and **don't** create a shared package here.

> `providergen/` is another `package main` in a subdirectory and can't be imported either.

## 4. Provider contract: packages that export interfaces must carry one

**This is the easiest trap in this repository.**

The kernel's `loadRequiredProviderArtifacts` returns `ErrProviderArtifactsRequired` for "the manifest
has `exports` but no `provider` section". **There's no exception channel**: there used to be a
compatibility bridge just for `nervus.pkgmanagerd`, removed once this repository's packaging chain
could produce the files.

The contract is produced by the service's own `providergen/` as two files:

```text
provider.binpb   deterministic protobuf bytes of a ProviderDescriptor
schemas.binpb    deterministic protobuf bytes of an InterfaceSchemaBundleSet
```

`build-image-tree.sh` runs it after compiling and before `sysmanifest`, so both files are included in
the `digests`, and the manifest says:

```json
"provider": {
  "descriptor": "provider.binpb",
  "schemas": "schemas.binpb"
}
```

### Why each service has its own instead of the packager generating them all

Generating a schema needs to import the interface's generated code. If `sysmanifest` did it for
everyone, it would have to import every interface's generated package, turning "add a capability" into
"change the packaging tool", exactly the coupling the data-driven Catalog is meant to remove.

### A capability interface can have no protobuf messages at all

`ProvidedInterfaceVersion.methods` allows inline method metadata (method_id, permission, risk,
Transfer budget), and then **no schema bundle and no `.proto` messages are needed**.

Camera frames, microphone samples and radar point clouds go through Transfer as opaque bytes anyway.
Inventing Request/Response messages for them that will never be used would just give "add a
capability" an extra `.proto` to maintain, generate and keep aligned between parties.

The kernel only really needs to know three things: **who's calling, whether it's allowed, and how big a
pipe can be opened**. All three are in `MethodMeta`, regardless of message shape.

The contract identity still exists, computed with `registry.MethodsHash(methods)` instead of a schema
hash. So the guarantee that "several vendors implementing the same standard interface must match byte
for byte" **is fully kept**.

## 5. Permissions and trust

**Declaring is the only way to possibly get one.** The manifest's `permissions` are a request; the
actual grant is the kernel's intersection of source + trust + signing role + GrantMode
(`permission.Registry.IntersectAt`).

Some this repository uses:

| Permission | Threshold | Use |
|---|---|---|
| `perm.service.register` | OEM trust + PRIVILEGED | Register a public interface |
| `perm.service.register.private` | Ordinary + NORMAL | An interface visible only inside the package |
| `perm.pkg.admin` | Platform trust + SYSTEM_ONLY + `platform-release` signature | Connect to the admin channel, write to staging |
| `perm.storage.shared` | Ordinary + NORMAL | The shared area between services |
| `perm.storage.user` | Ordinary + USER_CONSENT | The user documents area |

> **The kernel doesn't know any specific Package ID.** The install service can reach the admin channel
> because it holds `perm.pkg.admin`, not because it's called `nervus.pkgmanagerd`. The package name
> constant in `main.go` is gone. The only remaining hard-coded package names are the cannot-be-disabled
> list in `pkgregistry/lifecycle.go`, which is about "users can't disable key components", not about
> granting abilities.

### Development builds: `-dev-trust-system-packages` is required

Production binaries get the platform root public key injected with `-ldflags`; development builds
don't, so `LoadTrustStore` fails and every system package fails closed to `Ordinary`, while
`perm.service.register` needs OEM. **The result is that on a dev machine no service that exports a
public interface can register.**

```sh
nervud -dev-skip-preflight -dev-allow-sched-degrade -dev-trust-system-packages
```

This switch relaxes exactly one thing: **whether this signing key is authorised by the embedded
platform root**. Every signature is still really verified with Ed25519, the key_id must still equal the
public key's sha256, and digests are still hard checks. A modified binary, a modified manifest, or an
attempt to impersonate some key_id are all still refused.

(A release built with `nervus-deploy/build-release.sh`, which is what NervusOS-Cambridge installs, has
the platform root embedded and doesn't need this switch.)

## 6. The shared area between services

```text
/run/nervus/shared/<package_id>/      tmpfs, run-time exchange
/var/lib/nervus/shared/<package_id>/  disk, persistent
```

Owner = the package's UID, mode **0755**. A package holding `perm.storage.shared` can **write its own
subdirectory and read everyone else's**.

**Created on demand**: only packages granted `perm.storage.shared` get these two directories. Most
services don't need a shared area, and creating one for every package would waste inodes on tmpfs and
make the directory list disagree with who actually uses it.

### Five deliberate decisions

**① The root is nervud's alone; packages only write their own subdirectory.**

Letting packages write the root wouldn't just bypass the "one directory per package, owner = writer"
structure; the more direct risk is **squatting a name first**: a malicious package creates
`nervus.camerad/` and owns it, and camerad finds its own path unwritable when it starts. The sticky bit
stops "deleting other people's", not this. With the root writable only by nervud, that attack doesn't exist.

> Android goes further: apps **have no `/tmp` at all**, and `java.io.tmpdir` points at the app's own
> private directory. A shared `/tmp` is the classic breeding ground for symlink attacks and squatting.
> We keep a shared root because it has uses memfd can't cover (see the UDS note in section 7), but the
> root must be writable by the kernel alone.

**② 0755, not 0700 and not 0777.**
0700 degrades into a second private directory, with no sharing; 0777 lets any package tamper with the
configs and models others put out. 0755 is exactly "anyone can read, only the owner can write", without
opening write access to everyone. Per-package fine-grained write grants (ACLs) are left for v2.

**③ Deliberately separate from the user documents area (`perm.storage.user`).**
That's "the user's documents", aimed at apps and file pickers, and therefore `USER_CONSENT` +
`PRIVACY_SENSITIVE`. Two services wanting to drop an intermediate file shouldn't turn into "ask the user
for access to their documents".

**④ The tmpfs root is rebuilt every boot.**
Private data directories are only created at first install, but `/run` is empty after a reboot; if the
boot scan didn't fill it back in, a service's first write would get `ENOENT`.

**⑤ The criterion is the decided `GrantedPermissions`, not what the manifest asks for.**
Creating directories by request would let any package claim a spot by writing one line of manifest.

### ⚠ Boundary: sensitive data can't go here

Isolation of reads across packages in this system relies **only on the 0700 data directories** (a
component sandbox's `InaccessiblePaths` only lists the registry directory). So the 0755 shared area is
**readable system-wide**.

**The shared area only holds things that "anyone with `perm.storage.shared` may see".** Data that needs a
real permission threshold, such as camera frames, must go through Transfer's memory handles: the handle is
itself the credential, with no filesystem path to get around it. Writing such data into the shared area
would let any app bypass `perm.camera.capture` and read the files directly.

### Put your own service UDS here

To talk to another service with your own protocol, create the socket under
`/run/nervus/shared/<your package>/`: the directory is 0755, so other services can see the path and
connect. That's the most direct way to do "services talk to each other in their own protocol", with no
memfd needed.

**UDS suits control and small messages; heavy traffic goes through Transfer (section 7).**

## 7. High-throughput data: use Transfer, not files

The control plane's Envelope doesn't carry large data. Once a method declares a budget in
`MethodMeta.transfer`, the Provider may create a Transfer; nervud tightens direction, mode, packet size and
rate according to the authoritative metadata, caller permissions, connection budget and route lifetime, and
issues two short-lived one-time handles.

Once both ends attach to the kernel's own Transfer socket, **what flows inside is up to them**: the kernel
only frames and rate-limits, without parsing content.

> **Don't stream video through files on disk.** Raw 1080p30 is about 93 MiB/s, and writing that
> continuously to eMMC burns through the flash's life. Heavy run-time traffic either goes through
> Transfer or sits in the tmpfs shared area.

## 8. Where it's deployed

System packages are installed in the **read-only image area**:

```text
/usr/lib/nervus/system-packages/
├── nervus.pkgmanagerd/
│   ├── manifest.json
│   ├── manifest.sig
│   ├── provider.binpb
│   ├── schemas.binpb
│   └── bin/pkgmanagerd
└── <the next service>/
```

The directory name must equal `package_id` in `manifest.json`. The kernel's boot scan is
`Glob("/usr/lib/nervus/system-packages/*/manifest.json")`, with **no version subdirectory**: system
packages follow whole-image OTA, and several versions never coexist.

**They arrive by image flash or OTA, not with `nervusctl install`**: that path only accepts
`SourceDynamicInstall`, and a system package sent through it is refused (`ErrSystemPackageImmutable`).

Run-time paths are assigned by the kernel; nothing needs creating by hand:

| Path | Use |
|---|---|
| `/var/lib/nervus/package-data/<pkg>/` | Private data directory, owned by the package UID, `0700` |
| `/run/nervus/shared/<pkg>/` | Shared area (tmpfs), `0755` |
| `/var/lib/nervus/shared/<pkg>/` | Shared area (disk), `0755` |
| `/var/lib/nervus/registry/<pkg>.json` | UID and grant records, kept across reboots |

Build:

```sh
scripts/build-image-tree.sh <output dir> <signing-key.pem> [ABI]
cp -r <output dir>/* /usr/lib/nervus/system-packages/
```

The output is an **unpacked directory tree, not a `.nspkg`**. The latter is the zstd+tar format for
dynamic install, and the two paths must not be mixed.

**What ships is binaries, not source.** The target needs no Go toolchain, no source and no network, just
statically linked binaries (`CGO_ENABLED=0`) plus metadata. That's also why digests and signatures must be
computed at build time: the target has nothing to recompute them with.

## 9. Start order: random, and services must cope

`service.Manager.Start()` walks a Go map, and **the Go runtime deliberately randomises map iteration
order**, so system services don't start "in no particular order by chance"; the order is different on
every boot. The kernel also has no dependency declarations (no `After=`).

So the right way to write every service is: **if it can't reach the control plane, fail and exit**, and let
the supervisor restart with exponential backoff. Don't write a retry loop of your own: it would hide the fact
that "nervud isn't up" and make the process look healthy.

The crash budget never runs out:

```
backoff 1s → 2s → 4s → 8s, crashes at about t = 0, 1, 3, 7, 15
breaker = 5 crashes in a 10-second sliding window

t=7   4th crash; window [-3,7] holds 0,1,3,7 → 4 < 5  ✅
t=15  5th crash; window [5,15] holds only 7,15 → 2 < 5  ✅
```

**But be careful with `criticality: vital`**: if it really trips, it latches Safety and stops the whole
machine. Unless the machine shouldn't move while this service is down, use the default `optional`.

### One bad package doesn't take start-up down

Catalog conflicts (a resource with several managers, inconsistent interface contracts, namespace
overreach) used to fail the whole start-up. Now the boot scan **quarantines the offending package by
source**, audits it loudly and carries on.

The install path **doesn't quarantine**; it's still all or nothing: a new package with a conflict is
refused, and the existing system isn't touched.

## 10. Service list

| Package ID | Status | Job |
|---|---|---|
| `nervus.pkgmanagerd` | Working (install, inspect, uninstall, list; used by the App Store) | Software install |
| `nervus.safety.recovery` | Observing only | Safety re-arm policy (the kernel builtin already exposes `REARM`) |
| `nervus.camerad` | Working skeleton | General camera capability + V4L2 compatibility layer (see [camerad/README.md](camerad/README.md)) |
| `nervus.sessiond` | Planned | Control-principal (HUMAN/AI) sessions |

These IDs aren't names this repository chose: they're written on the **cannot-be-disabled list** in the
kernel's `internal/pkgregistry/lifecycle.go`. Renaming means changing both sides.

`nervus.settings` and `nervus.permissionui` are on that list too, but they're Compose Desktop apps and
live in `nervus-system-software`.

## 11. Why these services aren't in nervud

The kernel only provides **mechanism**; policy lives in services (the same split as Safety re-arm:
mechanism in the kernel, policy in a service). Putting things that will evolve with the product, like the
install flow and session management, into the kernel would make "change the install flow" and "change the
kernel" the same thing.

In turn they don't get the kernel's powers: they're ordinary Packages in the app UID range (20000-59999),
in the same systemd sandbox.

## 12. Signing

- Only `ed25519` is accepted; everything else is refused, with no "best-effort compatibility"
- The signature covers `"nervus-pkg-manifest-v1\x00" || the raw bytes of manifest.json`
- Role `platform-release` (the kernel's `sigblock.go`: "platform: nervud + core system services")
- `key_id` is the `sha256:` hex digest of the public key

```sh
openssl genpkey -algorithm ed25519 -out signing/platform-release.pem
```

`scripts/sysmanifest` is written in Go rather than calling `nervus-packaging`'s Kotlin `signing-lib`: this
repository is pure Go, and `manifest.sig` is just JSON + Ed25519, which the standard library produces
directly, so there's no reason to drag a JVM toolchain into the build for signing. The cost is two copies of
the format definition, held together by the assertions in `scripts/sysmanifest/manifest_test.go`.

## 13. Development environment

**Build and test on Linux or WSL.** The services themselves are cross-platform, but the nervud they connect to
is `//go:build linux`, so end-to-end checks only work on Linux.

```sh
scripts/sync-ipc.sh    # sync the nervus-ipc dependency; must point at the same commit as nervud
go build ./...
go vet ./...
go test ./...
```

`nervus-ipc` is this organisation's own module, and a freshly pushed commit often isn't indexed on
sum.golang.org yet, so a plain `go get` hits a 500. The script sets `GOPRIVATE`.

## 14. Hard rules

1. **Never hand-roll an envelope.** All communication with the kernel goes through `nervus-ipc/sdk`, which
   reuses the generated types of the frozen protos.
2. **Security decisions aren't made in this repository.** Authorisation, signature checks and decisions all
   live in nervud. Writing any code here shaped like "if ... then allow" is a design error.
3. **Check the table before sending a body.** The "implementation status" table in the `nervus-ipc` root
   README is the only authority: frozen in the proto doesn't mean supported by nervud, and sending an
   unimplemented body gets the connection closed, while the SDK side just sees "the connection dropped for
   no reason".
4. **Sensitive data stays out of the shared file area.** It's readable system-wide (see the boundary note in
   section 6).
