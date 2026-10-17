# Nervus Gazebo Motion Provider Implementation Tasks

This file is the persistent implementation ledger for the motion-only Gazebo Provider. Complete
one task, verify it, update this file, and stop for review before starting the next task.

Scope rule: this repository provides motion and the feedback strictly required to close a motion
control loop. Camera, depth, audio, lidar, point-cloud, tactile and general sensor capabilities are
owned by other Nervus system services. This Provider must not register sensor interfaces, create
sensor Transfer streams or expose arbitrary Gazebo topic passthrough.

## Task 1: C++ Gazebo Transport Adapter

Status: completed

- Define a stable C ABI with opaque ownership and typed status codes.
- Implement Transport 15 discovery, raw publish, synchronous raw request and raw subscribe.
- Implement bounded event delivery, coalescing, drop accounting and deterministic teardown.
- Build dynamically against the installed Jetty Transport 15 / Msgs 12 libraries.
- Add standalone native tests using an in-process Gazebo Transport peer.

Verification:

- Gazebo Transport 15.1.0 and Msgs 12.0.2 versioned CMake packages detected.
- RelWithDebInfo build completed without compiler warnings.
- Functional and pure C ABI tests passed.
- Full native test set passed for 20 consecutive repetitions.
- Debug build passed AddressSanitizer and UndefinedBehaviorSanitizer tests.

## Task 2: Go cgo wrapper and pure Go simulation API

Status: completed

- Add Linux+cgo bindings and fail-closed non-cgo stubs.
- Hide all C types behind Go interfaces and typed errors.
- Implement event pumping, contexts, timeouts and idempotent close.

Verification:

- Go 1.25 Linux+cgo build and tests passed against Transport 15 / Msgs 12.
- `CGO_ENABLED=0` tests passed and the platform stub returned `UNAVAILABLE`.
- `go vet` and Go race detector passed for normal and tagged round-trip builds.
- Tagged C++ peer verified Go service request/response, subscribed events and Double publish.
- Native C++ and pure C ABI regression tests still passed.
- `ldd` confirmed dynamic dependencies on `libgz-transport.so.15`, `libgz-msgs.so.12` and
  `libstdc++.so.6`.

## Task 3: Nervus protobuf and Provider artifacts

Status: completed

- Define manager and control interfaces under `gazebo.simulation.motion.provider.*`.
- Generate Go protobuf code.
- Generate deterministic Provider descriptor and schema artifacts.
- Define private permissions and the simulation world resource.

Verification:

- Nervus registry parsed and validated both typed interface schema bundles and the Provider
  descriptor.
- Deterministic regeneration reproduced the checked-in Go protobuf, `provider.binpb` and
  `schemas.binpb` byte-for-byte.
- The descriptor declares two interfaces, one exclusive world resource and three bilingual
  private permissions under the package namespace.
- Manager methods are resource-free; all control mutations, including `ResetWorld`, are bound to
  `simulation.world.main` and require a ControlLease.
- Method/event type names, timeout and payload budgets, delivery classes, permissions and manifest
  exports are covered by contract tests.
- `go mod verify`, full cgo and non-cgo tests, race detector and `go vet` passed in
  `Nervus-Ubuntu`.

## Task 4: Nervus system service

Status: completed

- Implement ServiceHost lifecycle and manager/control endpoints.
- Register all handlers before endpoints.
- Implement status, permission-aware methods, events and graceful unregistration.

Verification:

- The manager endpoint registers immediately with all eight handlers; the resource-bound control
  endpoint registers only while the backend is ready and retains all six handlers across recovery.
- Runtime registration derives the same schema hashes as the deterministic Provider artifacts and
  binds control to `simulation.world.main`.
- Handler tests cover declared permissions, unknown fields, typed errors, bounded joint batches,
  trusted ControlLease execution context, absolute monotonic deadlines, motion epochs, resource
  generations and command-sequence replay rejection.
- Typed manager/control events preserve backend source timestamps, and DEGRADED status removes the
  control endpoint before advertising the new status.
- Shutdown drains and unregisters control, waits for in-flight handlers, closes the backend,
  unregisters manager and finally closes ServiceHost.
