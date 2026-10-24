"""The arm: the PiPER driver's HTTP API (hardware/piper/piper_webctl.py), or a simulated arm with blocks on a table.

Both offer the same calls, in millimetres and degrees: state(), servo(pose), gripper(mm), stop(), estop(), release().
The real driver allows one controller at a time; this program is client "llm".
"""
from __future__ import annotations

import json
import math
import threading
import time
import urllib.error
import urllib.request

CLIENT = "llm"


class DriverError(Exception):
    pass


class DriverClient:
    def __init__(self, base: str = "http://127.0.0.1:8770"):
        self.base = base.rstrip("/")

    def _req(self, path: str, body: dict | None = None, timeout: float = 2.0) -> dict:
        data = None if body is None else json.dumps({"client": CLIENT, **body}).encode()
        req = urllib.request.Request(self.base + path, data, {"Content-Type": "application/json"})
        try:
            with urllib.request.urlopen(req, timeout=timeout) as r:
                return json.loads(r.read() or b"{}")
        except urllib.error.HTTPError as e:
            try:
                msg = json.loads(e.read()).get("error", "")
            except ValueError:
                msg = ""
            if msg == "busy":
                raise DriverError("another app controls the arm right now") from None
            raise DriverError(f"driver refused {path}: {msg or e.code}") from None
        except (urllib.error.URLError, OSError) as e:
            raise DriverError(f"arm driver unreachable: {getattr(e, 'reason', e)}") from None

    def state(self) -> dict:
        s = self._req("/api/state?lite=1")
        return {"reachable": True, "enabled": bool(s.get("enabled")), "fresh": bool(s.get("feedback_fresh")),
                "pose": s.get("pose_current"), "gripper_mm": s.get("gripper_actual_mm"),
                "status": s.get("arm_status"), "owner": (s.get("owner") or {}).get("client"),
                "workspace_mm": s.get("workspace_mm")}

    def enable(self):
        self._req("/api/enable", {"on": True}, timeout=5.0)

    def servo(self, pose: list, speed: int | None = None) -> dict:
        body = {"pose": [float(v) for v in pose]}
        if speed:
            body["speed"] = int(speed)
        return self._req("/api/servo", body)

    def gripper(self, mm: float, effort: int):
        self._req("/api/gripper", {"mm": float(mm), "effort": int(effort)})

    def stop(self):
        self._req("/api/stop", {})

    def release(self):
        self._req("/api/release", {})

    def estop(self):
        self._req("/api/estop", {})


# ---------------------------------------------------------------------------------------------------------------
class SimArm:
    """A Cartesian arm that slides toward its servo goal, and coloured blocks it can carry.

    The goal expires after 0.4 s like the real driver's servo deadman, so a caller that stops sending stops the arm."""

    SPEED_MM_S = 120.0
    BLOCK_MM = 30.0

    def __init__(self):
        self.lock = threading.Lock()
        self.pose = [250.0, 0.0, 300.0, 180.0, 0.0, 0.0]
        self.goal, self.goal_t = None, 0.0
        self.grip_mm, self.enabled, self.estopped = 60.0, True, False
        self.held = None
        self.blocks = {"red": [300.0, 120.0], "blue": [300.0, -120.0], "green": [380.0, 0.0]}
        self.heights = {k: 0.0 for k in self.blocks}     # bottom of each block above the table
        self._stop = threading.Event()
        threading.Thread(target=self._run, daemon=True).start()

    def _run(self):
        dt = 0.02
        while not self._stop.wait(dt):
            with self.lock:
                if self.goal is None or time.time() - self.goal_t > 0.4 or not self.enabled:
                    continue
                d = [self.goal[i] - self.pose[i] for i in range(3)]
                n = math.sqrt(sum(v * v for v in d))
                step = min(n, self.SPEED_MM_S * dt)
                if n > 1e-6:
                    for i in range(3):
                        self.pose[i] += d[i] / n * step
                self.pose[5] = self.goal[5]
                if self.held:
                    self.blocks[self.held] = [self.pose[0], self.pose[1]]
                    self.heights[self.held] = max(0.0, self.pose[2] - self.BLOCK_MM)

    # The table is z = 0 in the simulator; a block is grasped when the jaws close around its middle.
    def _block_at(self, x, y, z):
        for k, (bx, by) in self.blocks.items():
            if math.hypot(bx - x, by - y) < 20 and self.heights[k] <= z <= self.heights[k] + self.BLOCK_MM + 10:
                return k
        return None

    def state(self) -> dict:
        with self.lock:
            return {"reachable": True, "enabled": self.enabled, "fresh": True, "pose": list(self.pose),
                    "gripper_mm": self.grip_mm, "status": "EMERGENCY_STOP" if self.estopped else "NORMAL",
                    "owner": CLIENT if self.goal is not None and time.time() - self.goal_t < 0.4 else None,
                    "workspace_mm": {"x": [60, 480], "y": [-320, 320], "z": [0, 600]}}

    def enable(self):
        with self.lock:
            self.enabled, self.estopped = True, False

    def servo(self, pose, speed=None):
        with self.lock:
            if not self.enabled:
                raise DriverError("driver refused /api/servo: motors are not enabled")
            self.goal, self.goal_t = [float(v) for v in pose], time.time()
        return {"ok": True, "goal": pose}

    def gripper(self, mm, effort):
        with self.lock:
            self.grip_mm = float(mm)
            if mm < self.BLOCK_MM and self.held is None:
                self.held = self._block_at(self.pose[0], self.pose[1], self.pose[2])
                if self.held:
                    self.grip_mm = self.BLOCK_MM
            elif mm >= self.BLOCK_MM + 5 and self.held:
                # Let go: the block drops onto whatever is under it.
                k, self.held = self.held, None
                x, y = self.blocks[k]
                below = [o for o in self.blocks if o != k and math.hypot(self.blocks[o][0] - x, self.blocks[o][1] - y) < 25]
                self.heights[k] = max([self.heights[o] + self.BLOCK_MM for o in below] + [0.0])

    def stop(self):
        with self.lock:
            self.goal = None

    release = stop

    def estop(self):
        with self.lock:
            self.goal, self.enabled, self.estopped = None, False, True

    def close(self):
        self._stop.set()
