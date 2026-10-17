# Nervus Gazebo Motion Provider - NAO Fixed

This is a fixed NAO adaptation for the general Nervus Gazebo Motion Provider, located at
`gazebo/system-server/nao/nervus-gazebo-motion-provider`. It defaults to loading
`nao-fixed-v1.json` and `runtime-nao-fixed-v1.json`, binding only to `nervus_nao` in the world.
This adaptation does not modify `gazebo/models/nao` or existing worlds.

Current real capabilities are strictly limited to 24 `gz.msgs.Double` joint position controllers for the fixed Torso NAO, as well as
general world lifecycle, Pause, Resume, Step, and Reset. The current model has no JointStatePublisher,
so it does not declare that targets have converged; there are no dynamic gaits, standard Manipulators, Grippers, cameras, or sensor endpoints.
The product variant name is **Nerv. Gazebo Motion Provider - NAO Fixed**.
It reuses the general Provider's Resource, Interface, ControlLease, and Safety boundaries, enabling only already verified NAO compatible capabilities. The Go module remains
`github.com/nervus-os/nervus-gazebo-provider` for compatibility with existing dependencies, and the binary name remains `gazebod`. The technical namespace for private management contracts
is uniformly used
`gazebo.simulation.motion.provider.*`; after modifying proto, manifest, or contract constants, you must regenerate Provider artifacts and pass registry/schema hash validation, you cannot just replace source code strings.

This project is located in the Gazebo workspace, managed uniformly by the root Git repository of `gazebo/system-server`. It does not belong to
`repo/nervus-system-server`, nor does it write code to that directory; during implementation, it only treats existing
`nervus-system-server` and `nervus-ipc` as references for ServiceHost, Provider contracts, and lifecycle.

The target runtime is a persistent Go service process. Internally, it embeds a small C++ Adapter via cgo, which uses the native C++ Gazebo Transport API to connect to an already running Gazebo Server. It is not
loaded as a System Plugin into `gz sim`, nor is it a standalone bridge/sidecar.

Current status: overall architecture, C++ Gazebo Transport Adapter, Go/cgo Transport API, Nervus
protobuf/Provider contracts, ServiceHost system service layer, general Jetty backend, process lifecycle,
Transport recovery and versioned motion profiles have been implemented.
`gazebod` is the only persistent process; manager endpoint is always registered, world control endpoint is dynamically registered only when the backend confirms Transport, target world, world stats, and scene are ready. World readiness
does not depend on any specific model, number of joints, or motion profile.
The handler will revalidate permissions, ControlLease, Resource generation, motion epoch, command sequence, and Linux monotonic clock deadline, and completes removal, drain, and shutdown in bounded order.

attach backend support status, model, joint, Pause, Resume, Step, ResetWorld and
`gz.msgs.Double` joint position command. Current world has no JointStatePublisher, so only real
submission receipts are returned and `joint_state_feedback=false` is reported, and publish success is not
misrepresented as the joint being in place. Transport interruption will remove control readiness, clear old
scene/controller cache, continuously re-discover and increment world generation upon recovery. `attach` does not
own external processes; `managed` only stops process groups started by this service and
validated by Linux process identity. WSLg probe only reports read-only status, keepalive,
shared-memory and COPY MODE anomalies, and does not execute existing launcher's fix logic.

Task 7 has separated world/model generic discovery from motion Profile. The service will discover all models in
scene, then use strict, versioned Profile for deterministic binding; missing or incorrect Profile only makes corresponding
motion capabilities remain unsupported/degraded, without disabling available world management capabilities. Signed
Profile can only reference limited Resource slot, standard Interface and compiled-in Adapter declared by Provider
Descriptor, runtime configuration cannot create Resource, relax restrictions or turn any Gazebo topic into
unprivileged control capability.

Compiled-in Adapter allowlist is `joint_position`, `joint_trajectory`, `diff_drive` and
`humanoid_gait`. Currently only private compatible `joint_position` has been connected to real runtime; other three names,
message types, directions and lifecycle boundaries are fixed, but will not be registered as standard motion Endpoint
until full command, feedback, stop and safety semantics are implemented. `profiles/nao-fixed-v1.json` is a fixed Torso NAO's 24 joint regression
fixture, not a generic discovery rule, and does not declare mechanical arm or chassis standard capabilities.

## Service Boundary

This Provider is only responsible for motion and its direct closed-loop state:

- Simulation process, world, pause, resume, step and reset etc. motion test environment management;
- Model, joint and motion controller discovery;
- Single joint position, joint trajectory, mechanical arm, wheeled chassis and implemented gait control;
- Joint state, root pose, foot contact and pose feedback required for completing motion closed-loop; these data are only used
  as Provider internal control input or motion state summary, not as general sensor capability for external publishing.

