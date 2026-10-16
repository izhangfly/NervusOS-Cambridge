# nervus-ipc

The **source-of-truth repository** for the Nervus OS (NSOS) control plane protocol. The `.proto` schemas, the generated code for each language and the SDK all live here.

Architectural basis: the nervud kernel and system services architecture decisions, §10 (the IPC v1 protocol baseline) and §10.10 (SDK boundaries).

## Why the protocol isn't inside nervud

A public protocol can't hide in `nervud/internal/`. nervud is **one consumer** of the protocol, not its owner.
Go system Services, OEM Providers and Kotlin/Java Apps all generate their types from the same schema.
Putting it in the kernel repository would make "who can change the protocol" and "who can change the kernel" the same permission.

Dependency direction (§10.10):

```text
nervud                             → nervus-ipc/protocol + registry
Go system Services / OEM Services  → nervus-ipc/sdk
Kotlin/Java Apps and their Services → nervus-app-sdk           (landed, in another repository)
```

The hand-written JVM SDK is **not in this repository**. It's in `nervus-app-sdk` (`com.nervus.sdk.ipc`).
This repository's `jvm/` only holds protobuf generated code.

---

## ⚠️ Implementation status (SDK authors, read this)

**This table is the only authority SDK authors should use to decide "can I send this body".**

Frozen in a `.proto` doesn't mean nervud supports it. When nervud receives a body that is "valid in the protocol but not implemented in this build", it
**closes the connection** and audits it as `UnsupportedBody`. On the SDK side all you see is "the connection dropped for no reason",
and the audit only goes to nervud's log, so it's very hard to track down. Check this table before sending.

**Current protocol version: 2.0. v2 is not backward compatible with v1.** If the majors can't be agreed at the handshake, the connection is refused
(a Failure `HelloAck` is sent before closing, so you get the exact reason).
The break is the removal of two implicit defaults: an empty selector in `ResolveEndpoint` or `AcquireControl`
no longer points at `{nervus.resource.motion.base, base.main}`.

| body | numbers | nervud | notes |
|---|---|:---:|---|
| Hello / HelloAck | 10-11 | ✅ | |
| ResolveEndpoint / Result | 20-21 | ✅ | An empty selector has no implicit default any more; interfaces bound to resources must specify one |
| RegisterEndpoint / Result | 22-23 | ✅ | |
| EndpointDied / EndpointRevoked | 24-25 | ✅ | nervud → peer |
| UnregisterEndpoint / Result | 26-27 | ✅ | |
| Request / Response | 30-31 | ✅ | **The main call path; short commands use it** |
| Cancel | 32 | ❌ | A caller sending it still gets the connection closed |
| Subscribe / Unsubscribe | 40, 42 | ✅ | Subscriptions are by `(endpoint, event_id)` |
| SubscribeResult / UnsubscribeResult | 41, 44 | ✅ | Success carries `delivery_class` |
| Event / SubscriptionClosed | 43, 45 | ✅ | nervud fans out; backpressure follows `delivery_class` |
| Dispatch / DispatchResult | 50-51 | ✅ | nervud ↔ Service; from v2 on it **always** carries `ExecutionContext`, which also holds a long task's `operation_id` |
| CancelDispatch | 52 | ⚠️ | Sent on deadline or caller disconnect; explicit Cancel isn't wired yet. The Go ServiceHost supports it |
| PublishEvent | 53 | ✅ | One-way report from Service → nervud, **with no result**; subscribers neither send nor receive it |
| Ping / Pong | 60-61 | ✅ | |
| AcquireControl / ReleaseControl + Result | 70-73 | ✅ | Wired to `internal/control` |
| LaunchComponent / Result | 80-81 | ✅ | |

Protos outside the Envelope:

