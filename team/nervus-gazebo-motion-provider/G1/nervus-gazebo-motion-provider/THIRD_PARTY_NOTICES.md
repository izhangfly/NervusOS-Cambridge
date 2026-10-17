# Third-Party Notices

## Gazebo Msgs protobuf wire schema

`proto/gz/msgs/nervus_subset.proto` and its generated Go output contain a reduced,
wire-compatible subset of message definitions from `gazebosim/gz-msgs`.

- Upstream: https://github.com/gazebosim/gz-msgs
- Copyright: Open Source Robotics Foundation
- License: Apache License 2.0

The installed Gazebo Msgs 12 definitions under `/usr/share/gz/gz-msgs/protos` are the version
reference used by this repository. The subset omits fields that the Nervus Gazebo Motion Provider
does not inspect; protobuf unknown-field compatibility preserves interoperability with the full
upstream messages. Inclusion of a wire schema does not declare a Nervus sensor capability.

## Unitree G1 model

- Upstream: https://github.com/unitreerobotics/unitree_ros
- Pinned commit: `daadf41ee9afce8f90fdc09a98506012691fa122`
- Source model: `robots/g1_description/g1_29dof_mode_15.urdf`
- Runtime adaptation: `H:\robot\Nervus\gazebo\models\unitree_g1`
- License: BSD-3-Clause

The official source URDF, the 34 meshes referenced by that variant, and the upstream license are
retained with the converted model. Nervus adds the Jetty SDF conversion, fixed-pelvis joint,
position-controller configuration, and joint-state publisher; it does not claim ownership of the
Unitree geometry or robot description.
