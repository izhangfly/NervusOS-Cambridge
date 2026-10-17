# Unitree G1 motion provider tasks

This ledger contains only G1-specific work. Shared architecture and historical foundation work are
documented by the generic Provider at
`gazebo/system-server/nervus-gazebo-motion-provider/docs/SYSTEM-SERVICE-DESIGN.md`.

## G1-1: Official model import and Jetty conversion

Status: completed on 2026-08-13

- Selected Unitree's current `g1_29dof_mode_15` from official commit
  `daadf41ee9afce8f90fdc09a98506012691fa122` under BSD-3-Clause.
- Added a reproducible conversion script and copied only the 34 referenced meshes.
- Generated a standalone SDFormat 1.12 model with 29 native position controllers and mandatory
  joint-state feedback. Runtime loading needs neither ROS nor URDF conversion.
- Selected Jetty velocity-command position mode with a 2 rad/s cap for stable fixed-base precision;
  this fixture does not claim torque fidelity.
- Added a dedicated `nervus_g1` world using Bullet Featherstone at 2 ms steps.
- Validated that the real Gazebo Sim 10.5.0 server loads the model, all meshes, the physics engine,
  JointStatePublisher, and all 29 controller subscribers.

## G1-2: Fixed-pelvis Provider closed loop

Status: completed on 2026-08-13

- Created the `G1/nervus-gazebo-motion-provider` alternative build.
- Added strict `g1-fixed-v1.json` and explicit `runtime-g1-fixed-v1.json` defaults.
- Kept all 29 official position limits and required `gz.msgs.Model` joint-state feedback.
- Verified Pause, Step, atomic 14-joint dual-arm publication, feedback availability, and convergence
  of both seven-DoF arms to within 0.01 rad for 250 ms through the embedded C++ Transport adapter.
- Kept pelvis fixed to isolate joint-control acceptance from dynamic balance and gait failures.
- Did not claim BaseMotion, walking, gripper, sensors, or the incomplete standard Manipulator
  endpoint.

## G1-3: Standard Manipulator endpoint

Status: pending

- Register separate `arm.left` and `arm.right` Resources only after every experimental
  `nervus.interface.manipulator.arm@1` method has a complete handler.
- Implement ordered joint state, FK, explicit-frame TCP pose, constrained IK, collision checks,
  bounded time-parameterized trajectories, Stop, GoHome, cancellation, and operation reporting.
- Validate actual TCP pose and joint feedback before completing an operation. Publishing a command
  or reaching the end of its scheduled time is not convergence.
- Keep each joint bound to exactly one active command source so position and trajectory controllers
  cannot compete.
- Preserve ControlLease, Safety, motion epoch, deadline, generation, and command-sequence gates.

## G1-4: Dynamic standing and walking

Status: pending

- Build a separate dynamic model and world; do not remove `nervus_fixed_pelvis` from the accepted
  fixed fixture.
- Add motion-internal foot contact, body pose, IMU, support-polygon, and center-of-mass feedback.
- Validate stable stand, weight shift, single step, forward/backward multi-step motion, turns,
  controlled stop, fall detection, and recovery before registering BaseMotion.
- Do not treat `cmd_vel`, alternating leg joints, or root-pose teleportation as humanoid walking.

## G1-5: Hands and sensors

Status: deferred to separate capability providers

- The mode 15 rubber hands are fixed geometry and expose no gripper capability.
- Dexterous-hand variants require their own license, collision, coupling, contact, and Resource
  review after arm-only Manipulator control is complete.
- Camera, depth, lidar, IMU, and general sensor delivery remain outside this Motion Provider.
