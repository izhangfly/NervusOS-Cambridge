# Nervus Gazebo Motion Provider System Service Solution

## 1. Objectives and Boundaries

This project implements an independent Nervus Motion Provider system service, integrating the motion capabilities of robots in Gazebo Jetty into the existing Provider, ServiceHost, Endpoint, Resource, ControlLease,
permissions, event, and state system of Nervus-OS. The product name is **Nerv. Gazebo Motion Provider**.

Fixed boundaries:

- The common baseline is located at `gazebo/system-server/nervus-gazebo-motion-provider`, with Git history managed by `gazebo/system-server` uniformly; robot variants are located in their sibling subdirectories.
- No code is written into `repo/nervus-system-server`.
- `repo/nervus-system-server` and `repo/nervus-ipc` are only used as current contracts and examples for reading.
- The `deploy/` directory is not modified in this phase; packaging and system image integration will be designed separately later.
- Gazebo is not installed, models are not downloaded, and existing residential or NAO resources are not copied.
- Existing Gazebo Jetty 1.0.0, Gazebo Sim 10.5.0, Transport 15 and Msgs 12 are used.
- Only link Gazebo Transport and Msgs, do not link Sim, GUI, Rendering or physics engine.
- Adapter and Go service are in the same process, no bridge/sidecar service is added.
- Gazebo Server and GUI are still independent simulation processes, and cannot be compiled into the Provider process.
- Windows, WSL2, WSLg keepalive and shared-memory workaround are retained.
- Current NAO is fixed in world, only joint control and motion access templates are provided, no false `cmd_vel` bipedal walking capability is provided.
- This Provider is only responsible for joints, manipulators, wheeled chassis, gait, and internal feedback required for closed-loop motion.
- Perception capabilities such as camera, binocular/depth, microphone, general IMU, radar, point cloud, and tactile belong to other Nervus system services. This Provider does not register sensor interfaces, does not create sensor Transfer streams, and does not provide any Gazebo topic passthrough.
- Dynamic gait can internally read IMU, foot contact, and root pose, but only outputs motion state summary. These inputs do not constitute sensor Resources externally, and cannot be used as sensor Endpoints by other callers.

## 2. Overall Architecture

Implementation uses a Go service process, a Nervus `ServiceHost`, and a C++ `gz::transport::Node`. The existing version registers manager/control two Endpoints on the same IPC connection; the standard motion Endpoint must also reuse this connection, and can only be added according to already signed motion Resource.

```text
Nervus caller
    |
    | ResolveEndpoint / Dispatch / Subscribe
    v
gazebod: one Go process
    |-- manager endpoint Always exists, responsible for status and process lifecycle
    |-- world control endpoint Exists after Gazebo is ready, responsible for world and low-level joint control
    |-- motion endpoints Exist after Profile is ready, registered according to base/arm Resource
    |-- Go service core State machine, parameter validation, error mapping
    |-- motion adapters Joints, trajectories, chassis, gait, and closed-loop state
    |-- process controller attach or managed mode
    +-- cgo C ABI
          +-- C++ GazeboTransportAdapter
                +-- gz::transport::Node
                      +-- Gazebo Jetty server
```

These Endpoints are not multiple services. They share the same process, same ServiceHost, same Transport Adapter and same shutdown process. Standard motion Endpoints are only registered after the corresponding Profile, command path, feedback path and stop semantics are all ready.

The management plane must always exist, otherwise the caller cannot Resolve to the "start Gazebo" method after Gazebo stops. The world control plane is only registered after Transport discovers the target world, control service, stats and scene; it is removed upon connection loss to avoid continuing to publish unavailable backends as healthy Providers. Robot controller and feedback only affect their respective motion Endpoints, no longer blocking world readiness.

## 3. Names and Identity

Existing simulation management compatibility contracts use the private `gazebo.*` namespace. Standard robot motion capabilities use `nervus-ipc` already defined `nervus.interface.motion.base`, `nervus.interface.manipulator.arm` and corresponding standard Resources, not creating parallel protocols.

| Item | Value |
|---|---|
| Product Name | `Nervus Gazebo Motion Provider` |
| Git Repository | `nervus-gazebo-motion-provider` |
| Go module | `github.com/nervus-os/nervus-gazebo-provider` |
| Binary | `gazebod` |
| package ID | `gazebo.simulation.motion.provider` |
| component ID | `main` |
| SDK name | `nervus-gazebo-motion-provider` |
| manager interface | `gazebo.simulation.motion.provider.manager` |
| control interface | `gazebo.simulation.motion.provider.control` |
| world resource type | `gazebo.simulation.motion.provider.resource.world` |
| world stable role/handle | `simulation.world.main` |

Go module remains `github.com/nervus-os/nervus-gazebo-provider` for compatibility with existing code, binary name remains `gazebod`. The technical namespace for private packages, interfaces, permissions and world resources is unified as `gazebo.simulation.motion.provider.*`. Any technical ID modification must also migrate contract constants, manifest, proto, generated Go code, `provider.binpb`, `schemas.binpb` and caller references, and pass through Descriptor/schema hash verification; it cannot only modify source code strings or documentation.

All private permissions are strictly located under the package ID:

| Permission | Purpose | Initial Recommendation |
|---|---|---|
| `gazebo.simulation.motion.provider.permission.query` | Status, model, joint, capability query | NORMAL, ordinary trust |
| `gazebo.simulation.motion.provider.permission.control` | Pause, resume, step, joint command | USER_CONSENT, ordinary trust |
| `gazebo.simulation.motion.provider.permission.lifecycle` | Start/stop process, switch world, reset | PRIVILEGED, OEM trust |

