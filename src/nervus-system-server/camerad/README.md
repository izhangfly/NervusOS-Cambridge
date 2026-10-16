# nervus.camerad

Nervus OS's general camera service. It turns "a pile of `/dev/videoN`" into "which cameras this board
has, and which way each one faces".

On the Ubuntu laptop in NervusOS-Cambridge, the laptop's built-in webcam lands in the USB pool as
`cam.usb.0` (the reference board config declares no fixed cameras that exist there).

## The problem it solves

V4L2 only answers "what formats can this node produce". It can't answer two things:

- **Which one faces forward**: that's a board fact, not something the driver knows
- **Is it still the same one after a reboot**: `videoN` numbers depend on driver load order

This service answers the first with a **board JSON** and the second with **USB topology paths**.

## One camera = two resources

```
nervus.resource.camera          SHARED_OBSERVE      capture, shared by many
nervus.resource.camera.config   EXCLUSIVE_CONTROL   configuration, exclusive lease
```

They're split because their semantics are opposite: navigation, recording and AI detection can all
watch the same stream, while with two processes changing exposure at once, who wins depends on timing
and the picture flips between the two values.

Kernel leases are issued per resource and one endpoint binds one resource, so it has to be two
resources, two interfaces and two endpoints. The benefit is direct: **an App that only watches never
needs a lease**, and so is never refused because someone else is adjusting settings.

The config side's `stable_role` is the capture role plus a `.config` suffix: the Catalog's handle
index is globally unique across resource types, and a shared name would collide on a handle during setup.

## Board configuration

`boards/<board>.json`, chosen at build time with `NERVUS_BOARD` (default `reference`).

```json
{
  "schema": 1,
  "board": "acme-quadruped-v2",
  "cameras": [
    {
      "role": "cam.front",
      "match": {"usb_path": "1-1.1"},
      "labels": {
        "nervus.camera.facing": "front",
        "nervus.camera.kind": "color",
        "nervus.camera.mount": "head"
      }
    },
    {
      "role": "cam.depth",
      "match": {"device_path_link": "platform-csi0-video-index0"},
      "labels": {"nervus.camera.kind": "depth"}
    }
  ],
  "usb_pool": {"role_prefix": "cam.usb", "max_slots": 2}
}
```

### The file is read twice, deliberately

| When | Who reads it | What for |
|---|---|---|
| Build time | `providergen` | Generates the resource declarations in `ProviderArtifacts` |
| Run time | `camerad` | Maps roles to real device nodes |

**It must be the same file.** With two, "the Catalog has cam.front" and "which device cam.front points
at at run time" would become two facts that can drift independently, and after drifting neither side
reports an error; an App asking for the front camera just gets an endpoint that doesn't exist.

### Why the resource set is fixed at build time

`ProviderArtifacts` go into the manifest's `digests` and are signed with the image. If resources were
read fresh from JSON at run time, **editing one line of JSON would add yourself a "front camera"**;
something the signature doesn't cover isn't a contract.

The cost is repackaging for a new board. That's right: the board config is part of the image anyway.

### Locating devices: by position, not by number or serial

`usb_path` (such as `1-1.2`) describes **which physical port it's plugged into**, which doesn't change
unless it's rewired.

- Not `/dev/videoN`: the number changes with driver load order. Mapping by it means "after one reboot,
  front became rear", and no log mentions it
- Not the serial number: replace a camera with one of the same model to repair the machine and the
  serial changes, losing the role. A board config describes **positions**, not a specific module

MIPI/CSI cameras have no USB topology path; use the stable link name under `/dev/v4l/by-path/`
(`device_path_link`).

### Label allowlist

Platform semantic labels are limited to three keys, and their values are allowlisted too:

| Key | Values |
|---|---|
| `nervus.camera.facing` | front / rear / left / right / up / down |
| `nervus.camera.kind` | color / depth / ir / fisheye |
| `nervus.camera.mount` | free string |

Private labels must start with `nervus.camerad.`.

A misspelt key (`nervus.camera.face`) or value (`Front`) **fails at build time**. Otherwise, at run
time it would look like "this camera can never be selected", with nothing pointing at the typo.

### The USB cold-plug pool

`usb_pool` gives temporarily plugged-in USB cameras somewhere to land. Slots are assigned once, sorted
by USB topology path, **at the moment the service starts**.

Sorting compares segments **numerically**, not lexicographically: lexicographic order would put
`1-10` before `1-2`, so on a machine with 10 ports in use slot order wouldn't match physical order,
and only once the device count passes 10.

