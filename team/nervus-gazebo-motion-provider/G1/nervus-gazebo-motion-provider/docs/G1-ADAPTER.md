# Unitree G1 fixed-pelvis adapter

## Selected robot

The selected source is Unitree Robotics `g1_29dof_mode_15`, machine mode 15. It has 29 actuated
joints: twelve in the legs, three in the waist, seven in the left arm, and seven in the right arm.
The rubber hands are fixed geometry and are not grippers.

The Profile joint order is legs, waist, left arm, then right arm. Within each arm the order is:

```text
shoulder_pitch
shoulder_roll
shoulder_yaw
elbow
wrist_roll
wrist_pitch
wrist_yaw
```

All limits come from the selected official URDF. Arm and wrist Profile tolerance is 0.005 rad; the
real Jetty dual-arm closed-loop acceptance requires every commanded arm joint to converge within
0.01 rad and remain there for 250 ms, including physics and sampling delay.

## Gazebo conversion

`prepare-unitree-g1.ps1` asks the installed Jetty `gz sdf -p` implementation to convert the URDF to
SDFormat 1.12. It then adds:

- a fixed `world` to `pelvis` joint;
- one Jetty JointPositionController for every actuated joint;
- `use_velocity_commands` position servos capped at 2 rad/s and never above the URDF limit;
- a JointStatePublisher restricted to the 29 actuated joints;
- source commit, license, and asset-count provenance.

Velocity-command mode is intentional for this fixed-base acceptance fixture: Jetty bypasses the
force PID and applies a bounded position servo, avoiding gain-dependent oscillation across the
coupled 29-joint tree. This proves exact motion command and feedback behavior, not actuator torque
fidelity. A dynamic standing or walking model needs a separately tuned and validated controller.

The URDF converter lumps fixed decorative links into their parents. The head and rubber-hand
visuals remain present; the control-chain terminal frames are `left_wrist_yaw_link` and
`right_wrist_yaw_link`. No runtime URDF conversion or remote model lookup is required.

## Implemented closed loop

The G1 variant currently supports:

```text
Nervus request
  -> ServiceHost / ControlLease validation
  -> bounded G1 Profile lookup
  -> embedded C++ Gazebo Transport adapter
  -> gz.msgs.Double joint command
  -> gz.msgs.Model joint-state feedback
  -> returned Provider state or command receipt
```

Single and batch position commands share the existing motion epoch, deadline, world-generation,
command-sequence, and joint-limit gates. Feedback availability is mandatory for this Profile.

## Deliberately unsupported

- BaseMotion and walking;
- dynamic standing or balance recovery;
- standard Manipulator MoveToPose / trajectory / GoHome operations;
- collision-aware IK and TCP convergence claims;
- grippers or dexterous hands;
- exported camera, lidar, IMU, contact, or force-torque capabilities.

These are separate acceptance boundaries. In particular, removing `nervus_fixed_pelvis` without a
validated gait and balance controller only makes the robot fall; it does not implement locomotion.
