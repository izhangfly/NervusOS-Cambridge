# nervus.pkgmanagerd

The software install system service. It offers apps an IPC interface and forwards internally to nervud
over the admin channel.

```text
App (Kotlin)  ──IPC──▶  pkgmanagerd  ──adminwire──▶  nervud
                             │
                        unpack .nspkg (tar-slip guarded)
```

## Where it's installed

```text
/usr/lib/nervus/system-packages/nervus.pkgmanagerd/
├── manifest.json          ← the exact bytes that are signed
├── manifest.sig           ← Ed25519 signature by the platform-release role
├── provider.binpb         ← Provider contract (see the repository README, section 4)
├── schemas.binpb
└── bin/pkgmanagerd        ← payload, which must be covered by manifest.digests
```

The directory name **must** equal `package_id` in `manifest.json`. The kernel's boot scan is

```go
filepath.Glob(filepath.Join("/usr/lib/nervus/system-packages", "*", "manifest.json"))
```

It identifies the package by the content of `manifest.json`, and the directory is only a container; but a
mismatch sends people debugging to the wrong package, with no benefit at all.

**This is the read-only image area** (preflight checks that `/usr/lib/nervus/system-packages` is `0755` and
not writable). Getting it there takes an image flash or OTA, not `nervusctl install`: that path only
accepts `SourceDynamicInstall`, and a system package sent through it is refused outright.

The other two run-time paths are assigned by the kernel and need no manual setup:

| Path | Use |
|---|---|
| `/var/lib/nervus/package-data/nervus.pkgmanagerd/` | Private data directory, owned by this package's UID, `0700` |
| `/var/lib/nervus/registry/nervus.pkgmanagerd.json` | UID and grant records, kept across reboots |

To produce the tree:

```sh
scripts/build-image-tree.sh <output dir> <signing-key.pem> [ABI]
cp -r <output dir>/* /usr/lib/nervus/system-packages/
```

## It's a bridge, not a decision-maker

**Every security decision is in nervud's pkgregistry**: multi-role signature checks, digest re-checks, OEM
countersign admission, host ABI matching, upgrade decisions (downgrade protection + signing lineage
continuity), permission intersection, UID allocation, atomic commit.

This service does none of them, and **isn't allowed** to. Writing any code here shaped like "if ... then
allow" is a design error.

It exists for one reason: an app can't be root and can't reach the root-only admin channel, so it needs a
service running in the app UID range, explicitly let through by the kernel, to forward for it.

## Its one privilege, and how narrow it is

This is the **only** service allowed to connect to `/run/nervus/nervud-admin.sock`. On the kernel side
that's `admin.Config.ServiceUID`: at setup the kernel looks up this package's UID in the Registry, chowns
the socket's group to it and relaxes the mode to 0660.

What's granted is **"who can connect to this socket", not "what they can do once connected"**.

`adminclient.go` deliberately lives in this service's own `package main` rather than a shared package, so
no other service can import it: "only pkgmanagerd can touch the admin channel" is enforced by structure,
not by good behaviour.

## The install flow

```text
1. CmdBeginStaging ──▶ nervud creates a staging directory and returns its absolute path
2. Unpack()            unpack the .nspkg into it (the one step this service is responsible for securing)
3. CmdInstall     ──▶ nervud re-checks signatures/digests/decisions and commits atomically
```

Step 1 **must have nervud create the directory**; the service can't pick one. That guarantees three things:

- it's on the same filesystem as PackageRoot, so the `renameat2` at install time can't fail with EXDEV
- its owner and mode are controlled
- the path-escape check at install time has a clear criterion: "it must be a directory I issued"

When step 2 fails, **staging isn't cleaned up**. We have no permission to delete it and shouldn't have: that
directory belongs to nervud, and giving the forwarding layer delete rights would open one more hole to
abuse. nervud sweeps orphan directories older than an hour at the next begin-staging.

The `.nspkg` itself comes from the shared user-data area: `InstallRequest.nspkg_relpath` is relative to
`/var/lib/nervus/user-data`. The App Store copies catalog packages into `user-data/.store/` before asking.

## Unpacking is this service's security job

