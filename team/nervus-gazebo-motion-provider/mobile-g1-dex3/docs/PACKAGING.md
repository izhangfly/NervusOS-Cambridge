# Packaging Requirements

Task 9 defines the package inputs and release gate; it deliberately does not modify `deploy/` or
install a system service. Service installation and upgrade policy require a separate packaging
decision.

## Target

- OS/ABI: Linux x86-64 inside `Nervus-Ubuntu` on WSL2.
- Runtime: one always-on `gazebod` process with the C++ Adapter embedded through cgo.
- Gazebo: Jetty / Sim 10 with Transport 15, Msgs 12 and DART supplied by the system.
- Process mode: `attach` by default. Managed mode is a separate operational choice.

Do not compile absolute workspace paths into the binary or Profile. Package configuration must
provide the logical world, Profile root, runtime config root, Gazebo resource path and Transport
partition at launch.

## Runtime Payload

The package assembly step must include:

```text
bin/gazebod
manifest.json
provider/provider.binpb
provider/schemas.binpb
profiles/mobile-g1-dex3-v1.json
profiles/runtime-mobile-g1-dex3-v1.json
gazebo/models/nervus_mobile_g1_dex3/model.config
gazebo/models/nervus_mobile_g1_dex3/model.sdf
gazebo/models/nervus_mobile_g1_dex3/meshes/*.STL
gazebo/models/nervus_mobile_g1_dex3/source/LICENSE.unitree_ros
THIRD_PARTY_NOTICES.md
```

The base world may be installed when the package owns its simulation fixture. Grasp and bimanual
worlds, Python clients and test configurations are validation assets and should be placed in a
separate test payload unless operators explicitly need them.

The generic manifest and Provider descriptor contain reserved arm Resource/interface declarations,
but this Profile registers only standard BaseMotion and private compatibility joint motion. A
packager must not turn reserved slots into advertised runtime Manipulator or Gripper capabilities.

## Build and Integrity

Build `gazebod` in `Nervus-Ubuntu` with cgo enabled and `-trimpath`. The resulting executable is
dynamically linked to the installed Gazebo libraries; record `ldd` output and the Jetty version in
release metadata. Do not bundle libraries from an unrelated Gazebo release.

Before assembly:

```bash
/mnt/h/robot/Nervus/gazebo/system-server/mobile-g1-dex3/scripts/verify-final.sh
```

Generate SHA-256 digests only after the final directory layout is fixed, then populate the package
manifest using the approved Nervus packaging tool. Do not hand-edit Provider protobuf artifacts or
manifest digests.

## Installation Constraints

The generic, NAO, fixed G1 and Mobile G1 trees currently share the package ID, service ID, IPC
interfaces and executable name. They are alternative builds and must not be installed side by side.
Supporting simultaneous variants requires a deliberate contract and package identity migration.

The installer must preserve the existing root Gazebo launcher, optimized residential assets and
WSLg keepalive behavior. It must not copy the complete Unitree repository or redownload Gazebo
models. Uninstall must remove only files recorded as owned by this package.

Redistribution is blocked until the package owner selects the parent Nervus license for original
code/assets and includes it with the Unitree BSD-3-Clause license and Provider third-party notices.
