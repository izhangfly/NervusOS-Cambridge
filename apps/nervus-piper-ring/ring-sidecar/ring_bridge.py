#!/usr/bin/env python3
"""Zilo ring -> motion-intent sidecar for the NervusOS Piper-ring app.

Runs on the RDK next to the USB-C Bluetooth dongle. Reads the Zilo ring's 6-axis
IMU over BLE (via the vendor SDK in ~/zilo-whisper-ring-sdk), fuses it into an
absolute orientation + an integrated translation target, and streams motion
INTENTS as newline-delimited JSON over a local TCP socket. The Kotlin NervusApp
connects to that socket and turns each intent into a Manipulator@1 call
(MoveToPose / SetGripper) through nervud.

Why a Python sidecar: the Zilo SDK is Python/Swift + BLE; the app framework is
JVM. Keeping BLE + numeric fusion here (where the vendor SDK lives) and shipping
clean intents to the JVM app is the least-risk split. Swap this out for a native
BLE reader later without touching the app.

INTENT PROTOCOL (one JSON object per line, sent to every connected client):
  {"t":"pose",   "pos":[x,y,z], "quat":[w,x,y,z], "mode":"translate|gimbal"}
  {"t":"gripper","open":true|false}
  {"t":"gesture","name":"wave"}
  {"t":"status", "connected":true|false, "msg":"..."}

Absolute frames + units match Manipulator@1: position in METERS, quaternion
[w,x,y,z] dimensionless. Position is an *integrated target* seeded at HOME_POS and
clamped to WORKSPACE — the app streams it as MoveToPose so the arm chases it, the
same "leashed" idea as the webctl but driven by the ring.

!! UNVERIFIED (no ring / no BT dongle here yet): the exact Zilo function names are
   taken from ring_sound_use.md; axis signs and the translate/gimbal split need
   tuning on real hardware. See README "Verify later".
"""
from __future__ import annotations
import argparse, asyncio, json, math, sys, time

# The vendor SDK. `python3 ring_bridge.py` must run with ~/zilo-whisper-ring-sdk
# on PYTHONPATH (the launcher script adds it).
try:
    import ring_sound as sdk
except Exception as e:  # pragma: no cover - depends on deploy env
    sdk = None
    _IMPORT_ERR = e

# ---- tuning constants (calibrate on hardware) ----
HOME_POS = [0.20, 0.0, 0.30]          # m; a safe reachable seed pose for the TCP
WORKSPACE = ((0.05, 0.55), (-0.35, 0.35), (0.05, 0.55))  # (min,max) per axis, m
TRANS_GAIN = 0.0009                    # m of target motion per (m/s^2) of ring accel per tick
VEL_DAMP = 0.86                        # velocity leak per tick (kills dead-reckoning drift)
TILT_GIMBAL_DEG = 25.0                 # |tilt| beyond this => gimbal mode, else translate
GYRO_DPS_TO_RADPS = math.pi / 180.0
G = 9.80665


def _clamp(v, lo, hi):
    return lo if v < lo else hi if v > hi else v


def euler_to_quat(roll, pitch, yaw):
    """ZYX intrinsic (roll=X, pitch=Y, yaw=Z) -> quaternion [w,x,y,z]."""
    cr, sr = math.cos(roll / 2), math.sin(roll / 2)
    cp, sp = math.cos(pitch / 2), math.sin(pitch / 2)
    cy, sy = math.cos(yaw / 2), math.sin(yaw / 2)
    return [
        cr * cp * cy + sr * sp * sy,
        sr * cp * cy - cr * sp * sy,
        cr * sp * cy + sr * cp * sy,
        cr * cp * sy - sr * sp * cy,
    ]