- `go test ./...`, `go vet ./...`, race tests, non-cgo tests, module verification and deterministic
  artifact regeneration passed in `Nervus-Ubuntu`.
- The current Nervus `sysmanifest` accepted a staged `gazebod` package and covered the binary plus
  both Provider artifacts with digests.
- The embedded Adapter test binary dynamically resolved Transport 15 and Msgs 12 without Sim, GUI
  or Rendering libraries; no Gazebo or C++ component was installed.

## Task 5: Jetty and NAO minimum closed loop

Status: completed

- Connect to `nervus_home`.
- Implement pause, resume, step, scene/model/joint queries and joint commands.
- Validate HeadYaw command feedback when joint state feedback is available.

Verification:

- The in-process Go/cgo backend discovered the real `nervus_home` control and scene services,
  world stats and all 24 subscriber-only NAO joint controller topics.
- Fake-Transport Go tests cover readiness, scene and controller discovery, pause, resume,
  pause-preserving multi-step, all reset modes, native joint limits, positive and negative error
  paths, optional joint feedback and connection-required `gz.msgs.Double` publishing.
- A tagged test against the existing Gazebo Jetty world paused simulation, advanced at least three
  reported iterations while remaining paused, resumed when required and submitted
  `HeadYaw=0.7` through the native Transport adapter.
- The current world has no JointStatePublisher. Capabilities and command receipts therefore report
  feedback unavailable instead of claiming the commanded position was reached.
- Subscriber-only controller discovery is snapshotted before TopicInfo-based waits because Jetty
  Transport 15 can list those topics while returning no per-topic metadata. Commands are only
  reported submitted after a typed publisher establishes a real subscriber connection.
- No Gazebo/C++ package was installed, no model was downloaded, and no launcher, WSLg workaround,
  `deploy/` file or `repo/nervus-system-server` file was modified.

## Task 6: Process lifecycle and fault recovery

Status: complete

- Added a pure-Go `ProcessController` boundary with attach and managed implementations. Attach
  rejects Start, Stop and LoadWorld and never signals an external PID. Managed mode canonicalizes
  allowlisted SDF paths, launches with an argument array and independent Linux process group,
  verifies `/proc/<pid>/stat` process identity, and uses SIGTERM plus a bounded SIGKILL fallback.
- Managed process exit is reported with owned PID, exit code and expected/unexpected state. A
  stopped managed process is not eligible for Transport discovery, and shutdown only stops the
  process owned by this service instance.
- Transport discovery now runs as a recovery loop. Runtime loss clears cached scene, controller,
  feedback and subscriptions, withdraws readiness, emits a typed fault, rediscovers the world and
  increments world generation before control can return. Historical control commands are never
  replayed.
- LoadWorld is implemented as stop, wait, change logical allowlisted world and start; it is not
  presented as Gazebo hot loading.
- Added a read-only WSLg probe for display, `NervusWSLgKeepalive`, shared memory and recent Weston
  COPY MODE faults. It never remounts, starts or stops keepalive, calls `wsl --shutdown`, or edits
  the existing launcher scripts.
- Added Linux process ownership/exit tests, Transport disconnect/recovery and stale-generation
  tests, WSLg probe tests and all typed lifecycle event projection tests.
- Verified in `Nervus-Ubuntu` with `go test ./...`, `go vet ./...`, `go test -race ./...`, the real
  Jetty NAO closed loop and the current launcher-owned WSLg environment. Existing Gazebo and
  keepalive processes remained running.

## Task 7: Generic robot profiles and standard Nervus capabilities

Status: completed on 2026-08-12

Task 7 was completed on `main`. It establishes the generic service foundation and stops for review
before a NAO motion branch is created.

Current contract namespace: `gazebo.simulation.motion.provider.*`. Source-level renames must
be followed by deterministic regeneration of `provider/provider.binpb` and `provider/schemas.binpb`
and by registry/schema-hash verification before the contract is considered closed.

### Task 7A: Versioned motion Profile contract

- Replace the current single `ExpectedModel` / 24-joint assumption with versioned, typed motion
  Profiles. A Profile describes model matching, motion Resources, stable roles, frames, joint
  groups, controller bindings, state sources, limits, tolerances and home states.