The GrantMode, RiskClass, MinimumTrust, Chinese and English display names, and descriptions must be entered into `ProviderDescriptor.permissions`, and cannot be written only in Go constants or documentation. Before the formal implementation, confirm whether lifecycle should be changed to SIGNATURE based on the actual caller's signature role.

## 4. Recommended Directory

```text
nervus-gazebo-motion-provider/
├── README.md
├── go.mod
├── go.sum
├── manifest.json.in
├── cmd/
│   └── gazebod/
│       └── main.go
├── internal/
│   ├── app/ Assembly, signals, startup and shutdown sequence
│   ├── config/ Configuration parsing and validation
│   ├── provider/ ServiceHost, Endpoint, handler, events
│   ├── simulation/ Pure Go domain interface and state machine
│   ├── simprocess/ attach/managed process control
│   └── gztransport/
│       ├── client.go Go interface that does not expose C types
│       ├── errors.go
│       ├── events.go
│       ├── native_linux.go   linux && cgo
│       ├── native_stub.go    !linux || !cgo, fail closed
│       └── adapter_embed_linux.cc Embed the native Adapter into the same Go binary
├── native/
│   ├── include/ Stable C ABI
│   └── src/ Transport Node Implementation
├── proto/
│   └── gazebo/simulation/provider/v1/
│       ├── manager.proto
│       └── control.proto
├── gen/go/ Generate and submit the Go protobuf
├── providergen/ Generate provider.binpb/schemas.binpb
├── provider/ Submitted deterministic Provider binary artifact
├── scripts/ Repository build, generate and check scripts
├── testdata/ Small test world, no third-party model duplication
└── docs/
    └── SYSTEM-SERVICE-DESIGN.md
```

Independent repositories do not need to follow the "all files are package main" restriction of `nervus-system-server`.
Internal packages can be split, but ultimately only one `gazebod` process is generated.

## 5. Provider Contract /no_think

### 5.1 Manager interface

`gazebo.simulation.motion.provider.manager@1` does not bind Resource. Provider registers immediately after completing ServiceHost handshake.

| ID | Method | Permission | Read-only | Description |
|---:|---|---|---|---|
| 1 | `GetStatus` | query | Yes | Returns status of process, Transport, world, and simulation layers |
| 2 | `ListModels` | query | Yes | Returns model summary from scene/info |
| 3 | `ListJoints` | query | Yes | Returns joints, axis, limits, and controllability |
| 4 | `GetJointState` | query | Yes | Returns latest position, velocity, and sample time |
| 5 | `StartSimulation` | lifecycle | No | Starts managed Gazebo only in managed mode |
| 6 | `StopSimulation` | lifecycle | No | Stops only the process started and held by this service |
| 7 | `LoadWorld` | lifecycle | No | Controlled restart after validating world, does not accept shell text |
| 8 | `GetCapabilities` | query | Yes | Returns runtime mode and optional capabilities |

`ListModels` uses `/world/<world>/scene/info`, response `gz.msgs.Scene`.
`GetStatus` real-time status comes from `/world/<world>/stats`, type
`gz.msgs.WorldStatistics`, Jetty Server publishes by default at 5 Hz.

### 5.2 Control interface

`gazebo.simulation.motion.provider.control@1` binds world Resource, registering only after Adapter reaches READY.

Resource declaration:

- stable role: `simulation.world.main`
- resource type: `gazebo.simulation.motion.provider.resource.world`
- access mode: `EXCLUSIVE_CONTROL`
- risk class: `NORMAL`
- private label: `gazebo.simulation.motion.provider.kind=world`

| ID | Method | Permission | Lease | Description |
|---:|---|---|---|---|
| 1 | `Pause` | control | yes | `WorldControl.pause=true` |
| 2 | `Resume` | control | yes | `WorldControl.pause=false` |
| 3 | `Step` | control | yes | Only accepted in paused state, with step count limit |
| 4 | `SetJointPosition` | control | yes | Publish `gz.msgs.Double` to validated topic |
| 5 | `SetJointPositions` | control | yes | Bounded batch, same model and request deadline |
| 6 | `ResetWorld` | control | yes | Explicit reset, cannot be auto-replayed during connection retries |

Control method risk class is initially set to NORMAL, as they only change simulation, not real hardware.
`SetJointPosition(s)` still mark `is_motion=true`. Pause, Resume, Step, ResetWorld, and joint commands all require ControlLease, preventing multiple callers from concurrently competing for the same simulation. ResetWorld must be located in the control interface of the bound world Resource; currently, nervud will reject methods declared with `requires_control_lease=true` on non-Resource manager endpoints.

NAO first version only allows axis 0, topic shape is: /no_think

```text
/model/<model>/joint/<joint>/<axis>/cmd_pos
```

The current supported instance is `model=nervus_nao`, with message type `gz.msgs.Double`. The Adapter must validate the model, joints, and axis using scene, joint state, and Transport type information, and cannot simply concatenate strings and publish.

### 5.3 Standard motion interfaces

Task 7 has fixed the closed rules for Profile, Descriptor, schema, and limited Resource slot. Only fully implemented semantic Adapter can be bound to Nervus existing standard interface:

- `nervus.interface.motion.base`: wheel-based chassis or already verified biped gait;
- `nervus.interface.manipulator.arm`: left arm, right arm, or other mechanical arms;
- Each base/arm stable role corresponds to independent Resource, ResourceHandle and Endpoint;
- Interface, Resource type, stable role and permissions must have already entered the signed Provider Descriptor;
- The existence of topic only proves Transport visibility, not the completeness of standard interface semantic implementation.