| file | nervud | notes |
|---|:---:|---|
| `status.proto` | ✅ | |
| `safety.proto` | ⚠️ not wired | The types compile, but delivery goes to `NopPath` and reporting to `NopReports` |
| `schema.proto` | ✅ | The install chain consumes `ProviderArtifacts`; the Catalog decides by `schema_hash` |
| `method_registry.proto` | ✅ | `MethodMeta` / `EventMeta` are the authority for dispatch and subscriptions |
| `provider_descriptor.proto` | ✅ | The kernel no longer has hard-coded interface/permission/resource tables |
| `transfer.proto` | ✅ | Both FRAMED_RELAY and SHARED_MEMORY_RING are wired |

System interfaces (no body numbers of their own; they use the standard Resolve + Request):

| interface | nervud | notes |
|---|:---:|---|
| `nervus.interface.transfer.control@1` | ✅ | Begin / Commit / Abort for data plane pipes |
| `nervus.interface.resource.directory@1` | ✅ | Read-only device directory, filling ResourceSelector's enumeration gap |
| `nervus.interface.operation.control@1` | ✅ | Querying/cancelling long tasks (caller) and reporting them (Provider) |
| `nervus.interface.safety.control@1` | ✅ | Safety observation and re-arm |
| `nervus.interface.power@1` | ✅ | Orderly reboot/shutdown |

**Change discipline**: whenever the kernel wires something up, update this table and the matching `.proto`'s
`[KERNEL: NOT IMPLEMENTED]` / `[KERNEL: NOT WIRED]` marker in the same PR. If the two are out of sync, it's as good as unmarked.

## Why all languages share one repository

Because NRCP §22.6 requires it:

> The Host, SDK, Provider and firmware implementations of the same round must also depend on **one already frozen schema and set of golden vectors**, and must not each guess.

Split into `nervus-ipc` / `nervus-ipc-go` / `nervus-ipc-jvm` and tied together with submodules,
"which schema commit Go generated from" and "which schema commit the JVM generated from" become two
**facts that can drift independently**, with nothing forcing them to point at the same commit. Drift shows up in a nasty way:
both sides compile and run, and only disagree on how some field behaves.

In one repository this holds by construction: one `buf generate` produces every language at once,
one commit holds the schema and every generated copy, and one CI `git diff --exit-code` covers them all.
It also gives the Go ↔ JVM golden vectors §10.12 requires a natural home (no repository in the split layout would be right for them).

> **There used to be Python.** In 2026-07, following a language decision that "group C (ROS adapter / perception / Agent) uses Python",
> `python/protocol` was generated along with a hand-written `python/sdk`. That decision was dropped:
> system services use Go. The whole `python/` directory has been removed. Keeping it wouldn't have cost disk space; it would have cost the drift surface of
> "three SDKs each implementing the same framing and handshake", and one more side of golden
> vectors to maintain on every schema change. Restoring it only needs the two `protoc_builtin` entries added back to `buf.gen.yaml` and a rerun.

If splitting ever becomes necessary, `git subtree split` can cut out the Go packages or `jvm/` with their history.
**That direction is reversible**; the other one (split first, merge later) is much harder.

## Layout

