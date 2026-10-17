# Piper Ring Control — NervusOS app

Drive the AgileX **PiPER** arm with the **Zilo** smart ring (6-axis IMU), through the
full NervusOS stack. Built strictly against `nervus-app-sdk` + `nervus-ipc` and the
four-layer architecture (App → NervusOS → DimOS → robot).

```
Zilo ring (BLE, 6-axis IMU)
  └─ ring-sidecar/ring_bridge.py   [Python; Zilo SDK + IMU fusion]
        │  newline-JSON motion intents over TCP 127.0.0.1:8790
        ▼
  app/  PiperRingApp  (Kotlin NervusApp)                        ← L1 app, this repo
        │  Manipulator@1: MoveToPose / SetGripper  (Port A UDS /run/nervus/nervud.sock)
        ▼
  nervud (NervusOS)  — arbiter (HUMAN lease), permissions      ← L2, NOT on RDK yet
        │  Port B: Robot Profile → provider binding
        ▼
  DimOS  coordinator-cartesian-ik-piper  (PoseStamped → IK)    ← L3 (bridge already built)
        │  Port C: SocketCAN can0
        ▼
  PiPER 6-DOF arm + gripper                                    ← L4
```

- **Orientation** (dip the hand) → gripper tip orientation (gimbal).
- **Translation** (move the hand while flat/upright) → gripper XYZ (leaky-integrated, clamped).
- **Wave gesture** → built-in wave preset (demo).

## Layout (sibling repos required)
```
~/nervus-ipc          protobuf protocol + generated Go & JVM (io.github.nervusos.*)
~/nervus-app-sdk      Kotlin app framework (needs ~/nervus-ipc as a sibling)
~/nervus-piper-ring   THIS repo
   ├── app/                 Kotlin NervusApp (Manipulator.kt, PiperRingApp.kt)
   ├── ring-sidecar/        Python Zilo→intent bridge (ring_bridge.py)
   └── packaging/           .nspkg manifest.json
```

## Run it (on the RDK, once nervud is deployed)
```bash
# 1. Ring sidecar (needs the USB-C BT dongle + the ring paired):
PYTHONPATH=~/zilo-whisper-ring-sdk python3 ring-sidecar/ring_bridge.py --address <RING_BLE_ADDR>

# 2. The app (packaged .nspkg, launched from the desktop icon, or directly):
cd app && ./gradlew run
```

---

## ⚠️ Verify later (could NOT be verified — nervud is not on the RDK, no ring/BT dongle here)

Each item has a copy-paste command.

**A. Is NervusOS present + Port A socket up?**
```bash
ssh rdk-persist 'systemctl status nervud 2>&1 | head; ls -l /run/nervus/nervud.sock 2>&1'
```

**B. RDK Bluetooth dongle enumerates?** (needed to read the ring on the RDK)
```bash
ssh rdk-persist 'lsusb | grep -iE "bluetooth|BT"; hciconfig -a 2>&1 | head; bluetoothctl list 2>&1'
```

**C. App SDK + proto build (needs the two sibling repos):**
```bash
cd ~/nervus-app-sdk && ./gradlew build 2>&1 | tail -20        # generates JVM proto stubs
cd ~/nervus-piper-ring/app && ./gradlew build 2>&1 | tail -30 # then this app
```
- Confirm the **nervus-ipc JVM proto module coordinates** and wire them into
  `app/build.gradle.kts` (the `io.github.nervusos:...` line is a placeholder).

**D. Zilo ring pairs + streams IMU (from the machine with the dongle):**
```bash
cd ~/zilo-whisper-ring-sdk && python3 -c "import ring_sound; print('ok')"
# then follow ring_sound_use.md: scan for address → single-press → start_sensor_report → wait_sensor_data
python3 ~/nervus-piper-ring/ring-sidecar/ring_bridge.py --address <RING_BLE_ADDR>
# in another shell, confirm intents flow:
nc 127.0.0.1 8790            # should print {"t":"pose",...} lines when the ring moves
```
- **VERIFY the Zilo function names** used in `ring_bridge.py` against `ring_sound.py`
  (`RingSoundClient`, `wait_sensor_key_single_press_event`, `start_sensor_report`,
  `wait_sensor_data`, `wait_sensor_gesture_event`, `sensor_gesture_name`) and the
  `SensorDataBatch.samples[*].accel_x/gyro_x` field names + ranges.
- **Tune** `TRANS_GAIN`, `VEL_DAMP`, `TILT_GIMBAL_DEG`, axis signs, and `WORKSPACE`.

**E. AcquireControl lease** (the arbiter — HUMAN priority):
- `AcquireControl` is an IPC/envelope message (`nervus-ipc/proto/nervus/ipc/v1/envelope.proto`
  oneof #70; `AcquireControlSuccess.lease_id`). Confirm whether `nervus-app-sdk`'s
  `Component`/`NervusClient` exposes an `acquireControl(...)` call, or whether
  `resolveEndpoint(resourceRole="main")` already establishes the lease. Then thread
  `lease_id` into each call's `ExecutionContext`. (Marked `TODO(VERIFY)` in PiperRingApp.kt.)

**F. InterfaceProxy marshalling:** confirm `use<Manipulator>()` serialises the single
proto request via `.toByteArray()` and parses the proto response (check
`nervus-app-sdk/src/main/kotlin/com/nervus/sdk/runtime/InterfaceProxy.kt` +
`annotations/Method.kt`). Adjust `Manipulator.kt` signatures if it expects a different shape.

**G. Port B binding + DimOS path** (NervusOS → DimOS). The arm interface is pose-based
(`MoveToPose`), which maps to DimOS `coordinator_cartesian_command`/`cartesian_ik_task`:
```bash
ssh rdk-persist 'sudo systemctl stop piper-webctl; dimos run coordinator-cartesian-ik-piper'
# dimos status ; then publish a PoseStamped to coordinator_cartesian_command to confirm IK→can0
# dimos stop
```

**H. End-to-end**: ring → sidecar → app → nervud → DimOS → arm moves. Only possible once
A–G pass.