The prerequisite for standard Endpoint READY at least includes model binding, controller type, message direction, joints/coordinate system, limitations, feedback, stop behavior and generation consistency. If any item is missing, the capability remains discovering, degraded or unsupported, and it is not allowed to register an Endpoint that would return false success in advance.

Differential drive Adapter and biped gait Adapter are different implementations. `diff_drive` can map Twist, odometry and wheel speed limits; `humanoid_gait` must implement standing, balance, gait, controlled stop and fall handling. It is not allowed to configure a `/cmd_vel` topic for NAO and declare it as BaseMotion.

Mechanical arm Adapter is responsible for joint order, trajectory, FK, constrained IK, coordinate system, Home, Stop and target convergence. Only the time progress of JointTrajectoryController does not constitute evidence of completion, and the result must be judged based on actual joint state and configuration tolerance.

### 5.4 Events

Manager interface:

| ID | Event | delivery class | Description |
|---:|---|---|---|
| 1 | `StatusChanged` | STATE | Only the latest status is retained, maximum 10 Hz |
| 2 | `ProcessExited` | RELIABLE | Managed Gazebo abnormal exit and exit code |
| 3 | `TransportFault` | RELIABLE | Discovery, type, request timeout, queue overflow |
| 4 | `WSLgFault` | RELIABLE | keepalive, COPY MODE, display environment abnormality |
| 5 | `WorldChanged` | STATE | world name or generation change |

Control interface:

| ID | Event | delivery class | Description |
|---:|---|---|---|
| 1 | `JointStateChanged` | STATE | Merge latest joint state, default maximum 20 Hz |
| 2 | `CommandRejected` | RELIABLE | Asynchronously discovered state or type rejection |

Camera, depth image, audio, radar point cloud, tactile and other sensor data do not enter this Provider's event
Envelope, nor is a sensor interface or Transfer stream established by this Provider. Corresponding capabilities are handled by other
Nervus system services. This Provider's events only report simulation environment, Transport, motion Resource,
control command and motion loop closure faults.

## 6. Protobuf and Provider artifacts

The two private interfaces' proto package use `gazebo.simulation.motion.provider.v1`. Each method enum value
digit equals method ID, and is attached to Nervus `method_meta`:

- method ID 0 is reserved for UNSPECIFIED.
- request, response, error detail use full protobuf type names.
- Query sets `is_read_only=true`.
- Mutating sets `is_read_only=false`.
- Joint command sets `is_motion=true`.
- Methods that require arbitration set `requires_control_lease=true`.
- Events explicitly write STATE or RELIABLE, cannot rely on default values.

All handlers use generated enum constants, do not copy digits in Go. `providergen`, runtime
`schemaHash()` and handler must be constructed from the same generated descriptor to form the contract.

This repository's `providergen` generates: /no_think

- `provider.binpb`
- `schemas.binpb`

The current Descriptor simultaneously declares two compatible interfaces, one world Resource, and three private permissions.
If package, interface, permission, or resource ID changes, `provider.binpb` and
`schemas.binpb` must be regenerated and kept byte-wise consistent with the proto descriptor in source code, manifest export, and runtime contract constants.
When Task 7 adds a standard motion Endpoint, the Descriptor must also statically declare the allowed standard interfaces, motion Resources, and stable roles that can be provided; Profile can only be selected from this signature set. Manifest exports, Descriptor interfaces/resources, Profile bindings, and runtime RegisterEndpoint must be fully consistent, and schema hash must be byte-wise consistent.

## 7. Go Domain Layer

Provider handler does not directly call cgo; it only relies on pure Go interfaces:

```text
SimulationManager
    Status(ctx)
    Capabilities(ctx)
    Start(ctx, options)
    Stop(ctx)
    LoadWorld(ctx, world)
    ListModels(ctx)
    ListJoints(ctx, model)
    JointState(ctx, model, joint)

SimulationController
    Pause(ctx)
    Resume(ctx)
    Step(ctx, count)
    ResetWorld(ctx, mode)
    SetJointPosition(ctx, command)
    SetJointPositions(ctx, commands)

MotionBindingRegistry
    Bind(profile, model, generation)
    CapabilityStates()
    Invalidate(generation)
    Close()

MotionAdapter
    Validate(binding)
    Ready()
    Stop(ctx, executionContext)
    Close()

JointStateSource / RootPoseSource / Kinematics
    provide internal feedback required by motion adapters
```

Provider handler is responsible for:

- protobuf decoding and required field checks;
- taking the minimum of the caller's deadline and service limit;
- validation of model, joint, axis, numerical range, and batch size;
- mapping of Nervus status code to typed error detail;
- structured logging, but not returning path or C++ exception text as-is;
- not executing shell, `gz topic`, or `gz service`.
- not accepting any topic, service, or protobuf type provided by the caller;
- not registering or proxying sensor interfaces and sensor data streams.

Test using fake Transport Client, without starting Gazebo, and without requiring cgo.

## 8. C++ Adapter

### 8.1 Responsibility

C++ Adapter is responsible only for Gazebo Transport and gz-msgs:

