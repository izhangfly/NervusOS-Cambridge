# Nervus Mobile G1 Dex3

This directory contains the Gazebo Jetty motion variant that combines a stable wheeled base with
the official Unitree G1 waist, torso, dual seven-DoF arms and two Dex3 hands.

The variant is intentionally independent from the fixed and dynamic G1 fixtures and from the
HECTOR walking regression model. It consumes only the already checked-out Unitree assets under
`gazebo/third_party/unitree_ros`; generation never downloads a model.

Task progress and acceptance requirements are tracked in `docs/TASKS.md`. The model exposes a
bounded Jetty-native differential base command and separate wheel and three-dimensional odometry
topics. The signed Provider Profile is now under `profile/`: it exposes standard differential-drive
`BaseMotion` and keeps waist, dual-arm and Dex3 position commands on the existing private joint
control surface. The standard `arm.left` and `arm.right` slots remain reserved until the
Manipulator endpoint has complete semantics; this profile does not export sensors.

Run the generic service with this variant after starting the matching world:

```bash
gazebod -world nervus_mobile_g1_dex3 \
  -profile-root /mnt/h/robot/Nervus/gazebo/system-server/mobile-g1-dex3/profile \
  -profile-bundle mobile-g1-dex3-v1.json \
  -runtime-config-root /mnt/h/robot/Nervus/gazebo/system-server/mobile-g1-dex3/profile \
  -runtime-config runtime-mobile-g1-dex3-v1.json
```

Generate the simulation assets from Windows PowerShell:

```powershell
H:\robot\Nervus\gazebo\system-server\mobile-g1-dex3\scripts\prepare-model.ps1
```

Generated assets are written under `simulation/` in this directory. The script validates the
expected official source model before replacing generated files.

Run the isolated base-motion acceptance test in Nervus-Ubuntu:

```bash
/mnt/h/robot/Nervus/gazebo/system-server/mobile-g1-dex3/scripts/verify-base-motion.sh
```

The test starts its own Gazebo Transport partition, measures real model-root displacement for
forward, reverse and turn commands, confirms stop drift, and checks three-dimensional odometry
against the root pose. The dynamic world uses Jetty's DART physics backend because its wheel
contact produces repeatable chassis traction for this model. The test does not interact with an
already running GUI world.

Run the isolated waist, dual-arm and dual-Dex3 acceptance test:

```bash
/mnt/h/robot/Nervus/gazebo/system-server/mobile-g1-dex3/scripts/verify-joint-control.sh
```

The native control definition is `config/native-joint-control.json`. The test-only batch client
uses the installed `gz.transport` Python binding directly and keeps all publishers in one process;
it does not invoke `gz topic`. For example, against an already running matching Transport
partition, command only the left arm to the bounded Probe state and then return it Home:

```bash
python3 scripts/joint_batch.py --state probe --group left_arm
python3 scripts/joint_batch.py --state home --group left_arm
```

These commands validate basic position control and feedback. They do not certify contact force,
grasping or collision behavior.

Run the isolated Provider trajectory acceptance test:

```bash
/mnt/h/robot/Nervus/gazebo/system-server/mobile-g1-dex3/scripts/verify-provider-trajectory.sh
```

This starts a server-only world in a private Transport partition and exercises the Go Provider's
bounded left-arm trajectory, in-flight cancellation, feedback-confirmed Home and coordinated
whole-machine stop. The runtime path uses the embedded native Transport adapter; the script does
not use `gz topic` or `gz service` for robot control.

Run the isolated single-left-hand rigid-object grasp acceptance test:

```bash
/mnt/h/robot/Nervus/gazebo/system-server/mobile-g1-dex3/scripts/verify-single-hand-grasp.sh
```

The test uses the separate `nervus_mobile_g1_dex3_grasp.sdf` DART world. It verifies real thumb and
opposing-finger contact, lift, a continuous ten-second hold, return to the support table, opening
and separation. Joint commands and observations use one persistent native Gazebo Transport Python
node; the contact topic is test-only and is not a Motion Provider sensor capability. Passing this
fixture proves one configured rigid-block grasp, not general grasp planning, cloth handling or the
standard Nervus Gripper / Manipulator contracts.

Run the isolated bimanual mobile-manipulation acceptance test:

```bash
/mnt/h/robot/Nervus/gazebo/system-server/mobile-g1-dex3/scripts/verify-bimanual-mobile.sh
```

The separate `nervus_mobile_g1_dex3_bimanual.sdf` DART world provides one rigid payload with two
instrumented handles plus source and destination supports. The native Transport client requires
both hands to establish thumb and opposing-finger contact, lifts the payload, carries it forward on
the mobile base, verifies the stopped pose, places it on the destination support, opens both hands
and separates the wrists outward. Workspace guards enforce wrist separation, centerline clearance,
height agreement and a bounded base corridor. This is a repeatable rigid-payload acceptance fixture,
not a general manipulation planner or a cloth-manipulation claim; its contact observations are not
exported by the Motion Provider.

Operational and release documentation:

- `docs/TASKS.md`: completed implementation tasks and measured acceptance results.
- `docs/OPERATIONS.md`: runtime startup, acceptance and fault handling.
- `docs/DEVELOPMENT.md`: architecture, ownership boundaries and change workflow.
- `docs/PROVENANCE.md`: pinned Unitree source, license and generated transformations.
- `docs/PACKAGING.md`: runtime payload, integrity gate and installation constraints.

Run the complete release gate in Nervus-Ubuntu before packaging:

```bash
/mnt/h/robot/Nervus/gazebo/system-server/mobile-g1-dex3/scripts/verify-final.sh
```

Pass `--static-only` to omit the six isolated real Jetty sessions while iterating.
