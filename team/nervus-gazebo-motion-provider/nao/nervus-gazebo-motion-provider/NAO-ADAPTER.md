# Fixed NAO adapter

This directory is the deployable fixed-NAO variant of the generic Nervus Gazebo Motion Provider.
It binds only the model and controllers already present in the repository; it does not modify the
NAO SDF, the Lake House world, launcher scripts, or sensor-service boundaries.

## Bound instance

- World: `nervus_home`
- Model: `nervus_nao`
- Profile: `gazebo.nao.fixed.v1`
- Command adapter: `joint_position`
- Command type: `gz.msgs.Double`
- Command topics: `/model/nervus_nao/joint/<JointName>/0/cmd_pos`
- Controlled joints: the 24 joints listed in `profiles/nao-fixed-v1.json`
- Nervus Resource: `simulation.world.main`

The runtime selection is explicit in `profiles/runtime-nao-fixed-v1.json`; discovery of another
model does not grant it NAO capabilities.

## Deliberately unsupported

- `nervus.interface.manipulator.arm`
- `nervus.interface.motion.base`
- walking or `cmd_vel`
- finger or gripper control
- convergence claims without joint-state feedback
- camera, depth, audio, IMU, lidar, contact, and other sensor endpoints

The fixed NAO remains useful for environment lifecycle, discovery, low-level joint command, IPC,
lease, generation, epoch, sequence, and recovery regression tests. Precise manipulation and dynamic
walking require a separate robot/model variant with the corresponding feedback and controller
semantics.