- Creating and destroying a `gz::transport::Node` ;
- Discovering topics and services ;
- Caching Publishers by topic, and rejecting subsequent use of different message types for the same topic ;
- Calling `PublishRaw`, `SubscribeRaw`, `RequestRaw` ;
- Serializing and deserializing corresponding gz-msgs types with Jetty ;
- Putting callback messages into a bounded native queue ;
- Providing a stable C ABI ;
- Capturing all C++ exceptions and prohibiting exceptions across C boundaries.

It is not responsible for Nervus IPC, permissions, Lease, manifest, Windows `wsl.exe` lifecycle, GUI,
shell execution, motion semantics, gait, balance, sensor interface, or Transfer data stream.

### 8.2 C ABI

ABI passes only integers, fixed-layout structures, pointers with lengths, and opaque handles. Passing
`std::string`, STL containers, Gazebo classes, protobuf C++ objects, or exceptions is prohibited.

Plan to provide:

```text
gz_adapter_create
gz_adapter_close
gz_adapter_destroy
gz_adapter_wait_ready
gz_adapter_topic_info
gz_adapter_service_info
gz_adapter_publish
gz_adapter_request
gz_adapter_subscribe
gz_adapter_unsubscribe
gz_adapter_next_event
gz_adapter_free_buffer
```

Return status at least distinguishes:

```text
OK
INVALID_ARGUMENT
CLOSED
NOT_FOUND
NOT_CONNECTED
TYPE_MISMATCH
TIMEOUT
REMOTE_REJECTED
QUEUE_FULL
NATIVE_EXCEPTION
INTERNAL
```

The two results of `RequestRaw` must be handled separately: unexecuted indicates timeout or Transport failure; executed but
service result is false indicates remote rejection.

### 8.3 Callback Model

The first version does not allow Gazebo internal threads to directly callback Go:

1. `SubscribeRaw` callback only copies necessary metadata and serialized payload.
2. Data enters a C++ bounded queue or a latest-value table by key.
3. Go starts a goroutine, blocking call to `gz_adapter_next_event(timeout)`.
4. Adapter close wakes up condition variable, allowing the goroutine to bounded exit.

This avoids having C++ hold Go pointer long-term and avoids handling deleted
`runtime/cgo.Handle` during shutdown.

Go `Client`'s discovery wait, publish connection wait, and event read use short-period native
calls to check `context.Context`. Synchronous `RequestRaw` does not automatically retry and cannot safely be canceled during execution;
its native timeout takes the smaller value of request timeout and context deadline. If context is
canceled during the call, the method returns context error, but the caller must treat the request as "remote may have executed" for side-effecting requests.

Queue strategy:

- world stats, joint state: merged by topic/key, only latest value is retained;
- process/transport fault: bounded ring buffer, overflow forms metric and fault;
- sensed data: this Motion Provider does not subscribe, queue, or establish Transfer;
- no use of unbounded queues, no setting Transport HWM to 0.

### 8.4 Concurrency and Shutdown

The first version adopts a conservative model:

- Publisher cache and Subscriber map are each locked;
- Publish path does not hold request lock;
- native Adapter executes synchronous `RequestRaw` serially; Go layer uses bounded executor to limit pending requests;
- Request wait does not hold global mutex;
- callback only enqueues, no further Node call in callback;
- Jetty's `SubscribeRaw` only returns success status, does not return RAII Subscriber. Adapter thus limits each
  topic to one raw subscription, first marking the subscription as inactive, then explicitly calling
  `Node::Unsubscribe(topic)` ;
- Adapter maintains closing flag and in-flight callback count.

`Close` must be idempotent; repeated `Unsubscribe` returns `NOT_FOUND`. `Destroy(NULL)` is safe, but valid
handle can only be destroyed once, caller must first end all threads using this handle. During shutdown, first block new requests
and callbacks, then wait for in-flight to zero, finally destroy Publisher, Node, and Adapter.

## 9. Gazebo Transport Mapping

| Capability | Jetty Transport |
|---|---|
| Pause/Resume | `/world/<world>/control`, `WorldControl -> Boolean` |
| Step | Same control service, using `multi_step`, only allowed when paused |
| Simulation State | `/world/<world>/stats`, `WorldStatistics` |
| Scene/Model | `/world/<world>/scene/info -> Scene` |
| Joint Command | `/model/<model>/joint/<joint>/<axis>/cmd_pos`, `Double` |
| Joint State | `/world/<world>/model/<model>/joint_state`, `Model` |

Joint state topic only exists when world/model loads `JointStatePublisher`. Implementation cannot fabricate
"command has been reached":

- `PublishRaw` success only indicates the message has been submitted;
- Check publisher connection within bounded time before first publish;
- If command confirmation is needed, wait for joint state to reach tolerance and check timeout;
- If current world has no `JointStatePublisher`, capability reports
  `joint_state_feedback=false`, cannot return false feedback.

Jetty's current `scene/info` may include models but not NAO joints, subscriber-only control topic
may also appear in TopicList but cannot be queried via TopicInfo for now. Therefore Backend first takes a snapshot of strict match
`/model/<model>/joint/<joint>/0/cmd_pos` topic, then queries services and stats; confirmed controllers will
refresh cache across scenes. Actual command must still be established with subscription connection using `gz.msgs.Double` and successfully published before returning
submission success. Unknown scene joint types and limits are clearly marked as unknown, no fabrication of native metadata.

## 10. State Model

Do not represent the entire simulation with a single boolean value, response is divided into three layers: /no_think

```text
ProcessState:
    UNKNOWN / EXTERNAL / STOPPED / STARTING / RUNNING / STOPPING / EXITED

TransportState:
    DISCONNECTED / DISCOVERING / READY / DEGRADED

SimulationState:
    UNKNOWN / RUNNING / PAUSED

MotionCapabilityState (per Profile binding and Resource):
    DISCOVERING / READY / DEGRADED / UNSUPPORTED
```

