# Onboarding the Limo Pro: the **NervusOS (L2 / nervud)** layer
### For me, on the Nervus kernel side

> **Summary:** the kernel **does not drive hardware**. It does four things: it **registers** the Limo, **binds**
> its capability to a provider (DimOS), **enforces** permissions, and **arbitrates** who holds the body. "Swap the
> robot, swap the provider, keep the kernel": adding the Limo should be **configuration, not kernel code**. The
> Limo is a **mobile base**, so it uses the standard interface **BaseMotion@1** (the same one as the GO2 dogs),
> resource `nervus.resource.motion.base/main`.

Kernel repo: `github.com/Nervus-OS/nervud` (Go). **Clone it and read
`internal/pkgregistry/manifest.go` + the Robot Profile loader before editing** — the schema
below is the intended shape; confirm field names against source.

---

## The 4 things to add

### 1. Robot Profile entry — declare the Limo + bind its capability to the DimOS provider
The Robot Profile is the declarative manifest: *devices → capabilities → which provider serves
each* (Port B). The Limo is a **mobile base**, so it provides the **BaseMotion** capability on
resource `nervus.resource.motion.base/main`, served by the DimOS **limo** provider.

Template (confirm exact schema against nervud):
```yaml
# robot-profile.d/limo-pro.yaml   (or however nervud loads profiles)
robot:
  id: limo-pro
  display_name: "AgileX Limo Pro"
resources:
  - id: nervus.resource.motion.base
    role: main
    capability: nervus.feature.motion.base          # BaseMotion@1
    provider:                                        # Port B binding → DimOS
      kind: dimos
      # how nervud reaches/starts the provider that implements BaseMotion@1 for this body:
      blueprint: coordinator-limo                    # the DimOS blueprint (see doc #3)
      transport: lcm                                 # DimOS transport the provider speaks
  # optional perception, if you expose lidar/costmap on the event bus:
  # - id: nervus.resource.perception.lidar
  #   role: main
  #   provider: { kind: dimos, blueprint: coordinator-limo }
```
Key point: the same `nervus.interface.motion.base` interface already serves the **GO2 dogs**;
the Limo is just *another body bound to a different provider*. Apps written against BaseMotion
(the Limo team's app, a dog app) work on both without change.

### 2. Interface availability
BaseMotion@1 is a **standard interface** already defined in `nervus-ipc`
(`proto/nervus/interface/basemotion/v1/basemotion.proto`, generated Go in
`go/protocol/interface/basemotionv1`). nervud already knows it; you only need the **provider
binding** (above) so `resolveEndpoint("nervus.interface.motion.base", resourceRole="main")`
resolves to the Limo provider's endpoint. Nothing new to define at the interface level.

### 3. Permissions (bus-layer, default-deny)
Apps that drive the Limo request **`nervus.permission.motion.control`**. Grant it in the
capability table for the Limo app package (the install-time permission prompt, app-store style).
`safety.*` stays system-only. Verify the grant path in nervud's permission model (enforced at
the **bus**, not the arbiter).

### 4. Arbiter — nothing Limo-specific
The **Control Focus Arbiter** is generic over resources. Once `nervus.resource.motion.base/main`
(Limo) is registered, `AcquireControl` on it participates in **SAFETY > HUMAN > AI > NONE**
automatically: an app's HUMAN lease can be preempted by SAFETY or another HUMAN; nervud stamps
each forwarded `SetVelocity` with the trusted `motion_epoch`/`lease_id` in ExecutionContext
(the app must not). Confirm the Limo resource is enrolled in the arbiter's resource set (usually
a side effect of the profile load).

### 5. (Optional) Perception on the event bus — for the Alien Wandering app
The Alien app needs the **laser scan** (obstacle gating) and **normalized visual targets** (its
vision runs in a sibling `AdventureX-software` process). Expose these as **event-bus topics** so
the app subscribes *by permission* (default-deny), instead of reading ROS topics:
- `event.perception.scan` — published by the DimOS Limo provider (the Tmini lidar source).
- `event.perception.target` — published by the vision process, re-homed as a NervusOS perception
  **source** (a small service/app). Grant the Alien app `nervus.permission.perception.read`.
The Alien's **visual/semantic memory (`data/*.db`) stays app-private** — it does not belong in the
kernel or the event bus.

---

## Sequence when the app drives the Limo (what the kernel does)
```
app: resolveEndpoint(motion.base, role=main)      → nervud returns the Limo provider endpoint
app: AcquireControl(resource=motion.base/main)    → arbiter grants HUMAN lease_id + motion_epoch
app: SetVelocity(linear_x, angular_z)             → nervud checks perm + lease + arbiter focus
nervud: attach ExecutionContext(lease_id, epoch)  → forward over Port B to the DimOS provider
DimOS: drive Limo; publish MotionState            → nervud → app (SubscribeMotionState)
SAFETY event / preempt                            → arbiter revokes lease; app's calls now fail
                                                     with CONTROL_NOT_HELD / STALE_EPOCH
```

## Checklist
```bash
git clone https://github.com/Nervus-OS/nervud && cd nervud
# 1. Read the profile + provider-binding schema (make the YAML above match reality):
grep -rniE "RobotProfile|provider|binding|resource|capability" internal/ | head -40
sed -n '1,120p' internal/pkgregistry/manifest.go
# 2. Add the limo-pro profile; restart nervud; confirm the resource is registered:
#    (however nervud lists resources) e.g. a CLI/RPC that dumps registered resources
# 3. Confirm resolveEndpoint(motion.base, role=main) → the DimOS limo provider (needs doc #3 running)
# 4. Confirm AcquireControl(motion.base/main) grants a HUMAN lease
# 5. Grant nervus.permission.motion.control to the Limo app package
```

## Don't
- Don't put Limo driver code in nervud. The kernel never touches CAN/serial/velocity.
- Don't create a new interface — reuse `nervus.interface.motion.base`.
- Don't special-case the Limo in the arbiter — it's generic over resources.
