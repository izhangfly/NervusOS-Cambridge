# Development

## Ownership

The robot-independent service lives in the sibling `nervus-gazebo-motion-provider` directory.
Mobile G1-specific model generation, signed Profile/runtime data and acceptance fixtures live in
this directory. Shared fixes go into the generic Provider first; robot-specific assumptions must
not be added to generic discovery or the native Transport ABI.

The runtime path is:

```text
Nervus IPC -> Go Provider -> semantic motion adapter -> cgo C ABI
           -> in-process C++ gz::transport::Node -> Gazebo Sim systems
```

The C++ layer transports serialized protobuf messages and reports discovery metadata. It does not
implement robot semantics, permissions, leases, process ownership or sensor APIs. The Go Profile
resolver supplies the finite allowlisted topic templates and motion limits.

## Variant Layout

- `config/`: native acceptance targets and safety thresholds.
- `profile/`: signed capability bundle and runtime instance selection.
- `scripts/prepare-model.ps1`: deterministic conversion from the pinned local Unitree checkout.
- `simulation/models/`: generated runtime model, copied meshes, source URDF and license.
- `simulation/worlds/`: base and isolated manipulation acceptance worlds.
- `scripts/`: native Transport clients and reproducible verification entry points.
- `docs/`: task history, operations, development, provenance and packaging requirements.

The Profile registers standard `BaseMotion` and the private world-scoped compatibility joint
control surface. `arm.left` and `arm.right` remain reserved Resource slots. Do not register standard
Manipulator or Gripper interfaces until their FK, IK, TCP, operation, force, feedback and stop
semantics are complete. Do not add sensor capabilities to this Profile.

## Change Workflow

1. Change the generic Provider when behavior is robot-independent.
2. Change `prepare-model.ps1` when generated geometry, controllers or worlds change.
3. Regenerate from Windows PowerShell; never edit generated `model.sdf`, source URDF or `SOURCE.json`
   as the authoritative implementation.
4. Keep runtime limits in the signed Profile no wider than the generated URDF/SDF limits.
5. Add a physical acceptance condition for every new claimed motion behavior. Publishing a command
   is not evidence of motion.
6. Run `scripts/verify-final.sh` before release.

Use `apply_patch` for source edits. Do not write implementation code under
`repo/nervus-system-server`, modify `deploy/`, download replacement models or remove the root WSLg
keepalive logic.

## Verification Layers

- Asset check: source commit, copied license, mesh/controller counts, world identities, Profile and
  runtime bindings, capability boundary and Markdown placement.
- C++: C ABI header test plus native Transport discovery, publish, request, subscribe, queue and
  shutdown behavior.
- Sanitizers: the same C++ tests under AddressSanitizer and UndefinedBehaviorSanitizer.
- Go: deterministic Provider artifact regeneration, module verification, cgo and no-cgo tests,
  race detector, vet and a production binary build.
- Jetty: model load, all 31 joint controllers, base displacement, Provider trajectory lifecycle,
  single-hand retention and bimanual mobile placement.

Test-only scripts may use Gazebo CLI for readiness and observation. Robot commands in the
Provider, single-hand acceptance and bimanual acceptance must remain native Transport calls.
