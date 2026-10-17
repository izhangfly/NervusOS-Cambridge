# Limo Pro "Alien Wandering" as a NervusOS app: integration spec
### For the Limo team's AI coding agent

> **Goal:** turn your **`Limo-Pro_Alien` (Alien Wandering)** project into a **top-level (L1) app** on
> NervusOS: it has a user interface, **shows an icon on the NervusOS desktop and opens with a click**. The app
> calls the standard interface **BaseMotion@1 (base motion)** through **nervus-app-sdk**; the **NervusOS kernel
> arbitrates** and routes the calls to **DimOS**, which drives the Limo Pro. **The app no longer opens the serial
> port, no longer runs the `limo_base`/`ydlidar` ROS drivers and no longer publishes `/cmd_vel`.**

---

## 1. What you have today (I read your repository)
`Limo-Pro_Alien` is a catkin/C++/ROS project:
- **`limo_base`**: the AgileX base serial driver (`limo_driver.cpp`, `serial_port.cpp`, `limo_protocol.h`;
  460800 baud, frame header `0x55`, motion command `0x111`, state `0x211/0x221`, odometry `0x311`). It
  subscribes to `/cmd_vel` and publishes `/odom`, `/imu` and `/limo_status`. Supports differential, tracked,
  Ackermann and Mecanum steering.
- **`limo_bringup`**: the behaviour brain. `WanderingMotionPolicy` (wandering), `VisualServoController` (visual
  servoing) and `AckermannCommandLimiter` (Ackermann limits) are **pure computation**: laser or target in,
  `{linear, angular}` out, with no dependency on ROS messages. Plus the move_base / cartographer / gmapping /
  amcl / costmap navigation stack and its launch and param files.
- **`ydlidar_ros`**: the Tmini lidar driver, publishing `/scan`.
- **`limo_description`**: the URDF and meshes.
- **`data/*.db`**: `semantic_memory.db`, `visual_memory.db` and `candidate_posts.db`, the Alien's visual and
  semantic memory and its "posts".
- **Vision is not in this repository**: it lives in the sibling `AdventureX-software`, produces **normalised
  image targets** (`center_x` in [0, 1], `area_ratio`), is managed by `alien_wandering.sh` and feeds the visual
  servo.

## 2. The one rule
In NervusOS, **hardware belongs to the provider (DimOS)**. Your app is only a **client** that sends **intent**
through the kernel. You call `BaseMotion.setVelocity(...)`; the kernel arbitrates (who controls the base right
now: SAFETY > HUMAN > AI) and forwards the call to the DimOS driver. Everywhere you write `/cmd_vel` or the
serial port today, write `setVelocity(linear_x, linear_y, angular_z)` instead.

## 3. The stack
```
L1  your app  (Kotlin NervusApp, .nspkg, desktop icon)       <- this layer is yours
      |  Port A: nervus-ipc over UDS /run/nervus/nervud.sock (the SDK hides it)
L2  NervusOS (nervud): arbitration, permissions, routing
      |  Port B: Robot Profile -> provider binding
L3  DimOS: the Limo provider (ours, see document 3; pure Python, no ROS)
      |  Port C: serial at 460800 to the Limo base
L4  AgileX Limo Pro
```
Priority is **SAFETY(3) > HUMAN(2) > AI(1) > NONE(0)**. See the architecture diagram
`NervusOS-Architecture.html` (ask the coordinator for it).

## 4. The split, module by module
| Your module | What it is | Where it goes |
|---|---|---|
| `limo_base` (serial driver + `limo_protocol.h`) | /cmd_vel to serial; publishes /odom /imu /status | **Deleted, moved down into the DimOS provider** (document 3 rewrites your `limo_protocol.h` in Python) |
| `WanderingMotionPolicy` / `VisualServoController` / `AckermannCommandLimiter` | **Pure computation** (laser or target to linear/angular) | **Stays in the app**: port the three classes to Kotlin (same logic) and send the output to `BaseMotion.setVelocity` |
| move_base / cartographer / gmapping / amcl / costmap (navigation) | ROS navigation | **Not ported in v1**: the app starts with pure behaviours (wandering, visual servo) that emit velocities; SLAM and navigation move into DimOS later |
| `ydlidar_ros` (Tmini) | laser to /scan | **DimOS provider** (a sensor source for obstacle gating and navigation) |
| `limo_description` (URDF, meshes) | the model | DimOS / simulation; the app does not need it |
| `data/*.db` (semantic and visual memory, posts) | the Alien's memory and output | **Stays in the app** (private app storage) |
| Vision (sibling `AdventureX-software`) | camera + detection to normalised targets | **A perception source**: registered on the NervusOS event bus (a service or app that publishes targets), which your app subscribes to |
| `alien_wandering.sh` + systemd | orchestration | Replaced by the NervusOS app lifecycle and the Launcher |

