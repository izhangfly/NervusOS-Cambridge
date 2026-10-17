# Gazebo motion system servers

This repository contains the robot-independent Nervus Gazebo Motion Provider baseline and its
validated robot variants.

```text
system-server/
├── nervus-gazebo-motion-provider/       generic baseline
├── nao/
│   └── nervus-gazebo-motion-provider/   fixed-NAO variant
└── G1/
    └── nervus-gazebo-motion-provider/   Unitree G1 mode 15 fixed-pelvis variant
```

The generic baseline defaults to `profiles/generic-v1.json`. It declares the finite signed Nervus
Resource slots but contains no default robot binding. Robot capabilities are enabled only by a
strict signed Profile and, when required, an explicit runtime instance configuration.

The NAO variant defaults to the existing fixed `nervus_nao` fixture. It supports the 24 verified
joint-position controllers and world management, but does not claim precise arm, gripper, sensor,
or walking capabilities.

The G1 variant defaults to the converted official Unitree `g1_29dof_mode_15` fixture. It discovers
29 bounded position controllers, requires joint-state feedback, and has passed a real Jetty
closed-loop test for both seven-DoF arms. Its pelvis is fixed for deterministic control acceptance;
it does not claim BaseMotion, walking, gripper, sensor, or the still-incomplete standard
Manipulator operation endpoint.

These directories currently produce alternate builds of the same Provider contract and package
ID. They must not be installed together as separate packages. A robot variant should contain only
the smallest configuration, identity metadata, and tested implementation differences from the
generic baseline. Shared fixes are implemented in the generic baseline first and then deliberately
ported to each variant; generated protobuf and Provider artifacts must never be copied by hand.

If multiple variants need to be installed simultaneously, package, permission, private Resource,
SDK, and executable identities must first be split and regenerated as a separate contract
migration.