class Fusion:
    """Minimal complementary filter + leaky translation integrator."""

    def __init__(self, accel_range_g: float, gyro_range_dps: float):
        # raw int -> physical units (ranges come from SensorStartInfo)
        self.a_scale = (accel_range_g * G) / 32768.0
        self.g_scale = (gyro_range_dps * GYRO_DPS_TO_RADPS) / 32768.0
        self.yaw = 0.0
        self.vel = [0.0, 0.0, 0.0]
        self.pos = list(HOME_POS)
        self.last_t = None

    def update(self, s) -> dict:
        ax, ay, az = s.accel_x * self.a_scale, s.accel_y * self.a_scale, s.accel_z * self.a_scale
        gx, gy, gz = s.gyro_x * self.g_scale, s.gyro_y * self.g_scale, s.gyro_z * self.g_scale
        t = getattr(s, "timestamp_ms", None)
        dt = 0.02 if self.last_t is None or t is None else max(1e-3, (t - self.last_t) / 1000.0)
        self.last_t = t

        # orientation from gravity (roll/pitch) + integrated gyro (yaw)
        roll = math.atan2(ay, az)
        pitch = math.atan2(-ax, math.sqrt(ay * ay + az * az) + 1e-9)
        self.yaw += gz * dt
        tilt_deg = math.degrees(math.hypot(roll, pitch))

        if tilt_deg >= TILT_GIMBAL_DEG:
            mode = "gimbal"                      # dipped hand -> orientation drives the tip
        else:
            mode = "translate"                   # flat hand -> translate the tip
            # linear accel = measured - gravity (gravity ~ the low-freq accel vector)
            lin = [ax, ay, az - G]
            for i in range(3):
                self.vel[i] = self.vel[i] * VEL_DAMP + lin[i] * dt
                self.pos[i] += self.vel[i] * TRANS_GAIN * 1000.0
            for i, (lo, hi) in enumerate(WORKSPACE):
                self.pos[i] = _clamp(self.pos[i], lo, hi)

        quat = euler_to_quat(roll, pitch, self.yaw)
        return {"t": "pose", "pos": [round(p, 4) for p in self.pos],
                "quat": [round(q, 5) for q in quat], "mode": mode}


class Hub:
    """Fan-out newline-JSON to all connected TCP clients (the Kotlin app)."""

    def __init__(self):
        self.clients: set[asyncio.StreamWriter] = set()

    async def serve(self, host: str, port: int):
        srv = await asyncio.start_server(self._on_client, host, port)
        print(f"[ring_bridge] intent server on {host}:{port}", file=sys.stderr)
        return srv

    async def _on_client(self, reader, writer):
        self.clients.add(writer)
        try:
            await reader.read()  # we never read from the app; wait for EOF
        finally:
            self.clients.discard(writer)
            writer.close()

    def emit(self, obj: dict):
        line = (json.dumps(obj) + "\n").encode()
        for w in list(self.clients):
            try:
                w.write(line)
            except Exception:
                self.clients.discard(w)


async def ring_loop(address: str, hub: Hub):
    if sdk is None:
        hub.emit({"t": "status", "connected": False, "msg": f"zilo SDK import failed: {_IMPORT_ERR}"})
        print(f"[ring_bridge] cannot import ring_sound: {_IMPORT_ERR}", file=sys.stderr)
        return
    async with sdk.RingSoundClient(address=address) as ring:      # UNVERIFIED name
        hub.emit({"t": "status", "connected": True, "msg": "ring connected; single-press to enter gesture mode"})
        # gesture events (wave) run concurrently with the IMU stream
        gesture_task = asyncio.create_task(_gesture_loop(ring, hub))
        try:
            await sdk.wait_sensor_key_single_press_event(ring, timeout_s=60.0)
            start = await sdk.start_sensor_report(ring)
            fuse = Fusion(float(getattr(start, "accel_range_g", 8)),
                          float(getattr(start, "gyro_range_dps", 2000)))
            hub.emit({"t": "status", "connected": True, "msg": "IMU streaming"})
            while True:
                batch = await sdk.wait_sensor_data(ring, timeout_s=5.0)
                for sample in batch.samples:
                    hub.emit(fuse.update(sample))
        finally:
            gesture_task.cancel()
            try:
                await sdk.stop_sensor_report(ring)
            except Exception:
                pass


async def _gesture_loop(ring, hub: Hub):
    while True:
        try:
            ev = await sdk.wait_sensor_gesture_event(ring, timeout_s=3600.0)
            name = sdk.sensor_gesture_name(ev.gesture_id)
            hub.emit({"t": "gesture", "name": str(name)})
            if "wave" in str(name).lower():
                hub.emit({"t": "gesture", "name": "wave"})
        except asyncio.CancelledError:
            return
        except Exception:
            await asyncio.sleep(0.5)


async def main_async(args):
    hub = Hub()
    srv = await hub.serve(args.host, args.port)
    async with srv:
        while True:
            try:
                await ring_loop(args.address, hub)
            except Exception as e:
                hub.emit({"t": "status", "connected": False, "msg": str(e)})
                print(f"[ring_bridge] ring loop error: {e}; retry in 3s", file=sys.stderr)
                await asyncio.sleep(3)


def main():
    ap = argparse.ArgumentParser(description="Zilo ring -> motion-intent sidecar")
    ap.add_argument("--address", required=True, help="ring BLE address (scan with the Zilo SDK)")
    ap.add_argument("--host", default="127.0.0.1")
    ap.add_argument("--port", type=int, default=8790)
    asyncio.run(main_async(ap.parse_args()))


if __name__ == "__main__":
    main()
