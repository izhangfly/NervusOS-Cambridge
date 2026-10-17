# Nervus Gazebo Motion Provider

`nervus-gazebo-motion-provider` is the robot-agnostic motion Provider baseline for Gazebo Jetty facing Nervus-OS. It is located at `gazebo/system-server/nervus-gazebo-motion-provider`, robot adaptations derive from this baseline; it defaults to loading `profiles/generic-v1.json`, and does not declare robot motion capabilities merely because a model is found.
It connects the simulated robot's joints, robotic arms, and chassis/locomotion motion capabilities into Nervus's standard Resource, Interface, ControlLease and Safety system. The product name is uniformly **Nervs Gazebo Motion Provider**; this product name is uniformly used in documentation. The Go module remains `github.com/nervus-os/nervus-gazebo-provider` for compatibility with existing dependencies, and the binary name remains `gazebod`. The technical namespace for private management contracts is uniformly used as `gazebo.simulation.motion.provider.*`; after modifying proto, manifest or contract constants, Provider artifacts must be regenerated and passed registry/schema hash verification, and cannot only replace source code strings.

This project is located in the Gazebo workspace and is uniformly managed by the root Git repository `gazebo/system-server`. It does not belong to `repo/nervus-system-server`, nor does it write code to this directory; during implementation, it only treats existing `nervus-system-server` and `nervus-ipc` as references for ServiceHost, Provider contracts and lifecycle.

The target runtime is a persistent Go service process. Internally, it embeds a small C++ Adapter via cgo, and the Adapter uses the native C++ Gazebo Transport API to connect to an already running Gazebo Server. It is not a System Plugin loaded into `gz sim`, nor is it a standalone bridge/sidecar.

Current status: overall architecture, C++ Gazebo Transport Adapter, Go/cgo Transport API, Nervus protobuf/Provider contracts, ServiceHost system service layer, generic Jetty backend, process lifecycle, Transport recovery and versioned motion profiles have been implemented.
`gazebod` is the only persistent process; manager endpoint is always registered, world control endpoint is dynamically registered only when the Transport, target world, world stats and scene are confirmed ready. World readiness does not depend on any specific model, number of joints or motion profile.
Handler will revalidate permissions, ControlLease, Resource generation, motion epoch, command sequence and Linux monotonic clock deadline, and complete removal, drain and shutdown in bounded order.

Attach backend supports status, model, joint, Pause, Resume, Step, ResetWorld and `gz.msgs.Double` joint position commands. Since the current world has no JointStatePublisher, it only returns real commit acknowledgments and reports `joint_state_feedback=false`, and does not pretend that joints are in place just because publish is successful.
Transport interruption will remove control readiness, clear old scene/controller cache, continuously re-discover and increment world generation upon recovery. `attach` does not own external processes; `managed` only stops process groups started by this service and verified by Linux process identity. WSLg probe only reports read-only status, keepalive, shared-memory and COPY MODE anomalies, and does not execute existing launcher's fix logic.

Task 7 has separated the world/model generic discovery and motion Profile. The service will discover all models in the scene, then use strict, versioned Profiles for deterministic binding; missing or incorrect Profiles only cause the corresponding motion capabilities to remain unsupported/degraded, without disabling available world management capabilities. Signature Profiles can only reference limited Resource slots, standard Interfaces, and compiled-in Adapters declared by the Provider Descriptor; runtime configuration cannot create Resources, relax restrictions, or turn any Gazebo topic into an unprivileged control capability.

Compiled-in Adapters allowlist is `joint_position`, `joint_trajectory`, `diff_drive`, and `humanoid_gait`. Private-compatible `joint_position`, configuration-driven multi-joint trajectory actuators, and standard `diff- drive` BaseMotion have been connected to real runtime; `joint_trajectory` native controllers and `humanoid_gait` remain disabled. Standard Manipulators will not be registered until FK, IK, TCP, and Operation semantics are complete. `profiles/nao-fixed-v1.json` only retains as a fixed Torso NAO 24-joint regression fixture, not as the default configuration of a general service, nor does it declare mechanical arm or chassis standard capabilities. Runnable NAO adapters are located in the adjacent `nao/nervus-gazebo-motion-provider`.

## Service Boundary

This Provider is only responsible for motion and its direct closed-loop state:

- Simulation process, world, pause, continue, single-step, and reset motion test environment management;
- Model, joint, and motion controller discovery;
- Single-joint position, joint trajectory, mechanical arm, wheeled chassis, and implemented gait control;
- Joint state, root pose, foot contact, and pose feedback required for completing motion closed-loop; these data are only used as Provider internal control inputs or motion state summaries, not as general sensor capabilities for external publication.

Cameras, stereo/depth maps, microphones, general IMUs, radars, point clouds, tactile, and other perception streams are not part of this repository. They are provided by independent Nervus sensor system services. These components can exist in Gazebo models, but this Provider will not register interfaces for them, create Transfer streams, or provide raw topic passthrough. IMU, contact, and other feedback read for balance or fall detection in motion control cannot be used by callers as sensor Endpoints.

Full plan see [docs/SYSTEM-SERVICE-DESIGN.md](docs/SYSTEM-SERVICE-DESIGN.md).
C++ Adapter build and call constraints see [native/README.md](native/README.md). Continuous task state see [TASKS.md](TASKS.md).

## Provider Contract

- `gazebo.simulation.motion.provider.manager@1`: Existing simulation environment state, models, joints, motion capabilities and managed
  process lifecycle compatibility contract.
- `gazebo.simulation.motion.provider.control@1`: Pause, Resume, Step, ResetWorld, direct joint
  commands, configure joint group trajectory, Home, cancel and full stop; all methods bind to `simulation.world.main` and
  require ControlLease.
- `nervus.interface.motion.base`, `nervus.interface.manipulator.arm`: Interface, schema and
  `base.main`, `arm.left`, `arm.right` limited Resource slot have entered the signature artifact. Differential Profile that satisfies complete feedback and
  stop semantics will dynamically register standard BaseMotion; standard Manipulator remains unregistered.
- `provider/provider.binpb` and `provider/schemas.binpb`: Generated deterministically by protobuf descriptor,
  and verified by real parsing and invariants check through Nervus registry testing.
  When package/interface/permission/resource ID changes, these two artifacts must be regenerated together with proto and
  manifest.

## System Service Entry Point

Build Linux/cgo binary:

```bash
cd /mnt/h/robot/Nervus/gazebo/system-server/nervus-gazebo-motion-provider
go build -o gazebod ./cmd/gazebod
```

Run parameters:

```text
gazebod -sock /run/nervus/control.sock -mode attach \
  -world nervus_home \
  -profile-root profiles -profile-bundle generic-v1.json \
  -log-level info
```

Explicitly enable NAO regression Profile when used:

```text
gazebod -sock /run/nervus/control.sock -mode attach \
  -world nervus_home \
  -profile-root profiles -profile-bundle nao-fixed-v1.json \
  -runtime-config-root profiles \
  -runtime-config runtime-nao-fixed-v1.example.json \
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
cd /mnt/h/robot/Nervus/gazebo/system-server/nervus-gazebo-motion-provider
bash scripts/generate-proto.sh
bash scripts/generate-provider.sh
bash scripts/verify-contract.sh
```

The generation script only reads `../../../repo/nervus-ipc/proto`, and will not install Gazebo, download models, or modify `deploy/`.