Status includes:

- world name and generation;
- attach/managed mode;
- whether the process is held by this service and PID;
- monotonic age of recent stats;
- sim time, real time, iterations, real-time factor;
- scene, motion Profile, controller and motion feedback capability bits;
- stable reason enum of recent fault;
- Adapter, Gazebo Sim, Transport and Msgs runtime versions.

Translation principles:

- ServiceHost is normal while Gazebo is not running: manager exists, STOPPED/DISCONNECTED.
- Control service is discovered but stats timeout: DEGRADED, do not register control.
- Stats, scene and control service are all ready: READY, register control.
- Stats exceed threshold: first remove control, then report DEGRADED.
- Gaze bze recovers: rediscover and register, using new endpoint ID and generation.
- world READY does not require any robot Profile READY; each motion Resource is independently registered and removed.
- Some motion feedback is expired: first stop/reject the capability and remove corresponding Endpoint, without affecting other Resources' faults.

## 11. Process Control

### 11.1 attach mode

Default attach in first version, compatible with current Windows main entry point: /no_think

```text
start-gazebo.cmd
  -> start-gazebo.ps1
      -> wsl -d Nervus-Ubuntu -u root
          -> start-gazebo-wsl.sh
              -> WSLg prepare/keepalive
              -> gz sim
```

Provider does not close keepalive, does not reattach shared memory, does not execute `wsl --shutdown`, and also does not kill Gazebo that it did not start itself. In attach mode:

- Status, query, world control and joint control work normally;
- Automatically enters READY after discovering already running Gazebo;
- Start, Stop, LoadWorld return stable `PROCESS_EXTERNALLY_MANAGED`;
- Does not secretly `pkill`, nor start a second server.

### 11.2 managed mode

managed is an explicit configuration used for environments where Gazebo child processes are managed by the same Provider:

- Only start `worlds/*.sdf` under allowlist;
- Parameters use `exec.Cmd` parameter array, not shell;
- Child processes use independent process group;
- Record PID and start identity, only stop processes it holds;
- Stop first sends SIGTERM, waits for grace period, and finally sends SIGKILL;
- LoadWorld uses stop, wait, start and new generation, does not claim hot loading;
- Does not call `prepare` or `wsl --shutdown`;
- Reports fault if WSLg is not ready;
- GUI still depends on Windows established keepalive; headless server can use independent configuration.

Both modes implement the same `ProcessController` Go interface. Provider handler does not know specific scripts, will not scatter PowerShell, Bash or Gazebo CLI.

## 12. Environment and Discovery

`GZ_IP=127.0.0.1` remains unchanged.

Gazebo launcher and Provider must explicitly configure the same `GZ_PARTITION`. Gazebo is currently started by root, while Nervus package uses independent UID; default `<hostname>:<username>` will put both into different partitions, resulting in all topics/services disappearing.

Recommended partition name: /no_think

```text
nervus-gazebo
```

This round does not modify the startup script. The value must be added on both the launcher and Provider sides before joint debugging.

Current discovery order:

1. Create Transport Adapter/Node.
2. Confirm the specific types of world control, scene service, and stats topic.
3. Subscribe to stats and receive the first frame; once the condition is met, the world enters READY, regardless of any robot Profile.
4. Request scene/info, and establish a common directory for all models.
5. For each Profile, perform a strict model matcher to form zero or more instance bindings.
6. Adapter discovers command topic/service and state source according to its fixed rules; if necessary, snapshot subscriber-only topic before TopicInfo.
7. Validate controller type, joints/coordinate systems, limits, feedback, Resource declarations, and generation.
8. Each complete motion capability independently enters READY and registers a standard Endpoint; incomplete capabilities remain in discovering, degraded, or unsupported.

Existing fixed NAO Profile is retained for compatibility: it matches `nervus_nao` and 24 position-controller topics, but these values no longer participate in the general world readiness, nor do they become protocol constants.

Discovery uses bounded deadline and exponential backoff, and does not repeatedly perform full TopicList/ServiceList in Dispatch handler.

## 13. Configuration and Motion Profile

Configuration is divided into signature capability configuration and runtime instance configuration. The two cannot be used interchangeably.

### 13.1 Signature Capability Configuration

Motion Profile is released with the system package, its content is included in package digest, and is constrained by the static capability set of Provider Descriptor. Task 7 has already defined a versioned schema using protobuf, with strict protojson as the manually maintained source; unknown fields must not be discarded during parsing.

Each Profile must at least include:

- `schema_version`, stable profile ID, robot/variant description;
- model matcher: exact name, allowed instance naming rules, and optional structural features;
- capability: standard interface, Resource type, stable role, and compiled-in Adapter kind;
- Resource slot: reference the unique stable role signed declared in Provider Descriptor;
- frame: base, root, tool, left/right arm, etc., explicit coordinate systems;
- joint group: stable order, axis, upper and lower limits, speed/acceleration limits, zero position, and Home;
- command binding: Adapter allowed topic/service templates and fixed message types;
- state source: joint state, root pose, and IMU/contact needed internally for gait;
- completion policy: position/pose tolerance, settle time, feedback expiration threshold, and command timeout;
- controller exclusivity: only one command source can exist for the same joint in the same binding.

Profile cannot:

