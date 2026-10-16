# Writing a Nervus system service

From nothing to a system service that `nervud` can start and apps can call.

## Four rules

1. **This guide uses Go.**
2. **Don't change `nervus-ipc`.** Adding a capability doesn't need protocol changes; declare it with
   ProviderArtifacts shipped in the package (section 5). IPC is just the wire; if an extension needs a new
   wire, you're heading the wrong way.
3. **Services can connect to each other over IPC** (Resolve + Request, the same route apps use to call services).
4. **Always proto IPC with the kernel; your own protocol for data with the other end.** The kernel proves who
   you are, whether you're allowed, and how big a pipe you can open; it doesn't look at what flows in the pipe
   (section 7).

Every "note" in this guide corresponds to a trap someone really fell into, not a precaution. The
[common failures](#common-failures) at the end are a lookup table by exit code and error message.

---

## 0. First, which kind are you writing?

On IPC a system service has only two roles, and the code looks very different:

| | **Provider** | **Client** |
|---|---|---|
| Does | Offers an interface | Calls others' interfaces |
| Key call | `RegisterEndpoint` | `ResolveEndpoint` |
| SDK type | `sdk.NewServiceHost` | `sdk.Connect` |
| Manifest field | `components[].exports` | `components[].interfaces` |
| **Needs a Provider contract** | **Yes** | No |
| Example here | `pkgmanagerd` | `safetyrecoveryd` |

A service can be both, but decide which is its main identity first: that decides how the manifest is
written and which admission rules the kernel checks you against.

`camerad` is the example of both at once: it offers two interfaces (capture + config) and also
`ResolveEndpoint`s the kernel's Transfer Control to build data-plane pipes. It also shows three things only
it does: **one package exporting several interfaces**, **resource declarations coming from a data file
rather than code constants**, and **publishing events**.

---

## 1. Create the directory

**Every directory at the root is one service, with its code directly inside.** The only exception is
`scripts/` (build tools).

```sh
mkdir myserviced
```

Naming rules:

- The directory name is also the **binary name**, the **Go package path** and the **build target**; the
  three can't be separated
- **It must not clash with an existing service**
- By convention it ends in `d` (daemon)

```text
myserviced/
├── main.go            process entry: connect, register, Serve
├── service.go         business implementation
├── providergen/       Provider contract generator (Providers only; see section 5)
│   └── main.go
├── manifest.json.in   package manifest template
└── README.md          description + deployment paths
```

### Services can't import each other, and the compiler guarantees it

**Every `.go` file in this repository is `package main`.** Go forbids importing `package main`, so service
A trying to import service B **fails to compile**.

That's not a convention, it's the language. When code needs sharing, put it in `nervus-ipc` (the home of the
protocol and SDK), and **don't** create a shared package here.

> `providergen/` is another `package main` in a subdirectory and can't be imported either.

---

## 2. Write manifest.json.in

This is the only thing the kernel knows you by. Typical symptoms of a wrong field are "the service starts
but can't connect" or "registration is refused", with an error that often points somewhere else.

```json
{
  "schema": 1,
  "package_id": "nervus.myservice",
  "label": "My Service",
  "version": "0.1.0",
  "version_code": 1,
  "min_nervus_api": 1,
  "target_nervus_api": 1,
  "supported_abis": ["linux-arm64"],
  "permissions": [
    "perm.service.register"
  ],
  "components": [
    {
      "id": "main",
      "type": "service",
      "runtime": "native",
      "entry": "bin/myserviced",
      "launch_mode": "always-on",
      "criticality": "optional",
      "exports": [
        { "interface": "nervus.interface.my.thing", "visibility": "public" }
      ],
      "limits": { "memory_max_mb": 128, "tasks_max": 64 }
    }
  ],
  "provider": {
    "descriptor": "provider.binpb",
    "schemas": "schemas.binpb"
  },
  "digests": {}
}
```

### Field by field

| Field | Notes |
|---|---|
| `package_id` | Reverse domain name; system services use the `nervus.` prefix. **It decides UID allocation and the data directory name**; change it and it's a different package |
| `version_code` | A monotonically increasing integer; upgrade continuity relies on it, not the `version` string |
| `supported_abis` | Only the three canonical tokens `linux-arm64` / `linux-armv7` / `linux-x86_64`. Not `arm64-v8a` or `aarch64`, and **no normalisation** (`build-image-tree.sh` rewrites it from its ABI argument) |
| `permissions` | **You can only get what you ask for**; see the warning below |
| `components[].id` | Must match the `componentID` constant in the code |
| `components[].entry` | A path relative to the package root. For native, an ELF inside the package |
| `launch_mode` | `always-on` starts with the kernel; `on-demand` starts when resolved; `manual` only starts explicitly. `type: "app"` can't be `always-on` |
| `criticality` | `optional` only restarts on a crash; `vital` escalates crashes to Safety. **Start with `optional`** |
| `exports` | For Providers. `visibility: "public"` can be resolved by any package, `"private"` only by packages with the same signer |
| `interfaces` | For Clients. Declares which interfaces you **consume** |
| `provider` | **Required whenever there are `exports`**; see section 5 |
| `digests` | Leave `{}`; `sysmanifest` fills it in at build time |

> ⚠️ **`permissions` must be declared explicitly**
>
> What's in the manifest is a **request**; the actual grant is the kernel's intersection of "source + trust +
> signing role + GrantMode" (`permission.Registry.IntersectAt`). Anything not requested is never granted, with
> the symptom `missing permission perm.xxx`.
>
> Requesting doesn't guarantee it either: `perm.service.register` needs OEM trust + a system image source;
> `perm.pkg.admin` also needs the `platform-release` signing role. **In a development build all of these are
> refused** unless `-dev-trust-system-packages` is on (see section 8).

> ⚠️ **`exports` and `provider` must appear together**
>
> For "`exports` but no `provider`" the kernel returns `ErrProviderArtifactsRequired`. There's no exception
> channel: the compatibility bridge that existed just for `nervus.pkgmanagerd` was removed.

---

## 3. Write main.go

### Two constants that must line up

```go
// Must match components[].id in manifest.json.
//
// It's the declared_component_id in the handshake: nervud checks it via the peer's PID → cgroup →
// systemd unit → start record. Getting it wrong can't let you impersonate anyone; it just means you can't
// connect, and the error is UNAUTHENTICATED, which looks like a permission problem.
const componentID = "main"

// Must match exports[].interface (or interfaces[]) in the manifest.
const interfaceID = "nervus.interface.my.thing"
```

### Provider skeleton

```go
host, err := sdk.NewServiceHost(sdk.Config{
    SockPath:    sockPath,
    ComponentID: componentID,
    Log:         log,
})
if err != nil {
    return fmt.Errorf("connect to the control plane: %w", err)  // fail and exit; don't retry yourself
}
defer func() { _ = host.Close() }()

// [Every handler must be registered before registering the endpoint]
registerHandlers(host, svc, log)

epID, err := host.RegisterEndpoint(ctx, sdk.RegisterRequest{
    InterfaceID: interfaceID,
    Major:       1,
    SchemaHash:  contractHash,  // must match what provider.binpb declares
})
```

> ⚠️ **Handlers must be registered before `RegisterEndpoint`**
>
> The moment registration succeeds nervud may forward a Dispatch. A method not registered yet gets
> `NOT_FOUND`, and the caller thinks **the method doesn't exist** rather than "the service isn't ready yet".

> ⚠️ **`SchemaHash` is now required and compared byte for byte**
>
> It must equal the value your `providergen` wrote into the descriptor. The compatibility bridge that let
> `nervus.pkgmanagerd` send an empty hash is gone; **every Provider is treated alike**. A mismatch is
> `FAILED_PRECONDITION`.

> ⚠️ **If you can't reach the control plane, exit; don't write a retry loop**
>
> systemd restarts with backoff, and nervud's supervision handles repeated crashes according to
> `criticality`. Retrying yourself hides the fact that "nervud isn't up" and makes the process look healthy.

An empty `RegisterRequest.ResourceHandle` = unspecified. The kernel only checks, when it's non-empty, that it's
a known handle in the Resource Registry. For an interface not bound to a physical resource, **filling one in
gets it refused with `INVALID_ARGUMENT`**.

### Client skeleton

```go
c, err := sdk.Connect(sdk.Config{ComponentID: componentID, Log: log})
if err != nil {
    return err
}
defer func() { _ = c.Close() }()

ep, err := c.ResolveEndpoint(ctx, sdk.ResolveRequest{
    InterfaceID: interfaceID, MinMajor: 1, MaxMajor: 1,
})
```

> ⚠️ **Resolve needs retries**
>
> Components are started by the service module **concurrently, with no start order guarantee**. The first
> Resolve may well land before the target service's `RegisterEndpoint`. Exiting on failure makes systemd
> restart you, and the log fills with `NOT_FOUND`s that look like a fault but are just a race.
>
> Retrying with backoff 10 to 20 times, 500 ms apart, is about right.

### Select devices by meaning, not by name

For interfaces bound to a physical resource, use labels in a `ResourceSelector` instead of hard-coding a role:

```go
ep, err := c.ResolveEndpoint(ctx, sdk.ResolveRequest{
    InterfaceID: "nervus.interface.camera", MinMajor: 1, MaxMajor: 1,
    Selector: &ipcv1.ResourceSelector{
        Type:   "nervus.resource.camera",
        Labels: map[string]string{"nervus.camera.facing": "front"},
        // no Policy = REQUIRE_UNIQUE: several matches is an error, not a random pick
    },
})
```

`stable_role` is **a product of the board config** (whether this board calls the front camera `cam.front` or
`camera0`); depend on it and a different board means changing code.

> ⚠️ **Several candidates fail closed by default**
>
> No `Policy` means `REQUIRE_UNIQUE`: several matches fail outright. To let the system pick for you, write
> `SYSTEM_PREFERRED` explicitly.
>
> That's deliberate: "I asked for a camera and the system gave me any old one" is far more dangerous than a
> clear error when the candidates mix front and rear (or left arm and right arm).

### Exiting gracefully

```go
// Withdraw the endpoint actively so nervud treats it as an expected event
// (sending EndpointDied{SERVICE_SHUTTING_DOWN} to callers holding a binding),
// rather than lumping it in with "the process crashed".
_ = host.UnregisterEndpoint(unregCtx, epID, true)
```

### Log to stderr

systemd collects it into the journal; no rotation to manage yourself.

```go
slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lv}))
```

---

## 4. Register it in the build script

Add a `<package_id>:<directory>` line to the `SERVICES` array in `scripts/build-image-tree.sh`:

```sh
SERVICES=(
	"nervus.pkgmanagerd:pkgmanagerd"
	"nervus.safety.recovery:safetyrecoveryd"
	"nervus.camerad:camerad"
	"nervus.myservice:myserviced"        # ← new
)
```

For services with a `providergen/` directory, the script runs it automatically before `sysmanifest`.

### When data files need to ship with the package

When a service carries configuration files (board parameters, calibration data), they must be copied into the
package directory **before** `providergen` (which reads them), since `sysmanifest` computes the `digests` after.
With the order wrong, the kernel refuses the whole package with "file not covered by digests", an error that
looks like a corrupt image.

`camerad`'s board JSON is the example (the `if [[ "$svc" == "camerad" ]]` block in the script).

**`providergen` and the service itself reading the same file** is deliberate: camerad's resource declarations
are generated from it, and at run time it's used again for the role → device mapping. With two files, "the
Catalog has cam.front" and "which device cam.front points at at run time" would become two facts that can
drift independently, with neither side reporting an error after drifting.

---

## 5. New interfaces: write a Provider contract, don't change the kernel

**If you only consume existing interfaces, skip this section.**

> **This section used to tell people to edit `nervud/internal/endpoint/catalog.go` and
> `permission/catalog.go`. Those files no longer exist**; the data-driven `internal/catalog` replaced them,
> and interfaces, permissions and resources all ship with packages, so adding a capability doesn't change
> the kernel.

### 5.1 The simplest shape: a metadata interface with zero protobuf messages

A capability interface's payload often isn't protobuf in the first place (camera frames, microphone samples
and radar point clouds all go through Transfer as opaque bytes). Such interfaces **need no `.proto` messages
at all**:

```go
// myserviced/providergen/main.go
methods := []*ipcv1.MethodMeta{
    {
        MethodId:           1,   // OpenStream
        RequiredPermission: "perm.my.thing",
        RiskClass:          ipcv1.RiskClass_RISK_CLASS_NORMAL,
        IsReadOnly:         true,
        // request_type / response_type left empty: no control-plane messages
        Transfer: &ipcv1.TransferPolicy{
            Direction:         ipcv1.TransferDirection_TRANSFER_DIRECTION_PROVIDER_TO_CALLER,
            MaxStreams:        1,
            MaxPacketBytes:    4 << 20,
            MaxBytesPerSecond: 64 << 20,
        },
    },
    {MethodId: 2, RequiredPermission: "perm.my.thing"},  // CloseStream
}

hash, _ := ipcregistry.MethodsHash(methods)
descriptor := &ipcv1.ProviderDescriptor{
    PackageId: "nervus.myservice",
    Interfaces: []*ipcv1.ProvidedInterface{{
        InterfaceId: interfaceID,
        InterfaceVersions: []*ipcv1.ProvidedInterfaceVersion{{
            Major: 1, SchemaHash: hash, Methods: methods,
        }},
        RequiredPermission: "perm.my.thing",
    }},
}
// An empty bundle set: no schema at all
descriptorWire, schemaWire, err := ipcregistry.MarshalProviderArtifacts(
    descriptor, &ipcv1.InterfaceSchemaBundleSet{})
```

The kernel only really needs to know three things: **who's calling, whether it's allowed, and how big a pipe
can be opened**. All three are in `MethodMeta`, regardless of message shape.

> ⚠️ **Metadata interfaces must not declare `request_type` / `response_type` / `error_detail_type`**
>
> There's no schema bundle to resolve them against, and packaging refuses it.

> ⚠️ **None of the three `Transfer` budget items may be 0**
>
> If any of `MaxStreams` / `MaxPacketBytes` / `MaxBytesPerSecond` is 0, nervud refuses with
> `method transfer policy is unbounded`, and the error appears at the first stream open, far from "the
> manifest is missing an item". Packaging already catches it.

### 5.2 When you need structured messages: a schema bundle

Only write a `.proto` when the control plane really carries structured data, and build the bundle from the
generated method enum with `BuildSchemaBundle` (`pkgmanagerd/providergen/main.go` is the example).

**To publish events, use `BuildSchemaBundleWithEvents`** and pass an event enum as well
(`camerad/providergen/main.go` is the example):

```go
bundle, err := ipcregistry.BuildSchemaBundleWithEvents(
    interfaceID, 1,
    myv1.MyMethod(0).Descriptor(),
    myv1.MyEvent(0).Descriptor())
```

> ⚠️ **Events with payloads must go through the bundle, not inline in the descriptor**
>
> `ProvidedInterfaceVersion.events` is for **metadata interfaces**, and it doesn't allow `payload_type`:
> without a FileDescriptorSet there's no way to check that the message the type name points at exists. An
> interface with a schema is the opposite: its events almost always have payloads.
>
> Writing both is refused by the registry as "the same interface both inline and with a bundle".
>
> The event enum **must be in the same file as the method enum**, or its transitive dependencies aren't in the
> bundle's descriptor set. It doesn't affect `schema_hash` (it's in the same descriptor set anyway), so adding
> an event enum to an existing interface **doesn't break registration of Providers already installed**.

> ⚠️ **A missing `delivery_class` fails closed to RELIABLE**
>
> That's the strictest tier (no drops at all). A telemetry stream as RELIABLE gets its subscription cut by the
> kernel whenever the consumer is a bit slow. For "only the latest value" write `STATE`; for "if it's lost, it's
> lost" write `LOSSY`. The two mean opposite things, and leaving it out gets you a third.

**Take method ID constants from the generated code; don't copy them locally**. A copy doesn't just duplicate,
it quietly goes stale:

```go
methodInstall = uint32(pkgv1.PackageManagerMethod_PACKAGE_MANAGER_METHOD_INSTALL)  // right
const methodInstall = 1                                                            // wrong
```

### 5.3 Permissions and namespaces

| You want to define | Rule |
|---|---|
| `perm.*` platform permissions | Only packages signed `platform-release` may define them |
| Private permissions | Must be under `<package_id>.*` |
| `nervus.interface.*` standard interfaces | Only `platform-release` may **define**; `oem-service` may **implement** |
| Private interfaces | Must be under `<package_id>.*` |
| `nervus.resource.*` standard resource types | Only `platform-release` may define; OEMs can only bind existing ones |
| Resource label keys | Same: platform labels `nervus.*` only from platform-release; private labels in your own namespace |

> ⚠️ **Several Providers implementing the same standard interface must match byte for byte**
>
> `sameInterfaceContract` compares method_id, required_permission, risk_class and the Transfer budget. If any
> differs, the second Provider is refused with `interface ... conflicts with definition owned by ...`.
>
> That's exactly what makes vendors interchangeable: whichever one an App resolves to, the semantics are the same.

### 5.4 All three repositories must point at the same ipc commit

`nervud`, `nervus-system-server` and `nervus-ipc` **must point at the same commit**, or the two sides quietly
diverge in how they read the same Envelope while both compile and run. Sync with each repository's
`scripts/sync-ipc.sh`.

---

## 6. The shared file area between services

Request `perm.storage.shared` to get two directories (**created on demand; no request, no directories**):

```text
/run/nervus/shared/<package_id>/      tmpfs, gone on reboot. Run-time exchange
/var/lib/nervus/shared/<package_id>/  disk, persistent. Config / models / caches
```

Owner = your UID, mode 0755: **your own directory is writable, others' are readable**. The kernel creates the
directories, and the root is writable only by nervud, so no package can squat another's directory name.

### Want to talk to another service in your own protocol? Put the UDS here

```go
sock := filepath.Join("/run/nervus/shared", myPackageID, "control.sock")
ln, err := net.Listen("unix", sock)
```

The directory is 0755, so other services can see the path and connect. **That's the most direct way to do
"services talk in their own protocol"**: the kernel isn't involved and doesn't parse what you say.

UDS suits control and small messages; heavy traffic uses Transfer (section 7).

> ⚠️ **Sensitive data can't go here**
>
> The shared area is **readable system-wide** (read isolation across packages relies only on the 0700 data
> directories, and the shared area is 0755). Only put things here that "anyone with `perm.storage.shared` may see".
>
> Data that needs a real permission threshold goes through Transfer's memory handles: the handle is itself the
> credential, with no filesystem path to get around it. Writing camera frames into the shared area would let any
> app bypass `perm.camera.capture`.

> ⚠️ **Don't stream video through files on disk**
>
> Raw 1080p30 is about 93 MiB/s, and writing that continuously to eMMC burns through the flash's life. Heavy
> run-time traffic either goes through Transfer or sits in the tmpfs root.

---

## 7. High-throughput data: Transfer

The control-plane Envelope doesn't carry large data. The flow:

1. The method declares a budget in `MethodMeta.transfer` (section 5.1)
2. The Provider handler takes the current route from `CallContext.RouteID` and calls
   `nervus.interface.transfer.control@1` on the **same** `ServiceHost` connection
3. nervud tightens direction, mode, packet size and rate according to the authoritative metadata, caller
   permissions, connection budget and route lifetime, and returns two short-lived one-time `TransferHandle`s
   (Provider and Caller)
4. Both ends attach with `sdk.AttachTransfer` to the kernel's own Transfer socket

**Once attached, what flows in the pipe is up to the two ends**: the kernel only frames and rate-limits, without
parsing content. That's where "your own protocol for data with the other end" lands.

---

## 7.5 Long tasks: Operations

When one call takes 10 seconds (an arm trajectory, homing, navigating to a point), an ordinary
`Request/Response` isn't enough.

### First decide whether you really need it

An ordinary call's implicit contract is **response arrived = it's done**. For things that finish instantly, like
setting a speed or reading state, that's plenty; don't wrap them in an Operation.

| You want the caller to know | Use |
|---|---|
| What the device is like now (where, how fast) | An ordinary read-only method + state events |
| Whether my call has finished, and whether it worked | An Operation |

They complement each other rather than replace each other. In `basemotion.proto`, `GetMotionState` and
trajectory Operations exist side by side.

**If a capability has only one controller, actions never need cancelling and failure reasons don't matter**,
state methods are enough. Operations are for "several controllers + needs cancelling + needs a reliable failure
reason".

Why those three go together: A starts a 10-second trajectory; at second 3 an operator (a human, higher priority)
pre-empts the lease and starts another motion. With only state methods A sees `MOVING → MOVING → READY`; it
can't tell that its own call failed, and may even think it succeeded.

### Sequence

```
App --Request(a method declaring returns_operation)--> nervud
                nervud creates the Operation (PENDING); operation_id goes into the ExecutionContext
                nervud --Dispatch--> your service
                nervud <--DispatchResult{ACCEPTED}-- your service
App <--Response{ACCEPTED, OperationHandle}-- nervud
                nervud <--ReportProgress / CompleteOperation-- your service
App <--OperationChanged (subscribed event)-- nervud
```

**The Operation exists before the Dispatch**, so when you receive the Dispatch, `ExecutionContext.operation_id`
is already valid and you can reply `ACCEPTED` straight away.

### What the provider side does

1. The method's `MethodMeta` declares `returns_operation: true`
2. The handler takes the request and **returns at once** (replying `ACCEPTED` with an empty `payload`), handing the
   work to its own goroutine
3. When really starting, call `AcceptOperation` (PENDING → RUNNING)
4. Along the way, call `ReportProgress`
5. At the end, call `CompleteOperation`

All three report methods are on `nervus.interface.operation.control@1`; resolve it once, the same shape as
Transfer Control.

> ⚠️ **`Success.payload` must stay empty**
>
> nervud allocates the `operation_id` and owns the state machine. Write your own handle and the caller gets a
> number nervud doesn't know: cancelling never cancels and progress never arrives, **and neither side reports an
> error**. The kernel refuses an ACCEPTED with a payload.

> ⚠️ **`AcceptOperation` must bring back the `motion_epoch`**
>
> Hundreds of milliseconds of preparation may pass between receiving the Dispatch and really starting to move, and
> that's enough for a Safety stop to happen. Without checking it, you'd start moving on an authorisation that's
> already void.

> ⚠️ **The final state is written once**
>
> A repeated `CompleteOperation` returns `FAILED_PRECONDITION` + `ALREADY_TERMINAL`. Idempotently accepting the
> second one would hide a real class of fault: two paths in your service both believe they should finish this run,
> and they may reach opposite conclusions.

> NervusOS Cambridge note: the β kernel originally refused operations bound to no resource, which made
> `pkg_manager` INSTALL unreachable. It now accepts resource-free operations when there's no motion lease; motion
> operations must still name the resource their lease covers.

### What the consumer side does

The `Response` payload is an `OperationHandle`. With the `operation_id`:

- **Subscribe** to `OperationChanged` to watch it (the normal path)
- Call `GetOperation` once (covering two windows: the short gap between getting the handle and subscribing, and
  "it was still running when I left" after a reconnect)
- `CancelOperation` to ask for cancellation

> ⚠️ **The subscription must carry `scope = operation_id`**
>
> This interface has only one built-in endpoint, and every operation on the machine emits events from it. Without a
> scope it's refused: nervud decides visibility at Subscribe time, and subscribing to one that isn't yours fails at
> once. It's an instance of the general mechanism in the next section, except that the kernel knows who owns an
> operation itself and you don't have to register it.

> ⚠️ **`CancelOperation` is a request, not a command**
>
> Success only means nervud set the state to `CANCEL_REQUESTED` and told your service. An arm takes time to stop
> physically, and the real final state (`CANCELLED`, or "too late, it already `SUCCEEDED`") comes in later events. A
> synchronous "cancel finished" would be a lie.

### It isn't a "fast lane"

Operations have nothing to do with speed or priority, and aren't a dedicated line. Everything goes over the same
UDS. What really handles "fast" is three other things:

| Mechanism | Handles |
|---|---|
| Safety Lane | Emergency stops. A separate channel, no queueing |
| Transfer data plane | Large data. Camera frames never enter the control plane (section 7) |
| ControlLease + pre-emption | Who may move the machine |

---

## 7.6 Many instances on one endpoint: instance-scoped events

One endpoint often manages several things at once: four streams open on one camera, several trajectories on one
arm. All their events come out of **the same endpoint**.

Without handling it, an App subscribed to `StreamStateChanged` gets the state of **every** stream on the endpoint,
including streams other Apps opened. It gets resolutions, frame rates and fault times, enough to piece together
"what another app is doing with the front camera".

### Three steps

**1. Declare the event `scoped: true`**

```protobuf
option (nervus.ipc.v1.event_meta) = {
  event_id: 1
  delivery_class: DELIVERY_CLASS_STATE
  scoped: true            // delivered per instance, not broadcast
};
```

Once declared, a `Subscribe` without a scope is refused by nervud, and one with a scope on an undeclared event is
refused too. **Both directions fail closed**: silently ignoring the scope would be the truly dangerous one, since
the caller would think it's watching one stream while receiving all of them, the events would look completely
normal, and it would never notice.

**2. The Provider registers ownership when it creates an instance**

```go
// openStream: Commit first to get the stream_id, then register who owns this stream
if err := c.ep.BindEventScope(streamID, cc.RouteID); err != nil {
    c.log.Warn("registering event ownership failed; this stream's state events can't be subscribed to", "err", err)
}
```

`cc.RouteID` is the current route from the `CallContext`, the same thing Transfer uses. nervud looks up the caller
from it and records `(you, endpoint, scope) -> caller`.

You choose the `scope` value, as long as it's unique **within this endpoint**. A handle you were going to give the
caller anyway, like a stream_id, is the natural choice: the caller doesn't need to learn another numbering.

**3. Withdraw it when the instance closes**

```go
// [Send the final event first, then withdraw ownership]
c.publishState(streamID, camerav1.StreamPhase_STREAM_PHASE_STOPPED)
c.releaseScope(streamID)   // -> c.ep.ReleaseEventScope(streamID)
```

The other way round, subscribers wouldn't get STOPPED, which is the one they need most.

> ⚠️ **Withdraw on the error path too**
>
> A capture thread ending directly on FAULT, a service closing streams in bulk on a stop signal: all of these paths
> must reach `ReleaseEventScope`. nervud cleans up when the connection drops or the endpoint is withdrawn, but that
> covers crashes; opening and closing streams repeatedly at run time without withdrawing makes the ownership table
> grow without bound.

### Consumer side

```go
sub, err := host.Subscribe(ctx, sdk.SubscribeRequest{
    EndpointID: epID,
    EventID:    1,
    Scope:      streamID,     // the stream I opened
})
```

Subscribing to an instance that isn't yours returns `NOT_FOUND`, **not** `PERMISSION_DENIED`: the latter would tell
the caller "this instance exists, it's just not yours", which is information in itself.

### The kernel only knows pointers; it doesn't parse your protocol

nervud doesn't know what a `stream_id` is and never decodes `OpenStreamResponse`. It only keeps a
`(Provider connection, endpoint, scope) -> owner connection` table and checks it on `Subscribe`. So:

- **Ownership is per connection.** When the caller disconnects, its instance registrations go with it; after
  reconnecting it has to open the stream again.
- **A registration that never arrived = can't subscribe.** `BindEventScope` is one-way, and a failure only shows up
  as later subscriptions returning `NOT_FOUND`. So either register before returning the handle, or tell the caller
  about the failure; don't leave it holding a stream_id that will never receive events.
- **Registering again simply overwrites**, without an error. Closing and reopening with the same number is your
  business.

---

## 8. Build, sign, deploy

```sh
scripts/build-image-tree.sh ./out signing/platform-release.pem linux-arm64
```

This does five things: cross-compile (`CGO_ENABLED=0`, statically linked) → **produce the Provider contract** →
compute digests → fill in the manifest → Ed25519 signature.

Output:

```text
out/nervus.myservice/
├── bin/myserviced      static binary
├── provider.binpb      Provider contract
├── schemas.binpb       schema (an empty set for metadata interfaces)
├── manifest.json       digests filled in
└── manifest.sig        platform-release role signature
```

### Deployment path

```sh
cp -r ./out/* <image mount point>/usr/lib/nervus/system-packages/
```

**The directory name must equal `package_id`.** The kernel's boot scan globs
`/usr/lib/nervus/system-packages/*/manifest.json`, with **no version subdirectory**: system packages follow
whole-image OTA, and several versions never coexist.

> ⚠️ **System image packages and dynamically installed packages are two separate paths**
>
> | | System image package | Dynamically installed package |
> |---|---|---|
> | Location | `/usr/lib/nervus/system-packages/<pkg>/` | `/var/lib/nervus/packages/<pkg>/<version>/` |
> | Form | **Unpacked directory tree** | `.nspkg` (zstd + tar) |
> | Signing role | `platform-release` | `developer` (self-anchored) |
> | Device access | Can touch `/dev` | Never |

### Development builds need `-dev-trust-system-packages`

A development binary has no embedded platform root, so `LoadTrustStore` fails and every system package fails
closed to `Ordinary`, while `perm.service.register` needs OEM. **Without this switch your service can't register.**

```sh
systemd-run --unit=nervud --collect \
  --property=StandardOutput=append:/tmp/nervud.log \
  --property=StandardError=append:/tmp/nervud.log \
  ./nervud -dev-skip-preflight -dev-allow-sched-degrade \
           -dev-trust-system-packages -log-level debug
```

It only relaxes "is this key authorised by the platform root". Signature checks, key_ids and digests all still
apply: a modified binary, a modified manifest, or an attempt to impersonate some key_id are all still refused.

> ⚠️ **It must run as a systemd unit, not as a bare binary**
>
> Components' transient units have `BindsTo=nervud.service`. Run bare and that unit doesn't exist, so
> `StartTransientUnit` fails with `Unit nervud.service not found`.

---

## 9. What the kernel does for you

Before your service starts, the kernel provisions the run-time prerequisites in the **boot scan** (`pkgregistry`):

| Step | Result | Without it |
|---|---|---|
| Assign a stable UID | 20000–59999, **never reused** | — |
| `EnsureAppUser` | `nervus-app-<uid>` in `/etc/passwd` | `217/USER` |
| Private data directory | `/var/lib/nervus/package-data/<pkg>`, 0700 | `226/NAMESPACE` |
| Shared area subdirectories | `/run/nervus/shared/<pkg>` and the disk one, 0755 | `ENOENT` on write |
| Permission decision | `GrantedPermissions` | `missing permission` |

Then it starts a transient unit through `authority` → systemd:

```
nervus-<package_id>-<component_id>.service
```

The sandbox always includes `NoNewPrivileges`, `ProtectSystem=strict`, `SystemCallFilter=@system-service` and
`BindsTo=nervud.service` (when nervud dies, you stop).

**Only your own data directory (plus your own subdirectory of the shared area) is writable.** Anywhere else, even
with the right owner and mode, writes fail with `read-only file system`: **permissions and mounts are two
independent gates**.

### One bad package doesn't take start-up down

Catalog conflicts used to fail the whole kernel start-up. Now the boot scan **quarantines the offending package**,
audits it loudly and carries on. When your service is quarantined, the journal has
`pkgregistry: package quarantined at boot`.

---

## 10. Debugging

```sh
# your service
journalctl -u nervus-nervus.myservice-main.service -n 50
systemctl show nervus-nervus.myservice-main.service --property=ActiveState,SubState

# how the kernel sees you
journalctl -u nervud -b | grep -E "handshake complete|RegisterEndpoint|ResolveEndpoint|provision|quarantined"
nervusctl list
```

---

## Common failures

Look up by the **symptom** you see.

### systemd exit codes

| Symptom | Cause | Fix |
|---|---|---|
| `217/USER` | The UID isn't in `/etc/passwd`, so NSS can't resolve it | The kernel's `provision.go` should create it. If it didn't, the boot scan never got there |
| `226/NAMESPACE` | The private data directory doesn't exist, and `WorkingDirectory` points at it | Same as above |
| `203/EXEC` | Executable not found | Check the `entry` path; check the code root (system packages are in `/usr/lib/nervus/system-packages/`, **with no version subdirectory**) |
| `Unit ... was already loaded` | The previous transient unit wasn't collected | Clear it by hand: `systemctl reset-failed <unit>` |

### Loading and contracts

| Symptom | Cause |
|---|---|
| `ErrProviderArtifactsRequired` | The manifest has `exports` but no `provider` section. **No exception channel** |
| `provider.descriptor isn't covered by digests` | `providergen` didn't run before `sysmanifest` |
| `interface ... conflicts with definition owned by ...` | Inconsistent with an existing Provider's contract (any of method_id / permission / risk / Transfer budget) |
| `package quarantined at boot` | Your package was refused at the Catalog layer and the machine started without it. See the `err` on the same line |
| `declares inline methods and a schema bundle` | The same interface version both inlines `methods` and carries a bundle: two contracts |

### Handshake and registration

| Symptom | Cause |
|---|---|
| `UNAUTHENTICATED` | `componentID` doesn't match the manifest's `components[].id`. **Not a permission problem** |
| `interface not declared in manifest exports` | `interfaceID` doesn't match the manifest's `exports[].interface` |
| `missing permission perm.service.register` | Not requested in the manifest, **or** a development build without `-dev-trust-system-packages` |
| `interface schema hash does not match the catalog` | `RegisterEndpoint`'s `SchemaHash` differs from the descriptor's. The empty-hash bridge is gone |
| `RegisterEndpoint` returns `INVALID_ARGUMENT` | A `ResourceHandle` was given that isn't in the Resource Registry. If you bind no resource, **leave it empty** |

### Resolving and calling

| Symptom | Cause |
|---|---|
| Resolve `NOT_FOUND` | The target hasn't called `RegisterEndpoint` yet (a race) → **retry**; or it never started → check its journal |
| Resolve `PERMISSION_DENIED` | Missing the interface's permission |
| Resolve `FAILED_PRECONDITION` | Version negotiation failed; or the selector matched 0; or **it matched several and the policy is `REQUIRE_UNIQUE`** (the default) |
| A call returns `NOT_FOUND` although the method exists | Handlers were registered after `RegisterEndpoint` |
| `method transfer policy is unbounded` | One of the three `MethodMeta.transfer` budget items is 0 |

### Filesystem

| Symptom | Cause |
|---|---|
| `permission denied` writing outside your data directory | The sandbox only makes the data directory + your shared-area subdirectory writable. **By design** |
| `read-only file system` although owner and mode are right | `ProtectSystem=strict`. Ownership is one gate and mounts another; **you must pass both** |
| `ENOENT` writing to the shared area | `perm.storage.shared` wasn't requested, so the subdirectory wasn't created |

### Signing and installation

| Symptom | Cause |
|---|---|
| `signer key not trusted for its role` | Signed with a `platform` role but no platform root embedded → for development builds turn on `-dev-trust-system-packages` |
| `system package signature not verified; downgrading to Ordinary` | Same as above. After the downgrade `perm.service.register` is refused |
| `digest mismatch` | The binary or provider contract changed without rerunning `sysmanifest`. **Changed means re-signed** |

---

## Checklist

Go through it before a new service goes live:

- [ ] The directory name doesn't clash with an existing service
- [ ] The `componentID` constant == the manifest's `components[].id`
- [ ] The `interfaceID` constant == the manifest's `exports[].interface` / `interfaces[]`
- [ ] Provider: there's a `providergen/`, and the manifest has a `provider` section
- [ ] Provider: `RegisterEndpoint`'s `SchemaHash` == what the descriptor declares
- [ ] All three `MethodMeta.transfer` budget items are non-zero (for methods declaring transfer)
- [ ] For one endpoint with many instances: events are `scoped: true`, and every normal and error close path
      reaches `ReleaseEventScope`
- [ ] The needed permissions are requested under `permissions`
- [ ] Handlers are all registered **before** `RegisterEndpoint`
- [ ] The Client's Resolve retries
- [ ] Interfaces bound to resources select devices by labels, not hard-coded roles
- [ ] `UnregisterEndpoint` on exit
- [ ] Added to `SERVICES` in `scripts/build-image-tree.sh`
- [ ] All three repositories point at the same nervus-ipc commit
- [ ] The README has the deployment path
- [ ] `go vet ./... && go test ./... && go test ./... -race`

---

## Further reading

| To find out | See |
|---|---|
| Protocol fields and the Envelope structure | `nervus-ipc/README.md`, `proto/nervus/ipc/v1/envelope.proto` |
| Which bodies the kernel really supports | The "implementation status" table in `nervus-ipc/README.md` |
| Known kernel gaps | "Known gaps" in `nervud/README.md` |
| How interfaces/permissions/resources get into the Catalog | `nervud/internal/catalog/builder.go` |
| The admission chain for endpoint registration | `nervud/internal/endpoint/register.go` |
| The full set of sandbox properties | `nervud/internal/authority/systemd/props.go` |
| The package install transaction | `nervud/internal/pkgregistry/install.go` |