```text
nervus-ipc/
├── buf.yaml                    lint + breaking rules
├── buf.gen.yaml                generates Go + Java in one go
├── go.mod                      module github.com/nervus-os/nervus-ipc
├── proto/                      ← the source of truth, the bulk of the repository
│   ├── nervus/ipc/v1/
│   │   ├── envelope.proto            Envelope and every body (§10.4)
│   │   ├── status.proto              StatusCode, Success/Failure (§10.12)
│   │   ├── safety.proto              the five Safety boundary messages (§14.3)
│   │   ├── schema.proto              interface schema bundle distribution (§8.4)
│   │   ├── method_registry.proto     the method_meta option mechanism (extension number 60001)
│   │   ├── provider_descriptor.proto data-driven Provider contract (§7.2/§7.3)
│   │   └── transfer.proto            generic contract for the separate high-speed data plane
│   ├── nervus/interface/transfer/v1/     Transfer Control system interface
│   ├── nervus/interface/basemotion/v1/   standard interface BaseMotion@1 (robot dog locomotion)
│   ├── nervus/interface/manipulator/v1/  standard interface Manipulator@1 (robot arm)
│   ├── nervus/interface/camera/v1/       standard interfaces Camera@1 + CameraConfig@1
│   ├── nervus/interface/resourcedir/v1/  standard interface ResourceDirectory@1 (built into the kernel)
│   ├── nervus/interface/operation/v1/    OperationControl@1 (built into the kernel, long tasks)
│   └── com/acme/dog/v1/                  sample OEM private interface (extensibility acceptance)
├── protocol/                   generated Go types (committed; see below)
├── registry/                   dynamic schema bundle / Provider Descriptor checks
├── golden/                     the single construction source for the Go↔JVM golden vectors
├── sdk/                        Go Client / endpoint-scoped ServiceHost
├── jvm/
│   ├── settings.gradle.kts
│   ├── gradle/libs.versions.toml
│   └── protocol/
│       ├── build.gradle.kts
│       ├── src/main/java/      generated Java types (committed)
│       └── src/test/kotlin/    JVM-side golden vector assertions
└── testdata/golden/            *.binpb + vectors.json (generated by golden and committed)
```

**There's no `jvm/sdk`**: the hand-written JVM SDK is in the `nervus-app-sdk` repository. This repository's `jvm/` only holds generated code.