- Define the Profile schema in protobuf and use strict protojson as the human-edited source.
  Reject unknown fields, unsupported schema versions, duplicate roles/resources, invalid tokens,
  unsafe paths and non-finite values.
- Split signed package configuration from runtime instance configuration. Signed configuration may
  select only Resources, interfaces and adapters covered by the Provider descriptor. Runtime
  overrides may select world/model instances, partition, paths, timeouts and logging, but may not
  create capabilities or publish arbitrary topics.
- Bind every capability to a distinct Resource slot already declared by the signed descriptor.
  Multiple robot instances are supported only when distinct roles/slots are declared; configuration
  may not mint unlimited runtime Resources.
- Keep the Go module and `gazebod` binary stable during Task 7. The repository is now named
  `nervus-gazebo-motion-provider`; its Go module remains
  `github.com/nervus-os/nervus-gazebo-provider` for source compatibility. The private
  package/interface/permission/resource IDs use the `gazebo.simulation.motion.provider.*`
  namespace. Update human-facing manifest/descriptor metadata to the public product name
  **Nervus Gazebo Motion Provider**, then regenerate and verify all checked-in contract artifacts.

### Task 7B: Generic discovery and readiness

- Discover all scene models without depending on NAO names, joint counts or topic templates.
- Separate process readiness, Transport readiness, world readiness and each configured motion
  capability's readiness. A valid world remains manageable when no Profile matches a model.
- Track each Profile binding as discovering, ready, degraded or unsupported. Transport recovery
  invalidates controller/state caches and increments generation without replaying motion commands.
- Remove `ExpectedModel` and `MinimumControllable=24` from generic readiness. Preserve a configured
  fixed-NAO compatibility Profile as a regression fixture.

### Task 7C: Typed motion Adapter registry

- Introduce a compiled allowlist of semantic adapters: `joint_position`, `joint_trajectory`,
  `diff_drive` and `humanoid_gait`. Adapter names, topic/service direction and protobuf message
  types are fixed by code and Profile schema, not caller-provided strings.
- Implement the common motion lifecycle: bind, validate prerequisites, become ready, execute with
  deadline/lease context, stop, invalidate on generation change and close.
- Add reusable trajectory validation, time ordering, interpolation, joint-limit enforcement,
  cancellation and actual-state convergence checks. Gazebo trajectory time progress alone is not
  proof that a target was reached.
- Add interfaces for joint-state sources, root-pose sources and kinematics implementations. These
  are internal motion feedback dependencies, not externally exposed sensor capabilities.

### Task 7D: Nervus Resources and standard endpoints

- Keep simulation environment management on the existing `gazebo.simulation.motion.provider.*`
  compatibility interfaces. Expose robot motion semantics through existing Nervus standard
  interfaces such as `nervus.interface.motion.base` and `nervus.interface.manipulator.arm`.
- Register a separate Resource and endpoint for each configured base or arm role. Require exact
  agreement among Profile, Provider descriptor, schema artifacts and runtime endpoint binding.
- Do not register BaseMotion or Manipulator merely because matching Gazebo topics exist. The
  semantic adapter, command path, feedback path, stop behavior and required frames must all be
  ready.
- Preserve ControlLease, motion epoch, command sequence, monotonic deadline, Resource generation
  and stale-command rejection on every standard motion endpoint.

### Task 7E: Verification and stopping point

- Test missing Profiles, multiple models, multiple Profile instances bound to distinct signed
  Resource slots, unsupported adapters, message-type mismatch, duplicate Resource roles, unknown
  fields, bad paths and recovery during a command. All invalid capability configuration must fail
  closed without disabling world queries.
- Verify that the fixed NAO Profile still discovers and commands all 24 existing position
  controllers, while no standard arm/base capability is claimed until its complete adapter is
  implemented.
- Run `go test ./...`, `go vet ./...`, `go test -race ./...`, non-cgo tests, native tests and the
  real Jetty closed-loop test. Do not install Gazebo, download models or modify `deploy/`.
- Verify that no checked-in descriptor artifact still contains the old
  `gazebo.simulation.provider.*` namespace.