- Declare interface, Resource, or stable role that Provider Descriptor has not signed;
- Bind the same Resource slot to multiple models, or create new stable roles at runtime;
- Specify Adapter names that are not compiled;
- Concatenate caller input into any Gazebo topic/service;
- Override Adapter fixed protobuf types, directions, or safety constraints;
- Declare sensor capabilities such as camera, audio, radar, tactile, or Transfer flow.

### 13.2 Compiled-in Adapter Registry

Allowed Adapter kinds in compilation:

| Adapter | Gazebo Command Semantics | Nervus Semantics | Feedback Requirements |
|---|---|---|---|
| `joint_position` | Single-axis position command | Private low-level joint control | Publish connection; arrival declaration also requires joint state |
| `joint_trajectory` | Multi-joint temporal trajectory | Manipulator base | Joint state, limits, interpolation, and convergence |
| `diff_drive` | Differential wheel velocity | BaseMotion | Odometry/root pose, stop confirmation |
| `humanoid_gait` | Balance and gait implementation | BaseMotion | Root pose, IMU/contact internal feedback |

The registry is bound by code to Adapter name, message type, allowed template fields, direction, preconditions, and constructor.
Configuration can only fill these enums and their restricted parameters. The current service has no `camera`, `lidar`, `imu_sensor` or
`generic_topic` Adapter. Task 7 only connected `joint_position` to the private compatible control plane;
`joint_trajectory`, `diff,drive` and `humanoid_gait` have fixed contracts with public lifecycle, but no complete
runtime implementation, thus cannot make standard Endpoint READY.

### 13.3 Runtime Instance Configuration

Priority: command line > environment variables > configuration file > safe defaults. Runtime configuration is only responsible for deployment and instance selection:

- Nervus socket path and log level;
- `mode=attach|managed`;
- logical world, default world path and allowlisted world root;
- renderer `gpu|cpu|headless`;
- GZ_IP and GZ_PARTITION;
- Profile file path and allowed enabled signed profile ID;
- world/model instance name;
- discovery, service request, stats stale and shutdown timeout;
- event queue capacity, max request concurrency and max step count.

Runtime override cannot add new capability, Resource, topic, message type, joint group or relax motion limits.
Model instance override only changes matching instances, not the semantics and permissions of the Profile.

Each running capability must occupy a unique Resource slot declared by the Descriptor, for example
`base.main`, `arm.left` or `arm.right`. Simultaneously controlling multiple robots requires pre-declaration of different
role/slot in the signed artifact; "Generic Profile" indicates support for different robot structures and controllers, not indicating that configuration can infinitely generate unsigned Resource.

### 13.4 Validation and Failure Strategies

Paths are converted to canonical absolute path at startup, and confirmed to still be within allowlist root. world, model,
joint and frame only accept Gazebo legal tokens; values containing path separators, NUL or control characters are all rejected.

The following errors must fail closed: unknown schema/Adapter, unknown field, duplicate stable role, Descriptor
inconsistency, message type inconsistency, empty joint group, duplicate joint, non-finite number, inverted upper/lower bounds, unparsable frame,
unsafe path and controller conflict for the same joint. A single Profile cannot bind only making that motion capability unavailable;
it should not close already READY world management capability or other robot Resource.

## 14. Error Model

typed error detail must at least include:

```text
PROVIDER_NOT_READY
PROCESS_EXTERNALLY_MANAGED
PROCESS_ALREADY_RUNNING
PROCESS_NOT_RUNNING
WORLD_NOT_FOUND
WORLD_NOT_READY
WORLD_CHANGED
TRANSPORT_DISCONNECTED
SERVICE_NOT_FOUND
TOPIC_NOT_FOUND
TYPE_MISMATCH
REQUEST_TIMEOUT
REMOTE_REJECTED
MODEL_NOT_FOUND
JOINT_NOT_FOUND
JOINT_NOT_CONTROLLABLE
AXIS_NOT_SUPPORTED
OUT_OF_RANGE
FEEDBACK_UNAVAILABLE
QUEUE_OVERFLOW
WSLG_NOT_READY
NATIVE_FAILURE
```

Retry rules:

- Query can be retried once based on deadline;
- Joint commands, Step, Reset, Start, Stop, LoadWorld are not automatically replayed;
- Transport does not replay historical control commands after reconnection;
- batch only guarantees atomic validation before Adapter reception, and does not claim Gazebo physics update atomicity;
- Each response carries world generation, and the caller can identify whether the world has been replaced.

## 15. Service lifecycle

Startup order:

1. Parse runtime configuration, signature motion Profile, and verify closure with Provider Descriptor.
2. Initialize logger, metrics, and state machine.
3. Establish ServiceHost; exit directly on handshake failure, handled by supervisor.
4. Construct manager endpoint, install all handlers first.
5. Register manager endpoint using schema hash.
6. Create process controller.
7. Create C++ Adapter and Go event pump.
8. Run discovery state machine in background.
9. After world READY, construct compatible control endpoint, install handlers first, then register.
10. For each READY motion binding, construct independent Resource Endpoint, install full handlers, then register.
11. `ServiceHost.Serve()` blocks waiting for shutdown or connection break.

Shutdown order:

1. Block new motion and world control requests.
2. Execute bounded Stop on all READY motion Resource, drain and remove motion Endpoint.
3. Drain and remove compatible control endpoint.
4. Stop discovery loop and event pump.
5. Close Adapter and wait for native callback.
6. Managed processes are stopped according to configuration; attach does not stop external processes.
7. Drain and remove manager endpoint.
8. Close ServiceHost.
9. Bounded wait for all goroutines and native callbacks to exit.

