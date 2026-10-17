# Onboarding the AgileX Limo Pro ("Alien Wandering") into NervusOS

> Written after reading their repository, `Jude-robotic/Limo-Pro_Alien` (a ROS/C++ project from ADVX 2026).
> The app that came out of it is `apps/alien-wandering-app`.

**Key point:** the Limo Pro is a **mobile base**, so all three layers use the standard interface
**`nervus.interface.motion.base` (BaseMotion@1)**, the same one the GO2 dogs use. Not Manipulator, which is
for arms. BaseMotion is a velocity interface: `SetVelocity{linear_x, linear_y, angular_z}`, `Stop` and
`Get/SubscribeMotionState`.

**Their project:** an autonomous Limo that wanders, servoes on what it sees and keeps a visual and semantic
memory ("Alien Wandering").

- `limo_base` is the AgileX serial driver (`limo_protocol.h`, 460800 baud, frame `0x111`). It moves down into
  a **DimOS provider**.
- In `limo_bringup`, `WanderingMotionPolicy`, `VisualServoController` and `AckermannCommandLimiter` are **pure
  computation**. They **stay in the app**, which now outputs `setVelocity`.
- move_base, SLAM and costmap are the ROS navigation stack and are not ported in v1. `ydlidar_ros` (the lidar)
  goes to the provider; `data/*.db` (the memories) stays private to the app; vision lives in the sibling
  `AdventureX-software` project and becomes a perception source on the event bus.

Three documents, three owners, each independent:

| Document | Owner | What it covers |
|---|---|---|
| **1-APP-TEAM-Limo-to-NervusApp.md** | the Limo team's AI agent | Turn `Limo-Pro_Alien` into a NervusOS **app** (Kotlin `NervusApp`, `.nspkg`, desktop icon, UI) that calls **BaseMotion@1**, with **every direct hardware and ROS driver removed**. Includes a module-by-module split. |
| **2-NERVUSOS-add-Limo.md** | me (kernel) | Register the Limo in `nervud`: a Robot Profile binds `motion.base/main` to the DimOS provider, plus permissions, arbitration and perception sources on the event bus. Configuration, not kernel code. |
| **3-DIMOS-Limo-provider.md** | me / robots | A pure Python `coordinator-limo` provider: rewrite the serial protocol from their `limo_base/limo_protocol.h` in Python (no ROS) and drive a BASE coordinator from `twist_command`. Modelled on our PiPER bridge. |

**The split in one sentence:** the Limo team **keeps the brain and the UI and hands over the hands**. Every old
`/cmd_vel` or serial write becomes `BaseMotion.setVelocity()`, on the path app → NervusOS (arbitration) →
DimOS → Limo.

**Build order:** (3) the DimOS provider can drive the car, then (2) the kernel binds the resource, then (1) the
app calls it. The app team can work in parallel against a **mock BaseMotion service** (document 1, section 9).

The contract's source of truth: `github.com/Nervus-OS/nervus-ipc`
(`proto/nervus/interface/basemotion/v1/basemotion.proto`, generated Go and JVM; in this repository,
`src/nervus-ipc`).
