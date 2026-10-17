# Mobile G1 Dex3 Implementation Tasks

Complete and verify one task, then stop for review before starting the next task.

## Task 1: Independent Jetty model baseline

Status: completed on 2026-08-14

- Generate a standalone model from the locally pinned official G1 Dex3 URDF.
- Retain the three waist joints, two seven-DoF arms and two seven-DoF Dex3 hands.
- Replace the legs and pelvis with a low, heavy differential-drive-ready chassis and support
  contacts.
- Preserve physical inertias, collisions, joint limits and source provenance.
- Add bounded position holding and joint-state publication for the upper-body joints.
- Verify SDFormat validity, model inventory, headless Jetty loading and stable initial placement.

Verification:

- The generator accepted only the locally pinned Unitree source commit
  `daadf41ee9afce8f90fdc09a98506012691fa122` and copied 35 referenced meshes without network
  access.
- The generated model contains two wheel joints plus 31 upper-body joints: three waist, fourteen
  arm and fourteen Dex3 joints. No hip, knee or ankle link is present.
- Gazebo SDFormat validation passed for the model and standalone world.
- An isolated headless Jetty server loaded the model and exposed all 31 bounded upper-body
  controller topics plus the model joint-state topic.
- Two root-pose samples one second apart reported `(0, 0, 0.12)` with effectively zero roll,
  pitch, planar drift and vertical drift.
- Existing G1, HECTOR, NAO, launch, WSLg and deployment files were not modified.

## Task 2: Differential base motion

Status: completed on 2026-08-14

- Add the Jetty DiffDrive system with bounded linear and angular velocity.
- Publish three-dimensional odometry and verify forward, reverse, turn and stop behavior.
- Confirm actual root displacement instead of treating command publication as success.

Verification:

- The model uses Jetty's native `DiffDrive` system with a 0.61 m wheel separation, 0.12 m
  radius, bounded acceleration, 0.35 m/s maximum forward speed, 0.25 m/s maximum reverse speed
  and 0.60 rad/s maximum angular speed.
- The public `/model/nervus_mobile_g1_dex3/odometry` stream comes from a three-dimensional
  `OdometryPublisher`. Planar wheel integration remains independently observable on
  `/model/nervus_mobile_g1_dex3/wheel_odometry`.
- The dynamic world uses the locally installed DART physics backend. Bullet Featherstone rotated
  the wheel joints but produced no chassis traction for this model, while DART produced repeatable
  contact motion with the same geometry and commands.
- An isolated headless run measured 0.369 m forward motion, 0.224 m reverse motion and a 0.583 rad
  in-place turn. Lateral error during the forward segment was below printed precision and final
  stop drift was below 0.001 m.
- Three-dimensional odometry agreed with measured model-root planar displacement within printed
  precision. The acceptance test also rejects excess roll, pitch, vertical displacement, lateral
  drift, turn translation and post-stop motion.
- Command publication alone is not accepted: `scripts/verify-base-motion.sh` captures the actual
  model-root pose at every stage and compares odometry against it.

## Task 3: Dual-arm and dual-hand native control

Status: completed on 2026-08-14

- Verify every arm and Dex3 controller topic and joint-state entry.
- Add safe home states, joint groups, batch commands and feedback convergence tests.
- Keep hand contact tuning separate from basic joint-control acceptance.

Verification:

- `config/native-joint-control.json` defines five non-overlapping primary groups: three waist
  joints, seven joints per arm and seven joints per Dex3 hand. Their ordered union contains all 31
  upper-body joints exactly once.
- The Home state opens both hands and returns the waist, shoulders, elbows and wrists to their
  zero-position model baseline. A separate symmetric Probe state exercises every controller by at
  least 0.05 rad while remaining inside the official Unitree URDF position limits.
- `scripts/joint_batch.py` validates the complete group, state, message type, URDF limits and all
  controller connections before publishing. It uses the installed `gz.transport` Python binding
  as a single persistent native Transport node; it does not shell out to `gz topic`.
- Batch validation is all-or-nothing before publication. Individual Gazebo Transport messages are
  then sent close together, but physical updates are not claimed to be transactionally atomic.
- An isolated DART run found all 31 `gz.msgs.Double` command topics and all 31 joint-state entries.
  Every waist, arm, wrist and hand joint reached the Probe target and returned Home with maximum
  error below the six-decimal-radian reporting precision.
- The model root remained upright at 0.12 m and moved only 0.0005 m during the complete dual-arm
  and dual-hand cycle.
- This acceptance covers unconstrained position control only. Finger contact, grasp force,
  collision tuning and object retention remain explicitly deferred to Task 7.

## Task 4: BaseMotion Provider adapter

Status: completed

- Completed the generic `diff_drive` semantic adapter in the shared provider.
- Jetty publishes native `gz.msgs.Twist` to the profile command topic and decodes
  native `gz.msgs.Odometry` feedback without invoking the `gz` CLI.
- BaseMotion is registered only after exactly one signed `base.main` binding has
  command, odometry feedback and stop-confirmation readiness.
- SetVelocity, Stop, GetMotionState and SubscribeMotionState are exposed through
  the standard IPC contract. Differential drive reports no lateral velocity.