In one sentence: **the brain and the UI stay, the hands go to DimOS**. Your wandering and visual-servo
algorithms stay almost unchanged; only the I/O boundary moves.

## 5. The contract you consume: `BaseMotion@1`
`interface_id = nervus.interface.motion.base` (major 1, experimental).
Proto: `nervus-ipc/proto/nervus/interface/basemotion/v1/basemotion.proto`; JVM classes:
`io.github.nervusos.iface.basemotion.v1.*`.
Permission **`nervus.permission.motion.control`**; resource **`nervus.resource.motion.base`** (role `main`).

| method_id | Method | Request | Response |
|---|---|---|---|
| 1 | **SetVelocity** | `SetVelocityRequest{ double linear_x; double linear_y; double angular_z }` | none (fire and forget) |
| 2 | **Stop** | `StopRequest{}` (a controlled stop, **not** a safety e-stop) | none |
| 3 | GetMotionState | `GetMotionStateRequest{}` | `MotionState` |
| 4 | SubscribeMotionState | `SubscribeMotionStateRequest{}` | `MotionState` (stream) |

- **Units and frame** (body frame): `linear_x` in m/s, forward positive; `linear_y` in m/s, left positive (not
  supported by differential or Ackermann steering: `supports_linear_y=false`); `angular_z` in rad/s,
  counter-clockwise positive. These match the `linear.x` / `angular.z` of your current `/cmd_vel` (only Mecanum
  uses `linear.y`).
- `MotionState{ Readiness readiness; bool supports_linear_x/y/z; VelocityLimits{max_linear,max_angular}; uint64 active_motion_epoch }`.
  **The UI must clamp its input to `velocity_limits`**, and must not send commands while `readiness` is not READY.