Unpacking happens **before** nervud reviews anything, so a malicious `.nspkg` with `../..` or absolute-path
entries could write outside the target directory while being unpacked. nervud can't stop that for us: the
escape happens before it ever sees the tree.

Every guard in `unpack.go` answers a specific attack, and the logic is copied from the kernel's
`cmd/nervusctl/unpack.go` (proven on the production path) deliberately without "improvements":

| Guard | Stops |
|---|---|
| `safeJoin` refuses absolute paths and escapes after `..` folding | tar-slip |
| Prefix comparison **includes the separator** | `/a/staging-evil` passing as `/a/staging` |
| Only `TypeDir` / `TypeReg` accepted | symlink escapes, device nodes |
| 64 MiB per entry + an `io.CopyN` safety net | decompression bombs (including a lying `hdr.Size`) |
| At most 10000 entries | bombs made of huge numbers of small entries |
| Only the low 9 permission bits kept | setuid landing on disk as privilege escalation |
| `O_EXCL` | a duplicate entry name overwriting content that already passed review |

Trust in the content is still anchored in nervud: it re-runs `VerifyDigests` against `manifest.Digests` on
every file in staging.

## Methods

All five methods of `nervus.interface.pkg.manager` are registered (`handlers.go`):

| Method | Permission | Notes |
|---|---|---|
| `INSTALL` (1) | `perm.pkg.install`, needs user confirmation | A long task (`returns_operation`); only callers holding `perm.permission.admin` get through the confirmation gate |
| `UNINSTALL` (2) | `perm.pkg.install`, needs user confirmation | System image packages are refused with `ErrSystemPackageImmutable` |
| `LIST` (3) | `perm.pkg.query` | |
| `SET_COMPONENT_ENABLED` (4) | `perm.pkg.install`, needs user confirmation | Protected components can't be disabled |
| `INSPECT` (5) | `perm.pkg.query` | Read-only: unpack + verify + list the USER_CONSENT permissions, installing nothing |

Failures carry a typed `PackageManagerErrorDetail`. Because nervud's admin channel only returns a coarse
code plus human-readable text, `classify()` recognises the reason from that text. That's acknowledged
technical debt in nervud's adminwire, which should return structured reason codes; anything unrecognised
becomes `UNSPECIFIED`.

> INSTALL is declared `returns_operation`, and its endpoint binds no physical resource. NervusOS-Cambridge
> patched the kernel's operation manager to accept resource-free operations when there's no motion lease;
> before that, INSTALL over IPC was refused with `INVALID_ARGUMENT` at operation creation.

## Why the kernel lets it register

`endpoint/register.go` admits in seven steps; the first four can refuse:

| Step | Requirement | How this service meets it |
|---|---|---|
| 1 | The handshake completed (`caller.ComponentID` not empty) | `declared_component_id = "main"`, checked via PID → cgroup → unit |
| 2 | The interface must be declared in `manifest.components[].exports` | Declared in `manifest.json.in` |
| 2 | The component isn't disabled | It's on the protected list and can't be disabled |
| 3 | `visibility: public` → needs `perm.service.register` | Requested in `manifest.permissions` |
| 4 | A non-empty `resource_handle` must be in the Resource Registry | Left empty (this interface binds no physical resource) |

Step 5 compares `SchemaHash` byte for byte with the Catalog's copy; see `contract.go` for `schemaHash()`, which
goes through the same `BuildSchemaBundle` call as `providergen`.

## Files

| File | Job |
|---|---|
| `main.go` | Process entry: handshake → register → Serve |
| `service.go` | Business implementation, all forwarded through adminclient |
| `handlers.go` | IPC methods → service, errors → typed detail |
| `adminclient.go` | Admin channel client (**only this service can use it**) |
| `unpack.go` | `.nspkg` unpacking + tar-slip guard |
| `contract.go` | Interface constants and schema hash |
| `providergen/` | Produces the Provider contract at build time |
| `manifest.json.in` | Package manifest template; `digests` are filled in by `sysmanifest` |

## Running locally

```sh
# needs a running nervud
go run ./pkgmanagerd -sock /run/nervus/nervud.sock -log-level debug
```

The handshake fails (`UNAUTHENTICATED`) unless this process really is the component nervud started from
the manifest: `declared_component_id` is checked via PID → cgroup → systemd unit. That's expected, not a bug.
