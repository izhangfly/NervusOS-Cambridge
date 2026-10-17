# Onboarding the Limo Pro: the **DimOS (L3) and robot (L4)** layers
### For me and the robot team: the provider that actually drives the Limo

> **Summary:** DimOS is the **provider**. It owns the hardware and implements the **BaseMotion** capability that
> NervusOS routes to it (Port B). DimOS is **pure Python and runs no ROS**, so the serial protocol in the Limo
> team's `limo_base` (`limo_protocol.h`) is **rewritten in Python** here, instead of running `limo_ros`. The
> approach is the same as the PiPER bridge we already wrote
> (`~/NervusOS/robots/vendor/dimos/.../piper/blueprints/nervus_bridge.py` and
> `.../teleop/http/http_teleop_module.py`).

Provider repository: `github.com/dimensionalOS/dimos` (on the RDK at `~/NervusOS/robots/vendor/dimos`, venv
`.venv`, `dimos` on the PATH).

---

## What to build: a `coordinator-limo` blueprint
DimOS already has everything a mobile base needs:
- `HardwareType.BASE` and the coordinator's **`twist_command: In[Twist]`** port, mapped onto the base by
  **`_map_twist_to_base_joints`** (see `dimos/control/coordinator.py`).
- The **`unitree-go2`** provider is the reference mobile-base implementation (single, `-multi`, `-fleet`,
  teleop). Write the Limo after it.