- Lease, resource generation, motion epoch, deadline and per-resource command
  sequence checks are enforced. Stop waits for bounded low-velocity feedback.
- Verified with Jetty adapter tests, Provider handler/lifecycle tests and
  `go test ./...` in Nervus-Ubuntu.

## Task 5: Mobile G1 Dex3 motion Profile

Status: completed

- Added the strict signed `gazebo.unitree.g1.dex3.mobile.v1` Profile and runtime selection.
- The Profile binds `base.main` to native differential-drive `BaseMotion` and binds the complete
  31-joint waist/arm/Dex3 private compatibility group to `simulation.world.main`.
- Separate waist, left-arm, right-arm, left-hand and right-hand groups are defined for precise
  command selection; no camera, IMU, contact or other sensor capability is exported.
- `arm.left` and `arm.right` signed standard Resource slots remain reserved, rather than being
  falsely registered before the standard Manipulator endpoint is implemented.
- Strict bundle/runtime validation covers resource/interface agreement, complete assignments,
  model matching and Gazebo command/state message types. The Profile parser and resolver test
  pass in Nervus-Ubuntu.
- Real Jetty regression found all 31 declared joint controllers and feedback entries with zero
  reported Probe/Home error. Base regression measured 0.366 m forward, 0.226 m reverse,
  0.562 rad turn, zero printed stop drift and zero printed three-dimensional odometry error.

## Task 6: Trajectory and whole-machine stop behavior

Status: completed on 2026-08-15

- Add bounded, time-parameterized arm and hand trajectories.
- Implement home, cancellation, convergence and coordinated emergency stop.
- Prevent competing command sources for any joint.

Verification:

- Added configured joint-group trajectories to the private world-scoped control endpoint without
  prematurely registering the standard Manipulator interface. Trajectory and Home requests resolve
  joint order, position limits, velocity limits, tolerance and settle time from the signed Profile.
- The Go executor interpolates targets at 20 ms intervals, rejects non-monotonic timing, position
  limit violations and excessive segment velocity, and requires actual joint feedback to remain
  within tolerance for the configured settle interval before reporting convergence.
- Joint ownership is exclusive while a trajectory is active. Direct position commands that overlap
  an active trajectory fail instead of competing with it; disjoint arm and hand groups may run
  concurrently.
- Cancel waits for the trajectory publisher to exit, republishes the measured joint positions as
  hold targets and confirms bounded near-zero joint velocity. Whole-machine stop applies the same
  behavior to every configured joint and publishes zero velocity to every bound mobile base before
  confirming odometry stop.
- The signed private control contract now contains SetJointTrajectory, GoHome, StopMotion and
  StopAllMotion, with updated protobuf, schema bundle and Provider artifacts. Production execution
  remains entirely on the in-process Gazebo Transport adapter.
- `go test ./...` passed in Nervus-Ubuntu. The isolated real Jetty regression completed a one-second
  left-arm trajectory, cancelled a second trajectory in flight, returned the arm Home and confirmed
  whole-machine stop in approximately six seconds.

## Task 7: Single-hand rigid-object grasping

Status: completed on 2026-08-15

- Replace expensive or unstable finger mesh collisions where required.
- Tune contact, friction, closing speed and force limits.
- Verify grasp, lift, ten-second hold and controlled release.

Verification:

- Preserved the official Unitree visual meshes, inertias and URDF joint effort limits while
  replacing all fourteen moving Dex3 phalanx mesh collisions with bounded boxes. Both hands use
  `5.0` contact friction and a limited `0.45 rad/s` position-controller output; the arm and base
  controller modes are unchanged.
- Added the independent DART world `nervus_mobile_g1_dex3_grasp.sdf` with a 50 g rigid block and a
  support table. The main base-motion world remains free of grasp fixtures.
- `single_hand_grasp.py` uses one persistent native `gz.transport` node for all joint commands and
  pose, joint-state and test-only contact observations. It does not invoke `gz topic` or
  `gz service` for robot control.
- Acceptance requires simultaneous thumb and opposing-finger contact, measured object lift,
  continuous wrist-relative retention for at least ten seconds, return to the support boundary,
  opening and a separate wrist-motion probe proving the object no longer follows the hand.
- The isolated real Jetty run lifted the object `0.0973 m`, retained it for ten seconds across 100
  samples with `0.0001 m` maximum absolute and wrist-relative drift, then released it with
  `0.0130 m` object drift during the separation probe while the empty wrist moved `0.0398 m`.
- The 31-joint Probe/Home regression retained zero reported joint error and stable root pose. The
  base regression measured `0.367 m` forward, `0.241 m` reverse and `0.624 rad` turn with zero
  printed stop drift and odometry error. The real Provider trajectory/cancel/Home/stop regression,
  `go test ./...`, `go test -race ./...` and `go vet ./...` also passed.
- Contact observation exists only inside this acceptance world and is not exported by the Motion
  Provider. This task does not register standard Gripper or Manipulator capabilities and does not
  claim arbitrary-object or cloth manipulation.

## Task 8: Bimanual mobile manipulation