- Update this ledger and stop. The user creates the NAO development branch after reviewing Task 7.

### Task 7 completion record

- Added the protobuf-defined schema-v1 Profile bundle and runtime configuration, strict protojson
  loading, canonical allowlisted paths and validation for unknown versions/fields, duplicate roles,
  invalid tokens, non-finite limits, Adapter/type disagreement and descriptor closure.
- Added deterministic all-model discovery and per-binding discovering/ready/degraded/unsupported
  state. World readiness no longer depends on `nervus_nao`, a joint count or any matching Profile;
  recovery clears motion caches, invalidates generations and never replays stale commands.
- Added the compiled `joint_position`, `joint_trajectory`, `diff_drive` and `humanoid_gait`
  registry, common lifecycle boundaries, trajectory validation/interpolation/limits/cancellation/
  convergence helpers and internal-only feedback/kinematics interfaces. Only the private
  `joint_position` compatibility adapter is currently runtime-ready.
- Signed the standard BaseMotion and Manipulator interfaces, schemas and finite `base.main`,
  `arm.left` and `arm.right` Resource slots. Runtime binding fails closed instead of registering an
  empty or partial standard endpoint; the fixed NAO Profile therefore claims no standard base/arm
  capability.
- Added the fixed NAO schema-v1 regression Profile. The real Jetty test still discovers all 24
  position controllers and executes Pause, Step and a restorable HeadYaw command. The absent
  JointStatePublisher is reported as unavailable feedback rather than fabricated convergence.
- `scripts/verify-contract.sh` passed deterministic regeneration, module verification,
  `go test ./...`, `go test -race ./...` and `CGO_ENABLED=0 go test ./...`; `go vet ./...` passed.
  Native C++ and C ABI tests passed 2/2 against the installed Jetty Transport 15 / Msgs 12, and the
  tagged real Jetty closed-loop test passed against `nervus_home`.
- Source and checked-in Provider artifact scans contain no old
  `gazebo.simulation.provider.*` namespace. No Gazebo package/model was installed or downloaded,
  and neither `deploy/` nor `repo/nervus-system-server` was modified.

## Task 8: Fixed NAO variant packaging

Status: completed on 2026-08-13

- Added the fixed-NAO alternate build under
  `system-server/nao/nervus-gazebo-motion-provider` without modifying the NAO SDF or world.
- The default runtime configuration explicitly binds only `gazebo.nao.fixed.v1` to `nervus_nao`
  and `simulation.world.main`; discovery of another model cannot grant NAO capability.
- The variant has a distinct human-facing label and SDK name while retaining the same Provider
  package contract. It is an alternate build and cannot be installed beside the generic build.
- The real capability remains the 24 verified `gz.msgs.Double` joint-position controllers. Missing
  joint-state feedback prevents convergence claims and standard Manipulator registration.
- Walking, gripper, standard arm and all sensor capabilities remain unsupported. Future precise
  humanoid work proceeds in a separate G1 variant; this fixed NAO remains a regression fixture.

## Task 9: Dynamic locomotion

Status: not planned for this fixed-NAO variant

- Keep the fixed NAO Torso and its 24-controller regression behavior unchanged.
- Develop dynamic standing and walking in a separate G1 model/Profile variant with internal IMU,
  foot-contact and root-pose feedback.
- Never register `nervus.interface.motion.base` from this NAO variant or pretend that publishing
  `cmd_vel` / alternating leg joints is humanoid walking.

## Task 10: Full verification and packaging handoff

Status: pending

- Run unit, native, race, sanitizer and Nervus end-to-end tests.
- Complete operator and developer documentation.
- Produce packaging requirements without modifying `deploy/` until separately approved.

## Branch and merge policy

- `main` contains the generic Motion Provider foundation, typed adapters, schemas and stable
  regression fixtures.
- Feature branches such as `feature/nao-motion` isolate robot-specific development and unsafe model
  experiments.
- Robot support is distributed as validated Profiles and compiled semantic adapters, not maintained
  forever as divergent per-robot branches. Reusable infrastructure and validated Profiles return to
  `main` after review.
- Sensor services remain separate repositories/packages and are never folded into this task ledger.