**It isn't stable when the set of devices changes**: unplug the camera on `1-1.2` and the one on
`1-1.3` moves from `usb.1` to `usb.0`. That's not a defect, it's the nature of "the Nth one". Devices
that need absolute stability must be listed under `cameras` with an exact `usb_path`.

**Pool slots have no semantic labels.** Nobody knows which way a camera plugged in on the fly faces,
and giving it some `facing` would let Apps selecting by meaning pick a device facing who knows where,
which is worse than finding nothing.

### Cold plug only

Cameras plugged in while running don't appear automatically. Hot plug would need "a new device
appears → an endpoint is registered dynamically → a resource appears out of nowhere in the Catalog",
but the resource set is signed at build time, and adding resources at run time would bypass the
signature. Supporting it first needs an answer to "how do dynamic resources enter the signed
contract", which is bigger than one camera capability.

## The data plane doesn't go through nervud

```
App --Request(OpenStream)--> nervud --Dispatch--> camerad
camerad --BeginTransfer--> nervud                    build the pipe
camerad --AttachTransfer--> nervud-transfer.sock     attach the producer end
camerad --CommitTransfer--> nervud                   release the consumer end
camerad --OpenStreamResponse(caller handle)--> App   hand over the credential
App --AttachTransfer--> nervud-transfer.sock         attach the consumer end
                ↓
    after that frames flow in the shared-memory ring; nervud isn't on the data path
```

The order can't change: the caller ticket can't attach before Commit, and Commit must come after the
business response is certain to be returned. Committing first and then failing would leave the App
without a handle while the Transfer was already released, a dangling pipe nobody can close.

**There are no per-frame events on the control plane.** The ring has its own eventfd wake-up, and also
pushing "new frame" would move once-per-frame traffic back onto the control plane, the very thing the
shared-memory data plane exists to avoid. There are only two control-plane events, neither per frame:

| Event | Class | Meaning |
|---|---|---|
| `StreamStateChanged` | STATE | The stream opened / stopped / broke. Coalescing intermediate states loses nothing |
| `DeviceError` | RELIABLE | Dropped off, buffer overrun, driver error. Can't be coalesced or dropped |

## No nearest-match formats

Asking for 1920×1080 when the device only has 1280×720 returns `FAILED_PRECONDITION` +
`FORMAT_NOT_SUPPORTED`; **it never quietly gives you something smaller**. A silent downgrade would make
the App parse shared memory with the wrong dimensions, and the torn picture would be untraceable.

`S_FMT` is a negotiation, not a setting: the driver changes an unsupported request to the nearest
supported value **without an error**. So the format is read back after setting, and a mismatch fails.

## How vendors plug in

Vendors **don't implement the standard interface**; they implement a private interface in their own
namespace (something like `vendor.acme.camerad.interface.source@1`), which this service turns into the
standard one.

That way the standard semantics (`facing=front`) come from the platform's board config, and vendors
can't award themselves any: the kernel's namespace rules refuse a package signed `oem-service` that
declares `nervus.*` labels or interfaces. A vendor provides **a picture**, not "which way this picture faces".

The data plane still goes direct: camerad hands the vendor's `TransferHandle` to the App, and frames are
never copied through this process.

**That path isn't wired up yet**; only V4L2 devices are supported today. Hooking up a vendor source needs a
real vendor Provider to define the shape of the private contract.

## Board integration

```sh
camerad -profile boards/acme.json -dry-run
```

Prints the role → device mapping and exits without connecting to the control plane. Roles that are
declared but not plugged in get their own WARN: the most common problem during board integration is
"the config says front camera but the cable isn't plugged in", and the log then just has **one line missing**.

## Layout

```
camerad/
├── boardprofile/     board JSON parsing and validation (shared by build time and run time)
├── boards/           board configs, one per board
├── providergen/      build time: board JSON → ProviderArtifacts
├── contract.go       interface constants and schema hash
├── discover.go       device discovery, fixed-role matching, USB pool assignment
├── v4l2_linux.go     V4L2 compatibility layer (ioctl numbers computed from unsafe.Sizeof)
├── stream.go         stream lifecycle and frame pump
├── config.go         config endpoint (controls)
├── transfer.go       wrapper for the three Transfer Control steps
├── events.go         control-plane event publishing
├── errors.go         failures with a typed detail
├── pixfmt.go         V4L2 fourcc ↔ platform PixelFormat
└── main.go           wiring
```
