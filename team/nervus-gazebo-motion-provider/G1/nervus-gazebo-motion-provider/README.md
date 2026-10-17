# Nervus Gazebo Motion Provider - Unitree G1 Fixed

This directory is the Unitree G1 mode 15 variant of the robot-independent Nervus Gazebo Motion
Provider. It is managed by the Git repository rooted at `gazebo/system-server` and does not write
code into `repo/nervus-system-server`.

The variant defaults to:

- world `nervus_g1`;
- model `nervus_g1`;
- signed Profile `profiles/g1-fixed-v1.json`;
- runtime selection `profiles/runtime-g1-fixed-v1.json`;
- Provider package ID `gazebo.simulation.motion.provider`.

The Go process embeds the small C++ Gazebo Transport adapter through cgo. Runtime control uses the
native Gazebo Transport API, not `gz` command-line subprocesses, ROS, ros_gz, or Gazebo Classic
plugins.

## Current capability boundary

The converted model is Unitree's current `g1_29dof_mode_15`: 12 leg joints, 3 waist joints, and two
seven-joint arms. All 29 joints have bounded position commands and required joint-state feedback.
Commands are validated against the official URDF limits before publication.

The fixed fixture uses Jetty's velocity-command position mode, capped at 2 rad/s, for deterministic
fine joint control. This bypasses the force PID and therefore does not claim motor torque fidelity;
dynamic standing and gait need a separate controller and acceptance model.

This first acceptance model fixes `pelvis` to `world`. It is suitable for deterministic joint,
waist, wrist, and dual-arm motion tests, but it is not a walking robot and does not register
`nervus.interface.motion.base`. Camera, depth, lidar, IMU, contact, and force streams are outside
this Motion Provider.

The signed descriptor already reserves `arm.left` and `arm.right`, but this variant does not claim
the standard `nervus.interface.manipulator.arm` endpoint yet. The implemented closed loop is the
private world control interface with precise per-joint and atomic batch commands. Standard
Manipulator operations require the remaining FK/IK, collision checking, time-parameterized
trajectory, cancellation, and Operation reporting work; the service fails closed instead of
advertising those methods early.

## Model source

The runtime model is at `H:\robot\Nervus\gazebo\models\unitree_g1`. It was generated from the
official Unitree Robotics `unitree_ros` repository, commit
`daadf41ee9afce8f90fdc09a98506012691fa122`, under BSD-3-Clause. The conversion is reproducible:

```powershell
H:\robot\Nervus\gazebo\scripts\prepare-unitree-g1.ps1
```

The generated SDF is checked in separately from the sparse upstream checkout. Runtime loading does
not convert URDF and does not download meshes.

## Start Gazebo

GPU / WSLg D3D12:

```bat
H:\robot\Nervus\gazebo\scripts\start-gazebo.cmd H:\robot\Nervus\gazebo\worlds\nervus_g1.sdf
```

CPU / llvmpipe:

```bat
H:\robot\Nervus\gazebo\scripts\start-gazebo.cmd cpu H:\robot\Nervus\gazebo\worlds\nervus_g1.sdf
```

Do not remove the WSLg keepalive or shared-memory workaround from the existing launcher.

## Build and test the Provider

Run inside `Nervus-Ubuntu`:

```bash
cd /mnt/h/robot/Nervus/gazebo/system-server/G1/nervus-gazebo-motion-provider
go test ./...
go test -race ./...
go vet ./...
```

The full deterministic contract check is:

```bash
bash scripts/verify-contract.sh
```

The self-contained real Jetty test starts an isolated headless server, waits for all 29 controllers
and required feedback, tests Pause / Step and a 14-joint dual-arm command, then stops the server:

```bash
./scripts/verify-real-g1.sh
```

To attach the tagged test to an already running `nervus_g1.sdf` server on the same `GZ_PARTITION`,
run:

```bash
go test -tags jettyintegration ./internal/jetty -run TestRealJettyG1MinimumClosedLoop -v
```

## Run the system service

The compiled defaults select the G1 Profile and runtime instance, so no robot-specific flags are
required in the normal attach path:

```bash
go run ./cmd/gazebod -sock /run/nervus/nervud.sock
```

The service expects Gazebo to publish:

```text
/world/nervus_g1/model/nervus_g1/joint_state
```

and discovers all 29 command subscribers using:

```text
/model/nervus_g1/joint/<joint_name>/0/cmd_pos
```

Both G1 and the generic baseline currently build the same Provider package identity. They are
alternative package builds and cannot be installed simultaneously without a separate contract and
package identity migration.

Additional architecture and G1-specific details are under `docs/`.
