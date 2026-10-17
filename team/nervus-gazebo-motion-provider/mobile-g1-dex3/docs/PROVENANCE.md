# Provenance

## Unitree G1 and Dex3

- Upstream: `https://github.com/unitreerobotics/unitree_ros.git`
- Accepted commit: `daadf41ee9afce8f90fdc09a98506012691fa122`
- Source: `robots/g1_description/g1_29dof_with_hand_rev_1_0.urdf`
- Upstream license: BSD-3-Clause
- Generated mesh inventory: 35 official STL files

The generator reads the already checked-out source under `gazebo/third_party/unitree_ros`. Network
access is neither required nor used. A byte-identical copy of the upstream license is stored at
`simulation/models/nervus_mobile_g1_dex3/source/LICENSE.unitree_ros` beside the generated source
URDF. `SOURCE.json` records the accepted source commit, counts and drive parameters.

## Nervus Transformation

The generated variant retains the official G1 waist, torso, head, two seven-DoF arms and two
seven-DoF Dex3 hands. It removes the pelvis and legs and adds original Nervus geometry for a heavy
differential-drive base, lift column, wheels and low-friction supports. This is a mobile
manipulation fixture, not a representation of a production Unitree mobile base.

The transformation also:

- preserves the retained Unitree visual meshes, inertias and joint effort/position limits;
- replaces moving Dex3 phalanx mesh collisions with bounded boxes for stable DART contacts;
- adds bounded Gazebo position controllers, joint-state publication, DiffDrive and 3D odometry;
- adds Nervus-authored base, grasp and bimanual acceptance worlds and payload geometry.

The source URDF and generated SDF are reproducible outputs of `scripts/prepare-model.ps1`. The
official source archive under `third_party` remains untouched.

## Runtime Dependencies

Gazebo Jetty, Gazebo Sim 10, Gazebo Transport 15, Gazebo Msgs 12, DART and their transitive shared
libraries are system dependencies and are not vendored by this variant. The generic Provider's
reduced Gazebo Msgs protobuf schema is covered by its `THIRD_PARTY_NOTICES.md` and Apache-2.0.

Before redistribution, the package owner must retain the Unitree BSD-3-Clause notice and the
generic Provider third-party notices, and must apply the parent Nervus repository's license to
Nervus-authored code and generated base/world assets. This directory does not invent or override
the parent project's license.