- SLAM, costmap, A* and path-follower tasks already exist (for navigation with the Limo's lidar later).

So the Limo provider is **a BASE coordinator whose BASE hardware is a `LimoAdapter`**, driven by
`twist_command`. NervusOS `BaseMotion.SetVelocity` goes over Port B to this coordinator's `twist_command`.

### Files to add (shaped like the PiPER bridge)
```
dimos/hardware/bases/limo/adapter.py        # LimoAdapter: serial driver, Twist -> Limo velocity frame
dimos/hardware/bases/limo/limo_protocol.py  # frame parsing and packing, ported from their limo_protocol.h
dimos/hardware/bases/limo/config.py         # serial port, baud, steering mode, wheelbase, speed limits
dimos/robot/bases/limo/blueprints/basic.py  # coordinator_limo (+ _mock)
# register in dimos/robot/all_blueprints.py:  "coordinator-limo": "...:coordinator_limo"
```

## 1. `LimoAdapter`: a pure Python serial driver (**ported from their `limo_protocol.h`**)
The AgileX Limo Pro talks serial at **460800 baud** (`/dev/ttyTHS*` or a USB serial adapter). **The source is the
Limo team's `limo_base/`**: `limo_protocol.h`, `limo_driver.cpp` and `serial_port.cpp`. Port their frame format
to Python:
```
header  FRAME_HEADER = 0x55, FRAME_LENGTH = 0x0E
control:   MSG_MOTION_COMMAND_ID = 0x111        # write: linear velocity / angular velocity (or steering angle) / mode
state:     MSG_SYSTEM_STATE_ID   = 0x211        # vehicle state, control mode, voltage, error code
           MSG_MOTION_STATE_ID   = 0x221        # actual linear/angular velocity, current motion mode
odometry:  MSG_ODOMETRY_ID       = 0x311
actuators: MSG_ACTUATOR{1..4}_{HS,LS}_STATE_ID  # 0x251.. / 0x261..
```
```python
class LimoAdapter:
    """DimOS BASE adapter for the AgileX Limo Pro. Body-frame Twist -> 0x111 motion command frame.
    The steering mode (diff/track/ackermann/mecanum) is set at init. Protocol: their limo_protocol.h."""
    hardware_type = HardwareType.BASE
    def __init__(self, port="/dev/ttyTHS1", baud=460800, mode="diff"): ...
    def connect(self):  ...                      # open the port, wait for 0x221 to report the mode, enable
    def set_twist(self, vx, vy, wz):             # m/s, m/s, rad/s -> pack a 0x111 frame and write it
        ...                                      # REFUSE commands while the motion mode is unknown (as their driver does)
    def read_state(self) -> dict:                # parse 0x211/0x221/0x311 -> velocity, odometry, voltage, errors, mode
        ...
    def stop(self): self.set_twist(0, 0, 0)
```
Kinematics by mode: **diff/track** uses vx and wz (vy is ignored, so `supports_linear_y=false`);
**ackermann** uses vx plus a front-wheel angle derived from wz, limited to the minimum turning radius (the job of
their `AckermannCommandLimiter`); **mecanum** supports vx, vy and wz (their launch parameter is
`use_mcnamu:=true`).

## 2. The coordinator blueprint (`coordinator_limo`)
```python
_limo_hw = make_limo_hardware("base", port="/dev/ttyTHS1", mode="diff")   # BASE type
coordinator_limo = autoconnect(
    ControlCoordinator.blueprint(tick_rate=100.0, hardware=[_limo_hw],
        tasks=[ velocity_task(_limo_hw) ]),      # consumes twist_command -> the base
)
# + coordinator_limo_mock (adapter_type="mock") to check the pipeline with no hardware, like coordinator-piper-bridge-mock
```

## 3. Mapping BaseMotion@1 to DimOS
| BaseMotion@1 | DimOS |
|---|---|
| `SetVelocity{linear_x, linear_y, angular_z}` | send `Twist(linear=[x,y,0], angular=[0,0,z])` to `twist_command` |
| `Stop{}` | send a zero `Twist` |
| `GetMotionState` / `SubscribeMotionState` | `LimoAdapter.read_state()` -> `MotionState{readiness, supports_*, velocity_limits, active_motion_epoch}` |
- Differential and Ackermann report `supports_linear_y=false`; `velocity_limits` comes from the config (the app's
  UI clamps to it).
- `Readiness`: serial up and no fault is READY; connecting is NOT_READY; serial lost or e-stop is FAULT.
- `active_motion_epoch` must echo the epoch the kernel sent, so the arbiter can catch stale commands after a
  pre-emption.

## 4. Perception and navigation (later)
The Limo Pro carries a **Tmini lidar and a depth camera**. Wire them in as DimOS sensor sources to reuse
**SLAM, costmap, A* and path-follower** (DimOS has them; the GO2 provider uses them). For the Limo team's laser
gating and obstacle avoidance (the `/scan` that `WanderingMotionPolicy` reads), DimOS can publish the laser on
the event bus for the app to subscribe to (document 1, section 7). **v1 does teleop and velocity only**;
navigation moves down later.

## 5. Run
```bash
dimos run coordinator-limo-mock         # mock first: check the twist -> base pipeline; dimos status; dimos stop
ls /dev/ttyTHS* /dev/ttyUSB*            # find the Limo's serial port and put it in the config
dimos run coordinator-limo              # real robot: teleop or sending a Twist should move the car
```

## 6. Connecting to NervusOS (Port B, with kernel document 2)
- nervud's Robot Profile binds `nervus.resource.motion.base/main` to `blueprint: coordinator-limo` (document 2).
  Find out **how nervud starts or reaches** the DimOS provider (does it launch the blueprint, or connect to a
  running coordinator over LCM or zenoh?); both sides must use the same transport.

## 7. Don't
- Don't run `limo_ros` or any ROS on the robot: DimOS is pure Python and talks serial directly.
- Don't let two owners control the base at once (a teleop script and the coordinator): one owner, as with can0.
- Don't hard-code speed limits in the app: `MotionState` carries them from the config.

## 8. Templates to copy (already on the RDK)
- `~/NervusOS/robots/vendor/dimos/dimos/robot/manipulators/piper/blueprints/nervus_bridge.py`
- `~/NervusOS/robots/vendor/dimos/dimos/teleop/http/http_teleop_module.py`
- `dimos/control/coordinator.py` (`twist_command`, `_map_twist_to_base_joints`, `HardwareType.BASE`)
- the `unitree-go2` provider (the mobile-base reference)
- **The protocol's source of truth**: the Limo team's `limo_base/` (`limo_protocol.h`, `limo_driver.cpp`,
  `serial_port.cpp`), rewritten in Python