When ServiceHost unexpectedly disconnects, the process exits with non-zero, and does not internally infinitely reconnect. Gazebo Transport disconnect does not exit Provider, because manager still needs to report status and allow managed startup.

## 16. Build

This project is an independent exception to the `CGO_ENABLED=0` rule, and does not change other services.

Development and release builds are performed inside `Nervus-Ubuntu`:

- `GOOS=linux`, `GOARCH=amd64`, `CGO_ENABLED=1`;
- C++ standard at least C++17, based on Jetty installation requirements;
- Obtain include and link flags through existing Jetty's pkg-config/CMake metadata;
- Dynamically link existing Transport 15, Msgs 12, and their transitive dependencies;
- Do not statically package Gazebo, do not copy system `.so`.

Confirm actual metadata names and versions in the target distro before implementation, do not guess package names from source code:

```text
pkg-config --list-all | rg 'gz-(transport|msgs)'
pkg-config --modversion <transport-package>
pkg-config --modversion <msgs-package>
```

Post-build checks:

- `ldd` has no missing dependency;
- has Transport/Msgs, but no gz-sim, GUI, Rendering;
- `file`/`readelf` shows linux-x86_64 ELF;
- target dynamic library version matches the build environment;
- `!linux || !cgo` stub only returns unsupported, does not silently downgrade to shell.

Packaging, digest, signing, systemd sandbox, and dynamic library visibility will be handled separately later, no changes to `deploy/` in this phase.

## 17. Testing

### 17.1 Pure Go

- handler decoding, parameter validation, and typed error;
- manager/control registration order;
- READY registration and DEGRADED unregistration;
- all state machine transitions;
- attach never stops external processes;
- managed stops only the process it holds PID;
- deadline and native timeout;
- batch, step, string, and path boundaries;
- Profile schema version, unknown field, Descriptor closure, and runtime override boundaries;
- multi-model/multi Profile binding (using different signature Resource slot), duplicate Resource role, and single capability failure isolation;
- Adapter allowlist, fixed message type, joint limits, trajectory interpolation, and feedback convergence;
- standard motion Endpoint registers only when all prerequisite conditions are met;
- no sensor interface, Transfer stream, or generic topic publish configuration path exists;
- shutdown idempotency and goroutine leak.

### 17.2 C++ Adapter

- C ABI null pointer, length overflow, repeated close/destroy;
- Publisher cache and type conflict;
- timeout vs remote rejected distinction;
- subscription, exact unsubscribe and close race;
- queue backpressure, STATE merge and RELIABLE overflow;
- C++ exception does not cross ABI;
- AddressSanitizer/UndefinedBehaviorSanitizer;
- native data race checked separately, Go `-race` cannot cover C++;

### 17.3 Jetty Integration

Use only existing resources:

1. Start `nervus_home_nao.sdf` with existing launcher.
2. Adapter discovers `nervus_home`.
3. manager returns READY and scene model.
4. After Pause, stats paused=true.
5. After Step N, iterations advance N.
6. After Resume, stats paused=false.
7. ListModels includes `nervus_nao`.
8. ListJoints returns 24 controlled joints.
9. SetJointPosition HeadYaw=0.7.
10. Validate actual value enters tolerance when joint feedback is present.
11. Stop Gazebo, control retracts, manager retains and reports disconnected.
12. Restart Gazebo, control recovers with new endpoint ID.
13. WSLg keepalive remains, does not trigger COPY MODE.

### 17.4 Nervus End-to-End

- artifacts and manifest bidirectional closure;
- two schema hash registrations successful;
- no query permission cannot Resolve manager;
- with query but no control can only read;
- mutating methods without Lease are rejected by nervud;
- after holding Lease, Pause, Step, joint command succeed;
- standard base/arm Resource Resolve separately, and generation, Lease, epoch and sequence take effect;
- cannot Resolve corresponding standard motion Endpoint when Profile does not match or feedback is missing;
- STATE/RELIABLE event semantics are correct;
- on graceful exit, caller receives normal endpoint died reason;
- native SIGSEGV is restarted by supervisor, cannot claim process isolation internally.

## 18. Implementation Phase

### Phase A: Repository and Contract

- Establish Go module and nervus-ipc pin;
- Write manager/control proto;
- Generate Go protobuf;
- Write providergen, contract and manifest templates;
- Test artifacts, permissions, Resource and schema hash.

Completion condition: Can generate and verify deterministic Provider contract without connecting to Gazebo.

### Phase B: Transport Minimal Closed Loop

- Implement C ABI and `gz::transport::Node`;
- Implement stats, world control, scene info and joint publish;
- Implement Go `gztransport.Client`;
- Attach to existing Gazebo;
- Complete `GetStatus -> Pause/Resume/Step -> SetJointPosition -> Status`.

Completion condition: Nervus requests pass through Provider and Adapter to reach Jetty, and return verifiable results.

### Phase C: Lifecycle and Recovery

- manager/control dual endpoint;
- Transport reconnection and world generation;
- managed process controller;
- external process and managed process ownership protection;
- WSLg, abnormal exit and queue failure events.

Completion condition: Gazebo stops, crashes and restarts with consistent state, endpoint and events.

### Phase D: General Motion Profile (Task 7, `main`)

Status: Completed (2026-08-12).

