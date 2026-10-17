# Operations

## Runtime

The supported runtime is `Nervus-Ubuntu` on WSL2 with Gazebo Jetty, Gazebo Sim 10 and DART
already installed. This variant does not install Gazebo or download models. Keep `GZ_IP` set to
`127.0.0.1`; every Gazebo process and Provider process in one session must use the same
`GZ_PARTITION`.

Generate the model after changing the source URDF transformation or generated world templates:

```powershell
H:\robot\Nervus\gazebo\system-server\mobile-g1-dex3\scripts\prepare-model.ps1
```

The generator accepts only the pinned local Unitree checkout. It fails before overwriting a usable
model if the source commit, URDF, referenced mesh inventory or license is missing.

For an interactive WSL session, start the base world with:

```bash
variant=/mnt/h/robot/Nervus/gazebo/system-server/mobile-g1-dex3
export GZ_IP=127.0.0.1
export GZ_PARTITION=nervus_mobile_g1
export GZ_SIM_RESOURCE_PATH="$variant/simulation/models"
export SDF_PATH="$variant/simulation/models"
gz sim -r "$variant/simulation/worlds/nervus_mobile_g1_dex3.sdf"
```

Use the existing root Gazebo launcher when its WSLg keepalive and shared-memory workaround are
required. Do not terminate `NervusWSLgKeepalive` or remove the launcher workaround.

Start the Provider in a second shell with the same partition:

```bash
export GZ_IP=127.0.0.1
export GZ_PARTITION=nervus_mobile_g1
cd /mnt/h/robot/Nervus/gazebo/system-server/nervus-gazebo-motion-provider
./gazebod -mode attach -world nervus_mobile_g1_dex3 \
  -profile-root ../mobile-g1-dex3/profile \
  -profile-bundle mobile-g1-dex3-v1.json \
  -runtime-config-root ../mobile-g1-dex3/profile \
  -runtime-config runtime-mobile-g1-dex3-v1.json
```

`attach` mode never owns or stops the external Gazebo process. Production joint, base, trajectory
and stop commands pass through the in-process C++ Gazebo Transport Adapter. The Provider does not
shell out to `gz topic` or `gz service` for control.

## Acceptance

Run the complete release gate in `Nervus-Ubuntu`:

```bash
/mnt/h/robot/Nervus/gazebo/system-server/mobile-g1-dex3/scripts/verify-final.sh
```

Use `--static-only` while iterating on code or documentation. The default additionally launches
six isolated server-only Gazebo sessions for Provider trajectory, load, joint, base, single-hand
grasp and bimanual carry acceptance. It does not connect to or control an existing GUI session.

## Faults

- `command_topic_missing`: confirm the model finished loading and the Provider uses the same
  partition. The Provider regression explicitly waits for command subscribers before attachment.
- No GUI: use the maintained root launcher and inspect the existing Weston log checks. Do not add
  a second WSLg workaround to this variant.
- No base traction: confirm the world loaded DART. Bullet Featherstone is not accepted for this
  mobile fixture.
- Joint motion continues after a request: issue the Provider's coordinated stop operation. Killing
  an attached Provider does not own or stop Gazebo; use the existing Gazebo launcher lifecycle.
- Grasp regression fails: run only the isolated grasp fixture and retain its temporary output with
  `KEEP_RESULTS=1`. Do not tune the residential world to hide a fixture failure.

This service exposes motion only. Cameras, depth, audio, lidar, generic IMU and contact streams
belong to separate Nervus sensor Providers.