**No Kotlin DSL is generated**: the DSL builders `protobuf-kotlin` produces had zero uses in the whole codebase
(`nervus-app-sdk` and the golden tests all use Java's `.newBuilder()`), so they were removed from
`buf.gen.yaml`. To use them, add `protoc_builtin: kotlin` back; the wire format is unaffected.

### Go imports the whole IPC repository directly

The root directory is the Go module, at `github.com/nervus-os/nervus-ipc`. Consumers only need:

```sh
go get github.com/nervus-os/nervus-ipc@v0.1.0
```

then import `.../protocol/ipcv1`, `.../registry` or `.../sdk` as their role requires. Go only compiles the packages
actually imported; the repository's `proto/` and `jvm/` don't end up in Go binaries, and there's no need to
copy half the repository or maintain a second Go module.

The root module uses ordinary root tags (such as `v0.1.0`), not the old `go/v0.1.0` submodule tags.

### Provider permissions don't need kernel changes one by one

`ProviderDescriptor.permissions` can declare two kinds of permission: OEM private permissions must sit strictly under
`package_id.*`, and `perm.*` platform permissions can be distributed with platform-signed Providers. `registry` only checks
names, schemas and metadata shape, and doesn't treat a declaration as a grant; nervud must then use the verified trust /
signer role to decide whether the definer may occupy the platform namespace, and tighten declared values with the platform's risk floor.
So adding a standard camera, microphone or sensor permission doesn't need an entry in a kernel Go table, and an ordinary
Provider can't escalate by writing its own `perm.*`.

## Generic Transfer: separating the control plane from the high-speed data plane

The IPC repository defines no Camera- or Microphone-specific Envelopes. A capability interface's own `OpenStream`
is still an ordinary Request/Dispatch; only a method whose `MethodMeta.transfer` says so is entitled to create
a generic Transfer:

1. The Provider handler takes the current route from `CallContext.RouteID` and calls `nervus.interface.transfer.control@1`
   on **the same** `ServiceHost` connection.
2. nervud tightens direction, mode, packet size and rate according to the authoritative MethodMeta, the caller's permissions, the connection budget
   and the route lifetime, and returns two short-lived, single-use `TransferHandle`s, one for the Provider and one for the Caller.
3. Both ends use their handle to attach to the fixed Transfer UDS. The handshake is still length-prefixed protobuf, and after it succeeds
   it switches to `NVT1` frames. The Go SDK provides `AttachTransfer`, `TransferConn` and frame validation.
4. The baseline only implements `FRAMED_RELAY`. The memfd/eventfd ring ABI of `SHARED_MEMORY_RING`
   has to be frozen separately, and the SDK refuses it explicitly rather than guessing the format.

So adding a camera, microphone, radar or file stream later only adds its own capability schema and Provider
declaration; it adds no IPC body and doesn't change the IPC dispatcher.

## Generating

```sh
go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.11
buf lint
buf generate
```

- **Go** only needs `protoc-gen-go`, whose version is pinned by the `tool` directive in the root `go.mod`.
  The version in the `go install` above must match it.
- **Java** uses protoc's **built-in** generator (`protoc_builtin`), so it also needs the `protoc` binary itself
  on PATH.
- The `protobuf-java` runtime version must be **>=** the protoc version used to generate
  (from protoc v27 on, generated code checks the runtime version, and a lower one throws at class loading).
  Versions are managed centrally in `jvm/gradle/libs.versions.toml`.

### Generating the golden vectors

`testdata/golden/` isn't produced by `buf generate`; the Go side builds it:

```sh
go test ./golden/ -run TestGoldenUpdate -update
```

`golden/golden.go` is the **single construction source** of the vectors. If you change it, rerun this command and commit
the changes to `testdata/golden/`, or the Go and JVM assertions will both fail.

> The JVM's `JavaCompile` is pinned to UTF-8, so generated code carrying UTF-8 proto comments compiles with the same
> source encoding on Windows and Linux.

## Generated code is committed

`protocol/**/*.pb.go` and `jvm/protocol/src/main/**` are **committed to git** rather than generated at build time.
Three reasons:

1. **Consumers don't need the toolchain.** nervud, CI, developer machines and the RK3588 on the robot only need `go build`
   or `gradle build`. Done the other way round, go.mod can't express "generate, then compile",
   so `go build ./...` is no longer self-contained, and the Gradle side needs the right protoc version on every build machine.
2. **It makes "reproducible builds" verifiable.** §10.12 requires pinned protoc / runtime / plugin versions.
   Only with the output committed can CI regenerate it and run `git diff --exit-code`; a difference means
   someone's toolchain has drifted. Without committing, that requirement is only ever a slogan.
3. **The bytes reviewed are the bytes compiled into the binary.** For a TCB dependency, this reason is enough on its own.

The cost is repository noise and occasional merge conflicts. For a kernel dependency that trade is worth it.

## Compatibility rules (§10.12)

- Published field numbers **never** change meaning or type, and are never reused. On deletion, put the number and name in `reserved`; don't fill the gap.
- `method_id` and `event_id` stay stable once published, and are likewise kept unused after deletion.
- New fields must have safe default semantics; old receivers can ignore unknown fields.
- Every enum starts with `*_UNSPECIFIED = 0`; `OK` / `ACCEPTED` use non-zero numbers,
  and unspecified or unknown values always fail closed.
- Major incompatibilities bump `protocol_major`, and connections that can't be negotiated are refused.

`buf breaking` covers the first two, but **don't turn it on in CI before the schema is frozen**:
NRCP §22.6 explicitly allows breaking changes during the Rewrite v1 phase, and turning it on early only trains people to bypass it with `--force`.

## Namespaces

| use | value | notes |
|---|---|---|
| proto package | `nervus.ipc.v1` | |
| Go module | `github.com/nervus-os/nervus-ipc` | |
| Java package | `io.github.nervusos.ipc.v1` | **Hyphens are illegal in Java package names** (JLS §3.8), so the organisation name `nervus-os` loses its hyphen when reversed |
| Maven groupId | `io.github.nervus-os` | A groupId is a Maven coordinate, not a Java package name, so hyphens **are allowed** and it can match the organisation name exactly |

## Verification record

Run in full on Windows 11 (2026-07-26; previously also verified on WSL Debian). Toolchain versions:

| tool | version | notes |
|---|---|---|
| buf | 1.47.2 | prebuilt binary |
| protoc | 29.3 | aligned with protobuf-java 4.29.3 |
| protoc-gen-go | v1.36.11 | matches the `tool` directive in the root `go.mod` |
| Go | 1.26.0 | the module declares a 1.24.0 minimum |
| Gradle | 9.0.0 | pinned by the wrapper, including the distribution's sha256 |
| JDK | 17.0.3 | Gradle toolchain target 17 |

Verified (most recently after trimming down to Go + Java):

- [x] `buf lint` (STANDARD rule set) passes
- [x] `buf generate` produces 13 `.pb.go` files + 272 `.java` files
- [x] **Deterministic generation**: two consecutive `buf generate` runs give outputs with identical sha256.
      That's the precondition for the "regenerate in CI, then `git diff --exit-code`" gate
- [x] Go side: `go build ./... && go test ./...` all green (`golden` + `registry`)
- [x] **The golden vectors really run**: 18 `.binpb` files + `vectors.json` in `testdata/golden/`
      are generated and committed, covering TransferHandle / AttachTransferResult.
      Before this the test code existed, but the testdata had never been generated, so both sides were running against nothing
- [x] JVM side: `gradlew :protocol:test --rerun-tasks` succeeds on Windows
- [x] `.gitignore` works; `.gradle/` and `build/` don't leak into git status

Four real defects fixed during verification (none of them theoretical):

1. **`clean: true` with `out` pointing at the module root deletes `go.mod` and `go.sum`.**
   buf's clean deletes the whole directory, so `out` must point at a directory holding only generated code.
   The Go module now lives at the repository root, but the generation out is still narrowed to `protocol/`.
2. **`jvmToolchain(17)` fails outright on a machine with only JDK 21**, and no toolchain download source is configured
   by default. The foojay resolver has been added to `settings.gradle.kts`.
3. **No `repositories` were declared**, so not one dependency resolved. They're now declared centrally in `settings.gradle.kts` with
   `dependencyResolutionManagement`, and `FAIL_ON_PROJECT_REPOS` stops submodules adding their own sources.
4. **javac on Windows reads generated code as GBK by default**, so UTF-8 proto comments fail with
   `unmappable character`. `JavaCompile.options.encoding` is now pinned to UTF-8.

### To do

- [x] The Go SDK has landed (Client + endpoint-scoped ServiceHost); several
      endpoints on one connection are routed by `(endpoint_id, method_id)`, and `CancelDispatch` is supported
- [x] ~~Go ↔ JVM golden vectors~~ landed and in effect (18 vectors, both sides asserting the same `.binpb` files)
- [ ] CI: the `buf generate && git diff --exit-code` reproducibility gate
      (determinism has been verified by hand; the automation is what's missing)
- [ ] Kernel wiring: dynamic Provider Catalog / Method Registry / Transfer Manager,
      see the ⚠️ items in the "Implementation status" table above

**Under NRCP §25.2, these still need freezing separately before implementation**, so this repository currently only holds the parts that are certain:

- `ResourceSelector`'s final syntax, the public/private label directory, the `REQUIRE_UNIQUE` /
  `SYSTEM_PREFERRED` enums and the multi-candidate selection Policy (only `type` / `role` are fixed for now)
- `BaseMotion@1-experimental`'s final fields and `method_id`s
- `Operation`'s origin binding, and the final schema of typed progress / terminal results (`[v2+]`)

## Test requirements (end of §10.12)

At minimum, cover: partial packets, coalesced packets, zero length, the 128 KiB boundary, oversized lengths, malformed varints, deep/repeated fields,
duplicate request IDs, out-of-order responses, disconnects, slow reads and writes, permission revocation, Service restarts,
valid outcome/code/detail combinations, unknown reasons, malformed Service details,
and the Go ↔ JVM golden vectors.