- **Control lease (arbitration):** acquire a **HUMAN** lease before commanding (`AcquireControl`, `envelope.proto`
  oneof #70, which returns a `lease_id`). **Do not fill in sequence, deadline, epoch or lease yourself**: the
  kernel attaches them through the ExecutionContext (NRCP section 10.3). Item E of the checklist covers the SDK
  call.

## 6. Code skeleton (Kotlin)
```kotlin
package com.limo.alien

import com.nervus.sdk.component.*
import com.nervus.sdk.annotations.Interface
import com.nervus.sdk.annotations.Method
import io.github.nervusos.iface.basemotion.v1.*
import kotlinx.coroutines.*

@Interface(id = "nervus.interface.motion.base")
interface BaseMotion {
    @Method(id = 1) suspend fun setVelocity(req: SetVelocityRequest)   // fire-and-forget
    @Method(id = 2) suspend fun stop(req: StopRequest)
    @Method(id = 3) suspend fun getMotionState(req: GetMotionStateRequest): MotionState
}

class AlienWanderingApp(
    config: ComponentConfig = ComponentConfig(componentId = "alien_wandering")
) : NervusApp(config) {

    override val requiredInterfaces = listOf(
        InterfaceRequirement("nervus.interface.motion.base", 1, 1,
            resourceType = "nervus.resource.motion.base", resourceRole = "main"))

    private val scope = CoroutineScope(SupervisorJob() + Dispatchers.IO)
    // Your pure-computation classes, ported to Kotlin with the same logic:
    private val wander = WanderingMotionPolicy()          // scan -> {linear, angular}
    private val servo  = VisualServoController()          // (center_x, area_ratio) -> {linear, angular}
    private val limiter = AckermannCommandLimiter()       // clamp to Ackermann limits

    override fun onReady() {
        val base = use(BaseMotion::class)
        // TODO(VERIFY): acquire a HUMAN lease on nervus.resource.motion.base/main
        scope.launch {
            while (isActive) {
                // Perception now comes from the NervusOS event bus (section 7), not from ROS topics:
                val scan = Perception.latestScan()             // laser (the DimOS lidar source)
                val target = Perception.latestVisualTarget()   // (center_x, area_ratio) or null (vision source)
                var cmd = if (target != null) servo.commandForTarget(target.centerX, target.areaRatio)
                          else wander.command(scan)
                cmd = limiter.limit(cmd)                       // your existing Ackermann limiter
                base.setVelocity(SetVelocityRequest.newBuilder()
                    .setLinearX(cmd.linear).setAngularZ(cmd.angular).build())
                delay(50)                                      // 20 Hz
            }
        }
        // TODO: Compose Desktop UI (com.nervus.sdk.ui): wander on/off, live camera + target, map,
        // memory and posts browser, STOP
    }
    override fun onStop() { scope.cancel() }
}
```
See the same kind of sample we built for the arm, `nervus-piper-ring` (`PiperRingApp` uses `Manipulator@1`;
you use `BaseMotion@1`).

## 7. Perception input (vision and laser)
Your ROS topics (`/scan` and the vision process's normalised targets) become:
- **Laser, odometry, state:** from the provider. Use `SubscribeMotionState` for motion state; if the app needs
  the laser for gating, DimOS publishes `/scan` on the **event bus** (something like `event.perception.scan`) and
  the app subscribes with permission.
- **Visual targets:** turn the vision process in the sibling `AdventureX-software` into a **NervusOS perception
  source** (a service or app) that publishes normalised targets on the event bus (`event.perception.target`:
  `center_x, area_ratio`). Your app subscribes and feeds them to `VisualServoController`.
- Your **semantic and visual memory and posts (`data/*.db`) stay inside the app** and never go into the kernel.

(Apps subscribe to events with `subscribe(interfaceClass, eventId, payload)`; see `NervusApp.subscribe` in the
app SDK.)

## 8. Packaging as a desktop icon (`.nspkg`)
```json
{
  "package_id": "com.limo.alien",
  "version": "0.1.0",
  "name": "Alien Wandering — Limo Pro",
  "icon": "assets/icon.png",
  "components": [{ "id": "alien_wandering", "type": "app",
    "entry": "lib/alien-wandering.jar", "runtime": "jvm",
    "launch_mode": "manual", "interfaces": ["nervus.interface.motion.base"] }],
  "permissions": ["nervus.permission.motion.control", "nervus.permission.perception.read"]
}
```
`type: app` means the component extends `NervusApp`; the Launcher shows it as an **icon** that starts it on a
click. The manifest schema in `nervud/internal/pkgregistry/manifest.go` is authoritative.

## 9. Developing without the robot (mock)
NervusOS and the DimOS Limo provider are not running yet. Write a **mock BaseMotion service** first (a
`NervusService` implementing BaseMotion@1 whose `setVelocity` only logs), run the mock and your app on one
machine (both connected to the local `nervud`), and check that your UI and behaviours produce the right velocity
stream. See `NervusService` in the app SDK's design notes on the component base classes and lifecycle.

## 10. Do and don't
- Do use `setVelocity`/`stop`, clamp to `velocity_limits`, hold a HUMAN lease, and handle `Readiness` and being
  pre-empted (losing the lease).
- Don't open the serial port, run the `limo_base`/`ydlidar` ROS nodes, or publish `/cmd_vel`.
- Don't fill in epoch, lease or deadline yourself (the kernel does).
- Don't assume `linear_y` works (check `supports_linear_y`; it is false for differential and Ackermann).

## 11. Verify-later checklist
```bash
# A. nervud and the Port A socket:   ls -l /run/nervus/nervud.sock ; systemctl status nervud | head
# B. Build (nervus-ipc must sit next to nervus-app-sdk): cd nervus-app-sdk && ./gradlew build && cd ../<app> && ./gradlew build
# C. The generated JVM classes exist: find nervus-ipc/jvm -path "*basemotion*" -name "*.java" | head
# D. The mock BaseMotion service and the app connect to nervud, and setVelocity shows up in the log
# E. AcquireControl in the SDK:       read nervus-app-sdk .../ipc/NervusClient.kt and envelope.proto oneof #70
# F. On the real robot: the DimOS Limo provider (document 3) must be running and the Robot Profile bound (document 2)
```

## 12. References
- App SDK: `github.com/Nervus-OS/nervus-app-sdk` (with `github.com/Nervus-OS/nervus-ipc` next to it)
- Kernel: `github.com/Nervus-OS/nervud`; provider: `github.com/dimensionalOS/dimos`
- Your base protocol (which we need to build the provider): `limo_base/include/limo_protocol.h` and
  `limo_driver.cpp` in your repository
- Board: `d-robotics.github.io/rdk_doc/en/rdk_s/RDK`; reference app: our `nervus-piper-ring`