- world/scene discovery does not depend on NAO or any fixed robot;
- establish versioned Profile schema, strict protojson and signature configuration boundaries;
- establish compiled-in motion Adapter registry;
- separate world readiness from motion readiness of each Resource;
- include standard BaseMotion, Manipulator Interface/schema and limited Resource slot into signature artifact;
- do not register standard Endpoint without implementing full semantic Adapter, and fail closed when error declaration is detected at runtime;
- use fixed NAO as a compatibility fixture, but do not write its name, 24 joints or topic templates as protocol constants;
- stop after completing Go, race, native and real Jetty verification, waiting for user review and branch switch.

Completion condition: world can be managed without Profile; error Profile fail closed; only motion capability with full command, feedback, stop and permission chain can Resolve.

Verification result: fixed NAO Profile still discovers and controls 24 position controllers; no Profile, error Profile, multiple models, multiple instances, Resource slot exhaustion, type mismatch and Transport generation change all have test coverage. `go test`, `go vet`, race, non-cgo, native C++/C ABI and real Jetty closed-loop all passed; command acknowledgment will not falsely report position convergence when JointStatePublisher is not provided in real world. Standard base/arm Endpoint are currently not registered, to be completed by Task 8/9's full Adapter implementation.

### Phase E: NAO Fine Manipulator (Task 8, NAO branch)

- establish NAO motion variant without breaking fixed fixture;
- left and right arms use independent Resource and unique controller ownership;
- add JointStatePublisher and controlled JointTrajectoryController;
- implement trajectory interpolation, state convergence, FK, constrained IK, Home and Stop;
- integrate with `nervus.interface.manipulator.arm`;
- five-degree-of-freedom arm does not commit to arbitrary six-dimensional pose; unreachable goals are explicitly rejected;
- fingers are tested separately under Bullet, and gripper is not declared before verification.

Completion condition: left and right arms can execute repeatable trajectories within joint limits, deadline and error threshold, end-effector/wrist actual state meets tolerance, stop and disconnection both fail safe.

### Phase F: NAO Dynamic Locomotion (Task 9, NAO branch)

- establish separate dynamic variant, retain fixed fixture;
- remove fixed joint from dynamic variant;
- use root pose, IMU and foot contact internally to achieve standing, center of mass transfer, single step, multiple steps and balance;
- implement forward, backward, turning, controlled stop, fall detection and recovery;
- register `nervus.interface.motion.base` only after real root displacement and stability verification;
- do not expose internal IMU/contact as sensor system service capability.

Completion condition: not publish `cmd_vel` or alternate leg joints, but actual, stable, stoppable bipedal displacement.

### Phase G: Packaging (Task 10)

- Design an independent cgo build path;
- Handle the visibility of dynamic libraries in the system package sandbox;
- Generate manifest digest, signature, and installation directory;
- Determine the final wiring for Windows launcher, WSL service, and Nervus supervisor.

This phase will be discussed later, and no changes will be made to `deploy/`.

### 18.1 Branching and Delivery Strategy

- `main` stores generic Motion Provider, typed Adapter, Profile schema, and stable regression fixture.
- Task 7 has been completed and passed full validation in `main`; subsequent NAO feature branches will be created by users.
- NAO's arm, finger experiments, and dynamic walking variants will first be developed in branches such as `feature/nao-motion`.
- Branches are a means of risk isolation and not a permanent robot release format. Verified general improvements, semantic Adapters, and NAO Profile will eventually return to `main`; other robots will be extended via Profile and necessary compiled-in Adapters.
- Sensor Provider is designed, signed, and managed independently, and will not be integrated into this repository or branch plan.

## 19. Acceptance Red Lines

- Code is only in this independent repository, and will not be written to `repo/nervus-system-server`.
- Use current ServiceHost and Provider artifacts, and do not add new private IPC bus.
- One Provider process, no Adapter sidecar added.
- Handler does not execute Gazebo CLI or concatenate shell.
- C++ is only exposed to Go through stable C ABI.
- Native callback does not hold Go pointer long-term.
- Do not use infinite queues.
- Do not treat publish success as joint already in place.
- Do not kill externally launched Gazebo.
- Do not remove WSLg keepalive or shared-memory workaround.
- Do not reinstall Gazebo, do not download or copy models.
- Do not claim fixed Torso NAO supports `cmd_vel` bipedal walking.
- Do not register sensor interfaces such as camera, depth, audio, radar, tactile, and general IMU.
- Do not create sensor Transfer streams, do not provide any Gazebo topic/service passthrough.
- Internal motion feedback is only for closed-loop control and motion state summary, and cannot be faked as perception Resource.

## 20. Reference Basis

- `repo/nervus-system-server/WRITING-A-SERVICE.md`
- `repo/nervus-ipc/sdk/servicehost.go`
- `repo/nervus-ipc/proto/nervus/ipc/v1/provider_descriptor.proto`
- `repo/nervus-ipc/proto/com/acme/dog/v1/raw_gait.proto`
- <https://gazebosim.org/api/transport/15/classgz_1_1transport_1_1Node.html>
- <https://gazebosim.org/api/transport/15/envvars.html>
- <https://gazebosim.org/api/sim/10/classgz_1_1sim_1_1Server.html>
- <https://gazebosim.org/api/sim/10/pause_run_simulation.html>
- <https://gazebosim.org/api/sim/10/classgz_1_1sim_1_1systems_1_1JointStatePublisher.html>
- <https://gazebosim.org/api/sim/10/classgz_1_1sim_1_1systems_1_1JointTrajectoryController.html>
- <https://gazebosim.org/api/sim/10/classgz_1_1sim_1_1systems_1_1DiffDrive.html>
- <https://pkg.go.dev/cmd/cgo>