Camera, stereo/depth image, microphone, general IMU, radar, point cloud, tactile and other perception streams are not part of this repository.
They are provided by independent Nervus sensor system services. These components can exist in Gazebo models, but this Provider
will not register interfaces for them, create Transfer streams or provide raw topic passthrough. IMU, contact etc. feedback read for
balance or fall detection in motion control cannot be used by callers as sensor Endpoint.

Full plan see [docs/SYSTEM-SERVICE-DESIGN.md](docs/SYSTEM-SERVICE-DESIGN.md).
C++ Adapter build and call constraints see [native/README.md](native/README.md). Continuous task status see
[TASKS.md](TASKS.md).

## Provider Contract

- `gazebo.simulation.motion.provider.manager@1`: Existing simulation environment state, models, joints, motion capabilities and managed
  process lifecycle compatibility contract.
- `gazebo.simulation.motion.provider.control@1`: Pause, Resume, Step, ResetWorld and joint commands;
  All methods bind to `simulation.world.main` and require ControlLease.
- `nervus.interface.motion.base`, `nervus.interface.manipulator.arm`: Interface, schema and
  `base.main`, `arm.left`, `arm.right` limited Resource slot have entered the signature artifact. Currently there is no runtime Adapter that satisfies
  the full semantics, therefore these two standard Endpoints will not be registered.
- `provider/provider.binpb` and `provider/schemas.binpb`: Generated deterministically by protobuf descriptor,
  and tested by Nervus registry's real parsing and invariant verification.
  When package/interface/permission/resource ID changes, these two artifacts must be regenerated together with proto and
  manifest.

## System Service Entry Point

Build Linux/cgo binary:

```bash
cd /mnt/h/robot/Nervus/gazebo/system-server/nao/nervus-gazebo-motion-provider
go build -o gazebod ./cmd/gazebod
```

Run parameters:

```text
gazebod -sock /run/nervus/control.sock -mode attach \
  -world nervus_home \
  -profile-root profiles -profile-bundle nao-fixed-v1.json \
  -runtime-config-root profiles \
  -runtime-config runtime-nao-fixed-v1.json \
  -log-level info
```

`-profile-bundle` is a signed capability configuration that is released with the package and constrained by the Descriptor allowlist;
`-runtime-config` selects only world, model instance, partition, timeout, and log level. Both types of JSON are strictly parsed by protobuf schema, and any unknown field, unknown version, duplicate role/Resource, unsafe path,
non-finite number, message type or Adapter inconsistency will cause a fail closed at startup.

`attach` does not start, stop, or kill external Gazebo. Use `-gz-partition` explicitly to select the same Transport partition as Gazebo; if not specified, it will use `GZ_PARTITION`. Services will not execute `gz topic`,
`gz service` or concatenate shell; all runtime control is handled through the in-process C++ Transport Adapter.

Explicit `managed` mode requires logical world, SDF, and allowlist root:

```bash
gazebod -mode managed -world nervus_home \
  -world-file /mnt/h/robot/Nervus/gazebo/worlds/nervus_home_nao.sdf \
  -world-root /mnt/h/robot/Nervus/gazebo/worlds
```

Here only uses the `exec.Cmd` parameter array to call `gz sim` to start the managed child process; it is not a topic/service control channel. The default is still `attach`, and it will not start a second Gazebo. `-managed-headless` does not require WSLg; GUI managed mode only checks the existing Windows launcher established WSLg environment, and does not take responsibility for fixing it.

Full Go verification:

```bash
go test ./...
go vet ./...
go test -race ./...
```

Connection to the already running default world execution of real Jetty minimal closed loop:

```bash
GZ_IP=127.0.0.1 go test -v -count=1 \
  -run TestRealJettyNAOMinimumClosedLoop \
  -tags=jettyintegration ./internal/jetty
```

Read-only verification of the current WSLg launcher environment:

```bash
go test -v -count=1 -tags=wslgintegration ./internal/wslg
```

This test verifies the publication of Pause, paused state Step, 24 controlled joints, and HeadYaw. The NAO's Torso is currently fixed to the world; this service does not and will not fake `cmd_vel`, forward walking, or backward walking capabilities.

Regenerate in `Nervus-Ubuntu`:

```bash
export PATH="/root/go/bin:$PATH"
cd /mnt/h/robot/Nervus/gazebo/system-server/nao/nervus-gazebo-motion-provider
bash scripts/generate-proto.sh
bash scripts/generate-provider.sh
bash scripts/verify-contract.sh
```

The generation script only reads `../../../../repo/nervus-ipc/proto`, and will not install Gazebo, download models, or modify `deploy/`.