Status: completed on 2026-08-15

- Verify dual-hand lift, carry, base movement and placement.
- Add self-collision and workspace constraints needed for repeatable acceptance.
- Do not claim cloth manipulation from rigid-object tests.

Verification:

- Added the independent DART world `nervus_mobile_g1_dex3_bimanual.sdf` with one 150 g rigid
  payload, separate left and right handle collisions and source / destination support surfaces.
  Contact sensors are test-only and remain outside the Motion Provider capability surface.
- `bimanual_mobile_manipulation.py` uses one persistent native Gazebo Transport node for all
  fourteen Dex3 joints, both shoulder lift joints, both shoulder separation joints, the mobile
  base command, joint feedback, model poses and left / right contact observations. It does not
  invoke `gz topic` or `gz service` for robot control.
- Acceptance requires thumb and opposing-finger contact from both hands on the same rigid body,
  symmetric lift, continuous dual-wrist-relative retention while the base moves, measured base
  displacement and stop drift, placement inside the destination support boundary, opening and
  symmetric outward wrist separation.
- The workspace guard rejects wrist separation below `0.220 m`, either wrist crossing the
  protected `0.080 m` centerline clearance, or wrist-height disagreement above `0.025 m`. Base
  lateral and yaw corridor bounds remain active throughout the carry.
- The isolated real Jetty run lifted the payload `0.0989 m`, moved the base `0.2029 m`, retained
  the payload with `0.0063 m` maximum wrist-relative drift and stopped with `0.0016 m` drift. Both
  hands reached Open within `0.0272 rad`; outward separation then moved each wrist `0.0349 m` while
  payload drift was below printed precision.
- Single-hand grasp, all 31 upper-body joints, base motion and the real Provider trajectory /
  cancel / Home / stop regressions passed. `go test ./...`, `go test -race ./...` and `go vet ./...`
  also passed in Nervus-Ubuntu.
- This fixture proves one configured rigid-payload workflow. It does not register standard
  Manipulator or Gripper capabilities and does not claim grasp planning, arbitrary-object,
  deformable-object or cloth manipulation.

## Task 9: Final verification and documentation

Status: completed on 2026-08-16

- Run Go, native C++, race, sanitizer and real Jetty regressions.
- Complete operator, developer, provenance and packaging requirements.
- Do not modify deployment scripts until packaging is separately approved.

Verification:

- Added a single release gate covering pinned source and license integrity, generated assets,
  JSON, Python, shell and SDF validation, native C++ and C ABI tests, AddressSanitizer and
  UndefinedBehaviorSanitizer, deterministic contract regeneration, Go cgo and no-cgo tests, the
  Go race detector, `go vet`, a trimmed production binary build and all six real Jetty regressions.
  All temporary build products are isolated under `/tmp` and removed on exit; `--static-only`
  retains the same static gate when a graphical / real-time Gazebo session is unavailable.
- Native verification passed both CTest targets: the C++ Adapter test and the pure-C header / C ABI
  test. The sanitizer build passed both tests without an ASan or UBSan report. Deterministic
  contract regeneration, `go test ./...`,
  `go test -race ./...`, no-cgo tests, `go vet ./...` and the `gazebod -trimpath` build passed.
- Hardened real-Provider startup against stale Gazebo Transport discovery records: each test uses
  an isolated partition, the Provider receives that partition explicitly, command topics must be
  visible for eight consecutive samples, and real joint-state and odometry frames must arrive
  before the Go process starts. The final orchestrator also allows discovery records to retire
  before and between real regressions. When Jetty 15 omits subscriber-only command topics from
  `TopicList` / `TopicInfo`, the native Adapter now advertises a bounded type-matched probe and
  requires `Publisher.HasConnections()` without publishing a payload; missing or incompatible
  controllers remain fail-closed.
- Python Transport acceptance clients now explicitly unsubscribe callbacks and release publishers
  before interpreter shutdown. This removed a post-success native abort without weakening any
  grasp or bimanual assertion.
- The real Provider trajectory, cancellation, Home and whole-machine Stop regression completed in
  `6.13 s`. Jetty exposed all 31 controllers plus joint-state and BaseMotion feedback;
  the 31-joint Probe/Home cycle had zero reported error and `0.0006 m` root drift.
- BaseMotion measured `0.3932 m` forward, `0.2515 m` reverse and `0.6178 rad` turn with zero printed
  stop drift and odometry error. Single-hand acceptance lifted `0.0973 m`, held for ten seconds
  with `0.0001 m` relative drift, and produced `0.0135 m` release drift.
- Bimanual acceptance lifted `0.0990 m`, moved the base `0.2025 m`, retained the payload with
  `0.0062 m` maximum wrist-relative drift, stopped with `0.0017 m` drift and released with zero
  printed payload drift; the Open state error was `0.0242 rad`.
- The complete one-shot release gate finished successfully with
  `Final verification passed: static=true real_jetty=true`.
- Completed operator, developer, provenance and packaging documentation under `docs/`. No
  deployment script was changed, and the capability boundary remains motion-only: no sensor,
  standard Manipulator, standard Gripper, arbitrary-object or cloth-manipulation claim is made.
