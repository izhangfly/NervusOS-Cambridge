#!/usr/bin/env python3
"""PiPER policy execution portal — run a trained ACT checkpoint on the arm from a local web page.

    python piper_policy_portal.py                              # real arm + wrist camera
    python piper_policy_portal.py --mock-arm --mock-camera     # dry run: simulated arm, dataset video as camera

Open http://127.0.0.1:8791 . Workflow: Connect -> Load policy -> Enable -> Start pose -> Run (Space) -> Stop (Space/Esc)
-> mark Success/Fail. Everything the policy does is logged under deploy/runs/.

Design notes (all verified against LeRobot 0.4.4 and lerobot_robot_piper source):
- ACT inference is deterministic: the CVAE latent is zeros outside training and dropout is off in eval(). No seeds.
- Dataset joint names are `joint1..joint6, gripper` (no `.pos` suffix); the model sees RGB uint8 HxWx3 frames.
- Inference runs in a worker thread and returns a 30-step chunk; the 15 Hz control loop skips the actions that went
  stale while the network was running, so a slow CPU delays re-planning instead of freezing the arm.
- The plugin's `robot.connect()` enables torque AND drives to its home pose, and `disconnect()` parks then releases
  the motors. This portal never calls either: it opens the bus read-only and enables torque only when asked.
"""
from __future__ import annotations

import argparse
import csv
import io
import json
import math
import os
import queue
import signal
import sys
import threading
import time
import traceback
from collections import deque
from datetime import datetime
from http import HTTPStatus
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

import numpy as np

HERE = Path(__file__).resolve().parent
ROOT = HERE.parent
JOINTS = ["joint1", "joint2", "joint3", "joint4", "joint5", "joint6", "gripper"]
TEACHING = 0x02

# Per-step |action - state| 99th percentile in the training data (normalized units, 50 demos / 24,570 frames).
# The step limit is a multiple of this, so the policy can never move a joint faster than people did while teaching.
DEMO_STEP_P99 = {"joint1": 3.70, "joint2": 6.51, "joint3": 6.75, "joint4": 4.76, "joint5": 11.39, "joint6": 4.84, "gripper": 16.66}

DEFAULT_PARAMS = {
    "replan_every": 10,        # steps between new chunks (1..chunk_size). Lower = more reactive, more CPU
    "crossfade": 4,            # steps blended when a new chunk replaces the old one (0 = hard switch)
                               # [DELL] 4, not 3: measured 354 ms/inference on the i7-8650U (EXECUTION §8 row 300-600 ms)
    "speed_pct": 40,           # firmware MOVE J speed, percent (plugin hard-codes 30)
    "step_limit_x": 1.5,       # max per-step change of the COMMAND, as a multiple of the demo p99 (0 = off)
                               # [DELL 09-14] measured against the previous command, no longer against the measured arm
    "lead_limit_x": 4.0,       # [DELL] max distance command-ahead-of-measured, x demo p99 (0 = off). Joints only
    "ensemble": 2,             # [DELL] joint targets = mean of the newest N overlapping plans (1 = newest plan only)
    "envelope_margin": 5.0,    # targets clamped to the demonstrated joint range +/- this many units
    "gripper_effort": 500,     # GripperCtrl effort, 0.001 N·m. [DELL] 1000 (plugin default) crushed a 3D-printed cube
    "gripper_squeeze": 2.0,    # [DELL] gripper units commanded tighter than the measured cube width while holding
                               # (5 -> 2 on 09-14 20:50: fingers vibrated while stalled 3.4 mm inside the cube)
    "gripper_open_raw": 95000, # [DELL] raw width sent for "fully open" (68000 = off). See RealArm.send
    "gripper_release_steps": 8,  # [DELL] while holding, the plan must ask to open this many steps in a row (0 = off)
    "record_rollouts": 1,      # [DELL] 1 = save each run as a post-training package (AUTOMATED POST-TRAINING.md)
    "intervene_on_teach": 1,   # [DELL] 1 = drag-teach button during a run = human correction (recorded), not a stop
    "max_duration_s": 90,      # a run stops by itself after this long
    "stale_camera_ms": 500,    # stop if the newest camera frame is older than this
    "stale_arm_ms": 300,       # joint feedback older than this = a hiccup: nothing new is sent, the arm holds
    "stale_arm_grace_s": 1.0,  # [DELL] stop only if feedback stays stale this long (09-14: 310 ms CPU stalls stopped runs)
    "stale_camera_grace_s": 1.0,  # [DELL] same grace for camera blips, so a run is not killed before an intervention
    "heartbeat_s": 2.0,        # stop if the browser tab stops talking for this long while running
}
LIMITS = {
    "replan_every": (1, 30, int), "crossfade": (0, 10, int), "speed_pct": (10, 100, int), "step_limit_x": (0.0, 5.0, float),
    "lead_limit_x": (0.0, 10.0, float), "ensemble": (1, 3, int),
    "envelope_margin": (0.0, 30.0, float), "gripper_effort": (100, 5000, int), "gripper_squeeze": (0.0, 30.0, float),
    "gripper_open_raw": (68000, 101000, int), "gripper_release_steps": (0, 45, int),
    "record_rollouts": (0, 1, int), "intervene_on_teach": (0, 1, int),
    "max_duration_s": (5, 600, int),
    "stale_camera_ms": (100, 3000, int), "stale_arm_ms": (100, 3000, int), "stale_arm_grace_s": (0.2, 3.0, float),
    "stale_camera_grace_s": (0.2, 3.0, float),
    "heartbeat_s": (0.5, 10.0, float),
}


def now():
    return time.monotonic()


# =====================================================================================================================
# Hardware adapters. Interface: connect(), read() -> (dict norm, age_s), enable(), send(dict, speed, effort), estop(),
# ctrl_mode() -> int|None, close(). Replace RealArm internals with your recorder's proven helpers if they differ.
# =====================================================================================================================
class RealArm:
    STATUS_FRESH_S = 0.5

    def __init__(self, can_port: str, shim_dir: str):
        # The Dell's plugin __init__ installs the CAN shim itself on non-Linux hosts, reading PIPER_SHIM_DIR
        # (UPDATE 2026-09-10 / HANDOFF §7). Installing it a second time here could wrap the bus twice.
        if shim_dir:
            os.environ.setdefault("PIPER_SHIM_DIR", shim_dir)
        import lerobot_robot_piper  # noqa: F401  (bootstrap runs piper_mac_can.install())
        from lerobot_robot_piper.config_piper import PiperFollowerConfig
        from lerobot_robot_piper.piper_follower import PiperFollower
        self.robot = PiperFollower(PiperFollowerConfig(port=can_port))
        self.bus = self.robot.bus
        self.piper = self.bus.piper
        self.enabled = False

    def connect(self):
        self.bus.connect()  # opens CAN only: no torque, no parking

    def read(self):
        vals = self.bus.get_action()
        # [DELL, verified] time_stamp is the shim's time.time() at CAN receive (piper_mac_can.py -> protocol_v2
        # can_time_now). 0 means no frame ever arrived: the SDK cache then holds zeros that normalize to a
        # plausible pose (joint2=-100, joint3=+100), so that must read as infinitely old, never as fresh.
        ts = getattr(self.piper.GetArmJointMsgs(), "time_stamp", 0) or 0
        age = max(0.0, time.time() - ts) if ts > 1e9 else float("inf")
        return vals, age

    def _status(self):
        # STANDBY (0x00) stops the broadcast, so a cached status can be minutes old. Only trust a fresh one.
        st = self.piper.GetArmStatus()
        if not st.time_stamp or time.time() - st.time_stamp > self.STATUS_FRESH_S:
            return None
        return st.arm_status

    def ctrl_mode(self):
        try:
            s = self._status()
            return None if s is None else int(s.ctrl_mode)  # IntEnum in piper_sdk, int() is safe (verified)
        except Exception:
            return None

    def ctrl_mode_raw(self):
        """Cached ctrl_mode with NO freshness gate. Joint feedback (arm_age) is the liveness signal; the status frame
        that carries ctrl_mode is intermittent, so the 0.5 s gate in ctrl_mode() returned None and the drag-teach
        intervention never fired (UPDATE 09-14 21:2x). Trust only while arm feedback is live (callers gate on arm_age)."""
        try:
            return int(self.piper.GetArmStatus().arm_status.ctrl_mode)
        except Exception:
            return None

    def status_raw(self):
        """[ctrl_mode, arm_status, teach_status] from the cached status, no gate — for per-step diagnosis."""
        try:
            st = self.piper.GetArmStatus().arm_status
            teach = getattr(st, "teach_status", 0) or 0
            return [int(st.ctrl_mode), int(st.arm_status), int(teach)]
        except Exception:
            return None

    def fault(self):
        try:
            s = self._status()
            if s is None:
                return None  # staleness is caught by the feedback-age check
            code = int(s.arm_status)
            return None if code in (0x00, 0x02, 0x03, 0x04) else f"arm status 0x{code:02X}"  # soft faults ok (README §4)
        except Exception:
            return None

    def enable(self, gripper_effort: int):
        """Torque on and hold exactly where the arm is measured to be.

        [DELL] Works straight from drag-teach: README §4 (measured on this arm) - the enable sequence plus a CAN-mode
        motion command flips ctrl_mode 0x02 -> 0x01. Drag-teach is also the one mode where feedback is guaranteed
        live, so the hold pose is known to be current. Never commands a pose from stale feedback (STANDBY).
        """
        _, age = self.read()
        if age > 0.3:
            raise RuntimeError("no live joint feedback (the arm stops streaming in STANDBY). Press the drag-teach "
                               "button once so its light is ON, then Enable again - the portal takes it out of drag-teach")
        self.bus.enable_torque()  # EnablePiper until all 6 motors report enabled (5 s max)
        time.sleep(1.2)           # README §4 hard-won enable sequence: EnableArm, wait 1.2 s, then the mode command
        vals, age = self.read()
        if age > 0.3:
            raise RuntimeError("joint feedback stopped while enabling; not commanding anything")
        self.send(vals, 20, gripper_effort)  # ModeCtrl(CAN, MOVE J) + JointCtrl at the measured pose = hold
        deadline = time.time() + 3.0
        mode = None
        while time.time() < deadline:
            mode = self.ctrl_mode()
            if mode == 0x01:
                self.enabled = True
                return
            time.sleep(0.1)
        raise RuntimeError(f"arm did not switch to CAN control (ctrl_mode {mode}); press the drag-teach button "
                           f"so its light is off, then Enable again")

    GRIP_KNEE = 90.0  # normalized; below this the calibration is used unchanged (cube holds read ~76-84)

    def send(self, target: dict, speed_pct: int, gripper_effort: int, gripper_open_raw: int | None = None):
        raw = self.bus._unnormalize(target)
        g = int(raw["gripper"])
        cal_max = self.bus.calibration["gripper"].range_max
        if gripper_open_raw and gripper_open_raw > cal_max and target["gripper"] > self.GRIP_KNEE:
            # [DELL] The plugin calibration caps the gripper at raw 68,000, so every demo frame opened wider than 68 mm
            # (measured up to 101,500) was recorded as 100. "100" therefore means "wide open", and the policy can only
            # ever command 68 mm. Stretch 90..100 onto knee..gripper_open_raw; the grasp range (<90) is unchanged and
            # a wide-open gripper still reads 100 back, exactly as in training.
            knee_raw = cal_max * self.GRIP_KNEE / 100.0
            g = int(knee_raw + (target["gripper"] - self.GRIP_KNEE) / (100.0 - self.GRIP_KNEE) * (gripper_open_raw - knee_raw))
        self.piper.ModeCtrl(0x01, 0x01, int(speed_pct), 0x00)
        self.piper.JointCtrl(*(int(raw[j]) for j in JOINTS[:6]))
        # [DELL 09-14] Gripper only on change (>= 0.3 mm or new effort) or every 2 s. Re-sending the same setpoint with
        # 0x03 (enable + clear error) 15x/s while the fingers are stalled on the cube coincided with heavy gripper
        # vibration (runs 20:30-20:42: command constant for 72 s, fingers buzzing).
        t = time.monotonic()
        last = getattr(self, "_grip_last", None)
        if last is None or abs(g - last[0]) >= 300 or int(gripper_effort) != last[1] or t - last[2] >= 2.0:
            self.piper.GripperCtrl(abs(g), int(gripper_effort), 0x03, 0)  # 0x03 = enable + clear error, as the plugin does
            self._grip_last = (g, int(gripper_effort), t)

    def grip_feedback(self):
        """(effort 0.001 N·m, status_code bits: see piper_sdk arm_feedback_gripper.FOC_Status) for the logs."""
        try:
            gs = self.piper.GetArmGripperMsgs().gripper_state
            return [int(gs.grippers_effort), int(gs.status_code)]
        except Exception:
            return None

    def estop(self):
        self.piper.EmergencyStop(0x01)
        self.enabled = False

    def close(self):
        try:
            self.bus.disconnect(disable_torque=False)  # never park or drop the arm on exit
        except Exception:
            pass


class MockArm:
    """First-order joint tracking toward the last target; speed_pct scales the slew rate."""

    def __init__(self, start: dict):
        self.state = dict(start)
        self.target = dict(start)
        self.speed = 40
        self.enabled = False
        self.mode = TEACHING
        self._t = now()
        self._lock = threading.Lock()

    def connect(self):
        pass

    def _advance(self):
        t = now()
        dt, self._t = t - self._t, t
        if not self.enabled:
            return
        rate = 2.5 * self.speed  # units/s at speed_pct
        for j in JOINTS:
            d = self.target[j] - self.state[j]
            self.state[j] += max(-rate * dt, min(rate * dt, d))

    def read(self):
        with self._lock:
            self._advance()
            return dict(self.state), 0.0

    def ctrl_mode(self):
        return self.mode

    def fault(self):
        return None

    def enable(self, gripper_effort=None):
        with self._lock:
            self.enabled, self.mode = True, 0x01

    def grip_feedback(self):
        return None

    def status_raw(self):
        return [self.mode, 0, 0]

    def send(self, target, speed_pct, gripper_effort, gripper_open_raw=None):
        with self._lock:
            self._advance()
            self.target, self.speed = dict(target), speed_pct

    def estop(self):
        with self._lock:
            self.enabled = False

    def close(self):
        pass


class Camera:
    """Latest-frame camera thread. Opens by device NAME on Windows (README §6: never by index)."""

    def __init__(self, name: str | None, index: int | None, width=640, height=480):
        self.name, self.index, self.w, self.h = name, index, width, height
        self.frame = None
        self.stamp = 0.0
        self.times = deque(maxlen=30)
        self.alive = False
        self.error = None

    def _resolve_index(self):
        if self.index is not None:
            return self.index
        try:
            from pygrabber.dshow_graph import FilterGraph
            names = FilterGraph().get_input_devices()
        except Exception as e:
            raise RuntimeError(f"cannot list cameras by name (pip install pygrabber): {e}")
        matches = [i for i, n in enumerate(names) if self.name.lower() in n.lower()]
        if not matches:
            raise RuntimeError(f"camera '{self.name}' not found; devices: {names}")
        return matches[0]

    def start(self):
        threading.Thread(target=self._run, daemon=True, name="camera").start()

    def _run(self):
        import cv2
        try:
            idx = self._resolve_index()
            cap = cv2.VideoCapture(idx, cv2.CAP_DSHOW) if sys.platform == "win32" else cv2.VideoCapture(idx)
            cap.set(cv2.CAP_PROP_FOURCC, cv2.VideoWriter_fourcc(*"MJPG"))  # same request as the recorder
            cap.set(cv2.CAP_PROP_FRAME_WIDTH, self.w)
            cap.set(cv2.CAP_PROP_FRAME_HEIGHT, self.h)
            if not cap.isOpened():
                raise RuntimeError(f"camera index {idx} did not open")
            self.alive = True
            while self.alive:
                ok, bgr = cap.read()
                if not ok:
                    time.sleep(0.02)
                    continue
                if bgr.shape[1] != self.w or bgr.shape[0] != self.h:
                    bgr = cv2.resize(bgr, (self.w, self.h))
                self.frame = cv2.cvtColor(bgr, cv2.COLOR_BGR2RGB)  # training frames are RGB
                t = now()
                self.stamp = t
                self.times.append(t)
            cap.release()
        except Exception as e:
            self.error = str(e)
            self.alive = False

    def latest(self):
        return self.frame, (now() - self.stamp) if self.stamp else float("inf")

    def fps(self):
        if len(self.times) < 2:
            return 0.0
        span = self.times[-1] - self.times[0]
        return (len(self.times) - 1) / span if span > 0 else 0.0

    def stop(self):
        self.alive = False


class MockCamera(Camera):
    """Replays wrist video of one training episode at 12.5 fps (the real Dabai rate)."""

    def __init__(self, dataset_root: Path, episode: int = 2):
        super().__init__("mock", 0)
        self.root, self.episode = dataset_root, episode

    def _run(self):
        try:
            from lerobot.datasets.lerobot_dataset import LeRobotDataset
            ds = LeRobotDataset("local/replay", root=self.root, episodes=[self.episode])
            frames = [(ds[i]["observation.images.wrist"].permute(1, 2, 0).numpy() * 255).astype(np.uint8)
                      for i in range(0, min(len(ds), 450), 3)]
            self.alive, i = True, 0
            while self.alive:
                self.frame = frames[i % len(frames)]
                t = now()
                self.stamp = t
                self.times.append(t)
                i += 1
                time.sleep(0.08)
        except Exception as e:
            self.error = f"mock camera: {e}"
            self.alive = False


# =====================================================================================================================
# Policy
# =====================================================================================================================
class Policy:
    def __init__(self, path: Path, threads: int):
        import torch
        from lerobot.policies.act.modeling_act import ACTPolicy
        from lerobot.policies.factory import make_pre_post_processors
        torch.set_num_threads(threads)
        self.torch = torch
        self.path = path
        self.model = ACTPolicy.from_pretrained(str(path))
        self.model.config.device = "cpu"
        self.model.to("cpu").eval()
        cpu = {"device_processor": {"device": "cpu"}}
        self.pre, self.post = make_pre_post_processors(self.model.config, pretrained_path=str(path),
                                                       preprocessor_overrides=cpu, postprocessor_overrides=cpu)
        self.chunk_size = int(self.model.config.chunk_size)
        self.image_keys = [k for k in self.model.config.input_features if k.startswith("observation.images.")]
        self.last_ms = None

    def chunk(self, state: dict, image_rgb: np.ndarray) -> np.ndarray:
        from lerobot.policies.utils import prepare_observation_for_inference
        obs = {"observation.state": np.array([state[j] for j in JOINTS], dtype=np.float32)}
        for k in self.image_keys:
            obs[k] = image_rgb
        t = time.perf_counter()
        with self.torch.inference_mode():
            batch = self.pre(prepare_observation_for_inference(obs, self.torch.device("cpu"), None, "piper_follower"))
            actions = self.model.predict_action_chunk(batch)  # (1, chunk, 7), normalized
            out = np.stack([self.post(actions[:, i, :]).squeeze(0).cpu().numpy() for i in range(actions.shape[1])])
        self.last_ms = (time.perf_counter() - t) * 1000
        return out


def _policy_worker(conn, path, threads):
    """[DELL] Inference in its own process. In the portal process the CAN SDK parses ~2,300 frames/s in pure Python,
    and sharing that GIL doubled ACT inference on the Dell's i7-8650U (measured: 354 ms alone -> 720 ms with the
    arm connected, independent of torch thread count)."""
    try:
        pol = Policy(Path(path), threads)
        conn.send(("ready", pol.chunk_size, pol.image_keys))
    except Exception as e:
        conn.send(("error", f"{type(e).__name__}: {e}"))
        return
    while True:
        try:
            msg = conn.recv()
        except (EOFError, OSError):
            return
        if msg is None:
            return
        try:
            out = pol.chunk(*msg)
            conn.send(("ok", out, pol.last_ms))
        except Exception as e:
            conn.send(("error", f"{type(e).__name__}: {e}"))


class PolicyProcess:
    """Same interface as Policy (path, chunk_size, image_keys, last_ms, chunk()), executed in a spawned process."""

    def __init__(self, path: Path, threads: int):
        import multiprocessing as mp
        ctx = mp.get_context("spawn")
        self.path = path
        self.last_ms = None
        self.infer_ms = None
        self.lock = threading.Lock()
        self.conn, child = ctx.Pipe()
        self.proc = ctx.Process(target=_policy_worker, args=(child, str(path), threads), daemon=True)
        self.proc.start()
        child.close()
        if not self.conn.poll(300):
            self.close()
            raise RuntimeError("policy process did not start within 300 s")
        msg = self.conn.recv()
        if msg[0] != "ready":
            self.close()
            raise RuntimeError(msg[1])
        _, self.chunk_size, self.image_keys = msg

    def chunk(self, state: dict, image_rgb: np.ndarray) -> np.ndarray:
        with self.lock:
            if not self.proc.is_alive():
                raise RuntimeError("policy process died")
            t = time.perf_counter()
            self.conn.send((dict(state), image_rgb))
            if not self.conn.poll(30):
                raise RuntimeError("policy process did not answer within 30 s")
            msg = self.conn.recv()
            if msg[0] != "ok":
                raise RuntimeError(msg[1])
            self.last_ms = (time.perf_counter() - t) * 1000  # includes the pipe round trip
            self.infer_ms = msg[2]
            return msg[1]

    def close(self):
        try:
            self.conn.send(None)
        except Exception:
            pass
        try:
            self.proc.join(3)
            if self.proc.is_alive():
                self.proc.terminate()
        except Exception:
            pass


class Planner:
    """Async chunking: obs at step s -> chunk[i] is the target for step s+i. Stale entries are skipped on arrival."""

    def __init__(self, policy: Policy):
        self.policy = policy
        self.plan = {}          # step -> np.ndarray(7)
        self.history = deque(maxlen=3)  # [DELL] (obs_step, chunk) of the newest plans, for ensembling
        self.ens_n = 1
        self.inflight = None    # step of the observation being processed
        self.ready = None       # (obs_step, chunk)
        self.last_obs_step = None
        self.lock = threading.Lock()
        self.latencies = deque(maxlen=20)
        self.starved = 0
        self.error = None

    def request(self, step: int, state: dict, image: np.ndarray):
        with self.lock:
            if self.inflight is not None:
                return False
            self.inflight = step
        threading.Thread(target=self._work, args=(step, dict(state), image.copy()), daemon=True, name="policy").start()
        return True

    def _work(self, step, state, image):
        t = now()
        try:
            ch = self.policy.chunk(state, image)
            with self.lock:
                self.ready = (step, ch)
                self.latencies.append(now() - t)
        except Exception as e:
            self.error = f"policy: {e}"
        finally:
            with self.lock:
                self.inflight = None

    def integrate(self, k: int, crossfade: int):
        with self.lock:
            got, self.ready = self.ready, None
        if not got:
            return None
        s, ch = got
        self.last_obs_step = s
        self.history.append((s, ch))
        new = {s + i: ch[i] for i in range(len(ch)) if s + i >= k}
        skipped = max(0, k - s)
        for n, step in enumerate(sorted(new)):
            if n < crossfade and step in self.plan:
                w = (n + 1) / (crossfade + 1)
                new[step] = (1 - w) * self.plan[step] + w * new[step]
        self.plan = new
        return skipped

    def take(self, k: int, ensemble: int = 1):
        a = self.plan.pop(k, None)
        for old in [x for x in self.plan if x < k]:
            self.plan.pop(old)
        if a is None:
            self.starved += 1
            return None
        self.ens_n = 1
        if ensemble > 1:
            # [DELL 09-14] Async temporal ensembling. Each plan comes from a camera frame ~12 steps old and a new one
            # takes over every ~12 steps, so consecutive plans disagree by a few units and the arm reversed direction
            # ~1-1.5x/s on every joint (run logs 18:45-19:09). Averaging the overlapping plans for this step is ACT's
            # own temporal-ensembling idea, affordable at 700 ms/inference. The gripper is NOT averaged: the mean of
            # "open" and "closed" is a half-open gripper, which is what dropped the cube. It takes the newest plan.
            vals = [c[k - s] for s, c in list(self.history)[-ensemble:] if 0 <= k - s < len(c)]
            if len(vals) > 1:
                m = np.mean(vals, axis=0)
                m[6] = a[6]
                self.ens_n = len(vals)
                return m
        return a

    def remaining(self, k):
        return sum(1 for x in self.plan if x >= k)


# =====================================================================================================================
# Post-training rollout packages (contract: posttrain/CONTRACT.json, spec: AUTOMATED POST-TRAINING.md)
# =====================================================================================================================
CONTRACT_VERSION = 1


def read_contract(posttrain_dir: Path) -> dict | None:
    try:
        return json.loads((posttrain_dir / "CONTRACT.json").read_text(encoding="utf-8"))
    except Exception:
        return None


class RolloutRecorder:
    """Writes posttrain/rollouts/<run_id>/: episode.jsonl (one row per control step), wrist.mp4 (frame i = the exact
    image the policy saw at row i), meta.json. READY is written only after the operator marks the outcome, so the Mac
    never consumes a half-written or unlabelled run. Rows and frames go through ONE queue, so they cannot misalign;
    if the encoder ever falls behind, both are dropped together and the gap shows in `k`."""

    def __init__(self, posttrain_dir: Path, run_id: str, meta: dict, fps: int):
        self.dir = posttrain_dir / "rollouts" / run_id
        self.dir.mkdir(parents=True, exist_ok=True)
        self.meta = dict(meta, contract_version=CONTRACT_VERSION, run_id=run_id, fps=fps, video="wrist.mp4",
                         created=datetime.now().isoformat(timespec="seconds"))
        self.fps = fps
        self.q = queue.Queue(maxsize=60)
        self.frames = 0
        self.dropped = 0
        self.error = None
        self._f = open(self.dir / "episode.jsonl", "w", encoding="utf-8")
        self._writer = None
        self._t = threading.Thread(target=self._run, daemon=True, name="rollout-writer")
        self._t.start()

    def add(self, row: dict, image_rgb: np.ndarray):
        try:
            self.q.put_nowait((row, image_rgb))
        except queue.Full:
            self.dropped += 1

    def _run(self):
        import cv2
        while True:
            item = self.q.get()
            if item is None:
                return
            row, img = item
            try:
                if self._writer is None:
                    h, w = img.shape[:2]
                    self._writer = cv2.VideoWriter(str(self.dir / "wrist.mp4"), cv2.VideoWriter_fourcc(*"mp4v"), self.fps, (w, h))
                self._writer.write(cv2.cvtColor(img, cv2.COLOR_RGB2BGR))
                row["frame"] = self.frames
                self.frames += 1
                self._f.write(json.dumps(row) + "\n")
            except Exception as e:
                self.error = str(e)

    def close(self, summary: dict):
        self.q.put(None)
        self._t.join(timeout=30)
        if self._writer is not None:
            self._writer.release()
        self._f.close()
        self.meta.update(summary, frames=self.frames, dropped=self.dropped, writer_error=self.error)
        self._write_meta()

    def _write_meta(self):
        tmp = self.dir / "meta.json.tmp"
        tmp.write_text(json.dumps(self.meta, indent=2), encoding="utf-8")
        os.replace(tmp, self.dir / "meta.json")

    def finalize(self, outcome: str, case: str, note: str) -> bool:
        if (self.dir / "READY").exists():
            return False  # first label wins: the Mac may already have consumed this package
        self.meta.update(outcome=outcome, case=case, note=note, labelled=datetime.now().isoformat(timespec="seconds"))
        self._write_meta()
        (self.dir / "READY").write_text(outcome, encoding="utf-8")
        return True


# =====================================================================================================================
# Controller
# =====================================================================================================================
class Controller:
    FPS = 15

    def __init__(self, args):
        self.args = args
        self.params = dict(DEFAULT_PARAMS)
        self.state = "disconnected"   # disconnected, connected, enabled, moving, running, stopped, estopped
        self.message = "Connect to the arm to begin."
        self.arm = None
        self.cam = None
        self.policy = None
        self.planner = None
        self.lock = threading.RLock()
        self.last_heartbeat = now()
        self.joints, self.arm_age, self.target = None, None, None
        self.run = None
        # [DELL] dry runs log to a local temp folder: deploy/runs/results.csv drives policy promotion on the Mac
        self.runs_dir = (Path(os.environ.get("TEMP", HERE)) / "piper_posttrain_mock" / "runs"
                         if (args.mock_arm or args.mock_camera) else HERE / "runs")
        self.results_path = self.runs_dir / "results.csv"
        self.posttrain_dir = Path(args.posttrain_dir)
        self.rollout = None
        self.info = self._dataset_info()
        self.envelope = self.info["envelope"]
        self.start_pose = self.info["start_pose"]
        self.stop_flag = threading.Event()
        self.correct_flag = threading.Event()  # [DELL/MAC] explicit "Start correction" trigger, read by _loop
        self.preview_result = None
        self.shutdown_fn = None
        threading.Thread(target=self._monitor, daemon=True, name="monitor").start()

    # ---- dataset-derived facts ----
    def _dataset_info(self):
        root = Path(self.args.dataset_root)
        env = {j: (-100.0, 100.0) for j in JOINTS}
        env["gripper"] = (0.0, 100.0)
        start = {"joint1": 0.44, "joint2": -100.0, "joint3": 98.58, "joint4": -1.2, "joint5": 4.88, "joint6": 0.87, "gripper": 0.0}
        fps = 15
        try:
            info = json.loads((root / "meta/info.json").read_text())
            fps = int(info["fps"])
            names = info["features"]["observation.state"]["names"]
            assert names == JOINTS, f"dataset joint order {names} != {JOINTS}"
            stats = json.loads((root / "meta/stats.json").read_text())["observation.state"]
            env = {j: (float(stats["min"][i]), float(stats["max"][i])) for i, j in enumerate(JOINTS)}
        except Exception as e:
            self.message = f"Dataset metadata not read ({e}); using built-in envelope."
        return {"envelope": env, "start_pose": start, "fps": fps}

    # ---- helpers ----
    def _clamp(self, target: dict, current: dict, last: dict | None = None) -> dict:
        """Envelope, then a rate limit on the COMMAND, then (joints only) a cap on how far it may lead the arm.

        [DELL 09-14] The step limit used to be measured from the *measured* position. The gripper answers commands
        with a 0.5-0.8 s dead time, so its command was pinned to measured+20 and the gripper crept open in stalls
        (run 18:53: plan 100, sent 39, measured 19 for 0.6 s) - slow, half-open, and the model then flipped between
        open and close. Joints were clipped the same way on 24-60 % of steps whenever the arm lagged.
        """
        p = self.params
        out = {}
        for j in JOINTS:
            v = float(target[j])
            lo, hi = self.envelope[j]
            v = min(hi + p["envelope_margin"], max(lo - p["envelope_margin"], v))
            if p["step_limit_x"] > 0 and last is not None:
                lim = DEMO_STEP_P99[j] * p["step_limit_x"] * (3.0 if j == "gripper" else 1.0)
                v = min(last[j] + lim, max(last[j] - lim, v))
            if j != "gripper" and p["lead_limit_x"] > 0 and current is not None:
                lead = DEMO_STEP_P99[j] * p["lead_limit_x"]
                v = min(current[j] + lead, max(current[j] - lead, v))
            out[j] = min(100.0, max(-100.0 if j != "gripper" else 0.0, v))
        return out

    def _gripper(self, plan_g: float, meas_g: float, g: dict) -> tuple[float, str]:
        """[DELL 09-14] Gripper command with a grasp latch. Returns (normalized command, mode).

        holding = the fingers have STOPPED (stable 0.4 s) a little wider than the command, in the cube-width zone: they
        are on the cube. (Free fingers reach the command; fingers still travelling after the gripper's 0.5-0.8 s dead
        time are far from it or not stable.) While holding, the command stays at measured - squeeze regardless of the
        plan, which prevents both failure modes in the logs: a stale plan saying "close to 0" (with effort 1000 that
        crushed the printed cube) and a single plan saying "open" mid-lift (dropped it). Release needs
        `gripper_release_steps` consecutive open requests.
        """
        p = self.params
        sq = p["gripper_squeeze"]
        last = g.get("last")
        hist = g.setdefault("hist", deque(maxlen=6))
        hist.append(meas_g)
        stable = len(hist) == hist.maxlen and max(hist) - min(hist) <= 1.0
        holding = (last is not None and stable and 40.0 <= meas_g <= 92.0 and 1.5 <= meas_g - last <= 12.0)
        if holding and p["gripper_release_steps"] > 0:
            g["open_count"] = g.get("open_count", 0) + 1 if plan_g >= meas_g + 8.0 else 0
            if g["open_count"] < p["gripper_release_steps"]:
                cmd = max(0.0, meas_g - sq)
                g["last"] = cmd
                return cmd, "hold"
            g["open_count"] = 0
            g["last"] = plan_g
            return plan_g, "release"
        g["open_count"] = 0
        # not holding: follow the plan; squeeze only applies when the plan closes onto something
        cmd = plan_g if plan_g >= 95.0 else max(0.0, plan_g - sq)
        g["last"] = cmd
        return cmd, "follow"

    def _squeeze(self, target: dict) -> dict:
        """[DELL] Command the gripper slightly tighter than the policy's target.

        The demos were drag-taught, so while the cube was held the recorded gripper position IS the cube's width
        (raw 52,000-53,600) and action[t] = state[t+1] repeats it. Commanding exactly the contact position asks
        for ~zero grip force and the cube can slip out during the lift. A few units tighter makes the firmware
        squeeze, limited by gripper_effort. The state fed back to the policy still reads the cube's width, as in
        training. Applied only to what is sent, never fed back into the plan.
        """
        s = self.params["gripper_squeeze"]
        if s <= 0:
            return target
        out = dict(target)
        out["gripper"] = max(0.0, out["gripper"] - s)
        return out

    def _set(self, state, message):
        self.state, self.message = state, message

    def snapshot(self):
        cam_fps = self.cam.fps() if self.cam else 0.0
        _, cam_age = self.cam.latest() if self.cam else (None, None)
        run = self.run or {}
        pl = self.planner
        lat = [x * 1000 for x in pl.latencies] if pl else []
        mode = self.arm.ctrl_mode() if self.arm else None
        age = self.arm_age
        live = age is not None and not math.isinf(age)
        return {
            "state": self.state, "message": self.message, "mock": {"arm": self.args.mock_arm, "camera": self.args.mock_camera},
            "params": self.params, "limits": {k: [v[0], v[1]] for k, v in LIMITS.items()},
            "arm": {"connected": self.arm is not None, "enabled": bool(self.arm and self.arm.enabled),
                    "ctrl_mode": mode, "teaching": mode == TEACHING,
                    # inf (never received) must not reach round()/JSON: it would crash /api/state
                    "feedback_age_ms": round(age * 1000) if live else None, "feedback_live": live and age < 0.3,
                    "joints": self.joints, "target": self.target},
            "camera": {"fps": round(cam_fps, 1), "age_ms": None if cam_age in (None, float("inf")) else round(cam_age * 1000),
                       "error": self.cam.error if self.cam else None},
            "policy": {"loaded": self.policy is not None, "name": self.policy.path.name if self.policy else None,
                       "chunk_size": self.policy.chunk_size if self.policy else None,
                       "latency_ms": round(sum(lat) / len(lat)) if lat else (round(self.policy.last_ms) if self.policy and self.policy.last_ms else None),
                       "latency_max_ms": round(max(lat)) if lat else None},
            "run": {k: run.get(k) for k in ("id", "steps", "elapsed_s", "starved", "skipped_avg", "overruns", "stop_reason",
                                            "checkpoint", "interventions", "recorded_frames", "segment", "labelled")},
            "current_policy": self._current_policy(),
            "checkpoints": self.list_checkpoints(),
            "start_pose": self.start_pose, "envelope": self.envelope, "preview": self.preview_result,
        }

    def list_checkpoints(self):
        d = Path(self.args.policies_dir)
        if not d.is_dir():
            return []
        return sorted(p.name for p in d.iterdir() if not p.name.startswith(".") and (p / "model.safetensors").is_file() and (p / "config.json").is_file())

    # ---- actions ----
    def connect(self):
        with self.lock:
            if self.arm is not None:
                if self.state in ("running", "moving"):
                    return
                _, age = self.arm.read()
                if age < 1.0:
                    return
                # [DELL] Reconnect: feedback went stale (seen 09-14 after an E-STOP: 20 min of silence while the
                # page could only say "no live feedback"). Close the old bus first - two buses on one candleLight
                # starve each other (UPDATE 09-13 bug 5).
                old, self.arm = self.arm, None
                try:
                    old.close()
                except Exception:
                    pass
                time.sleep(0.5)
            self.message = "Connecting…"
            # [DELL] publish self.arm only after connect() succeeded, so a failed attempt can be retried
            arm = MockArm(self.start_pose) if self.args.mock_arm else RealArm(self.args.can_port, self.args.shim_dir)
            arm.connect()
            self.arm = arm
            if self.cam is None:
                self.cam = MockCamera(Path(self.args.dataset_root)) if self.args.mock_camera else Camera(self.args.camera_name, self.args.camera_index)
                self.cam.start()
            age = float("inf")
            for _ in range(20):
                _, age = arm.read()
                if age < 0.3:
                    break
                time.sleep(0.1)
            if age < 0.3:
                self._set("connected", "Connected (motors not enabled). Load a policy.")
            else:
                self._set("connected", "CAN open but NO live feedback from the arm: it is in STANDBY or unpowered. "
                                       "Press the drag-teach button so its light is ON (the portal exits drag-teach itself).")

    def load(self, name: str):
        with self.lock:
            if self.state == "running":
                raise RuntimeError("stop the run before loading another policy")
            # Only names the portal itself lists. The request is unauthenticated; joining an arbitrary string onto
            # policies_dir would let "..\..\x" or an absolute/UNC path load (and run) a model from anywhere.
            if name not in self.list_checkpoints():
                raise RuntimeError(f"unknown checkpoint {name!r}")
            path = Path(self.args.policies_dir) / name
            self.message = f"Loading {name}…"
            pol = Policy(path, self.args.threads) if self.args.inprocess else PolicyProcess(path, self.args.threads)
            img = np.zeros((480, 640, 3), dtype=np.uint8)
            s = dict(self.start_pose)
            for _ in range(3):  # warm-up + honest latency measurement
                pol.chunk(s, img)
            old, self.policy, self.planner = self.policy, pol, Planner(pol)
            self.preview_result = None
            if old is not None and hasattr(old, "close"):
                old.close()
            if self.params["replan_every"] > pol.chunk_size:
                self.params["replan_every"] = pol.chunk_size
            self.message = f"Loaded {name}: chunk {pol.chunk_size}, one inference ≈ {pol.last_ms:.0f} ms on this CPU."

    def enable(self):
        with self.lock:
            self._need(connected=True)
            if self.state in ("running", "moving"):
                raise RuntimeError(f"already {self.state}")
            # [DELL] RealArm.enable() holds the pose measured *after* enabling (and leaves drag-teach itself).
            # The old code re-sent self.joints from the 10 Hz monitor, which can be stale.
            self.arm.enable(self.params["gripper_effort"])
            self._set("enabled", "Motors enabled and holding. Move to the start pose.")

    # Calibrated on checkpoint 100000 at the rest pose (max joint deviation planned 2 s ahead, normalized units):
    # the demos' own start frames -> mean 57 (the model rises); a daylight frame of the changed room on 09-14 -> 4
    # (the model would sit still). Motion-free: runs one inference, sends nothing to the arm.
    PREVIEW_GOOD, PREVIEW_WEAK = 20.0, 8.0

    def preview(self):
        """What the policy WOULD do from the current joints + camera frame. Sends nothing."""
        with self.lock:
            self._need(connected=True, policy=True)
            if self.state in ("running", "moving"):
                raise RuntimeError(f"not while {self.state}")
        state, age = self.arm.read()
        img, cam_age = self.cam.latest()
        if age > 0.3:
            raise RuntimeError("scene check needs live joint feedback")
        if img is None or cam_age > 0.5:
            raise RuntimeError("scene check needs a fresh camera frame")
        ch = self.policy.chunk(state, img)
        dev = ch - np.array([state[j] for j in JOINTS])
        end = float(np.abs(dev[-1, :6]).max())
        verdict = ("model plans to move" if end >= self.PREVIEW_GOOD else
                   "weak: may hesitate" if end >= self.PREVIEW_WEAK else
                   "model would likely SIT STILL: scene/light differs from the demos")
        self.preview_result = {"at": datetime.now().strftime("%H:%M:%S"), "score": round(end, 1), "verdict": verdict,
                               "good": end >= self.PREVIEW_GOOD, "weak": self.PREVIEW_WEAK <= end < self.PREVIEW_GOOD,
                               "change_2s": {j: round(float(dev[-1, i]), 1) for i, j in enumerate(JOINTS)},
                               "brightness": round(float(img.mean()) / 255, 2), "ms": round(self.policy.last_ms or 0)}
        return self.preview_result

    def goto_start(self):
        with self.lock:
            self._need(connected=True, enabled=True)
            if self.state == "running":
                raise RuntimeError("stop the run first")
            self._set("moving", "Moving to the start pose…")
        threading.Thread(target=self._goto, args=(dict(self.start_pose),), daemon=True, name="goto").start()

    def _goto(self, pose):
        try:
            cur, _ = self.arm.read()
            steps = 60  # 4 s at 15 Hz, slow and smooth
            for i in range(1, steps + 1):
                if self.stop_flag.is_set():
                    break
                w = 0.5 - 0.5 * math.cos(math.pi * i / steps)
                tgt = {j: cur[j] + (pose[j] - cur[j]) * w for j in JOINTS}
                self.arm.send(tgt, 20, self.params["gripper_effort"])
                self.target = tgt
                time.sleep(1 / 15)
            time.sleep(1.0)
            self._set("enabled", "At the start pose. Place the block, then Run.")
        except Exception as e:
            self._set("stopped", f"Start-pose move failed: {e}")
        finally:
            self.stop_flag.clear()

    def start_run(self):
        with self.lock:
            self._need(connected=True, enabled=True, policy=True)
            if self.state in ("running", "moving"):
                raise RuntimeError(f"already {self.state}")
            img, age = self.cam.latest()
            if img is None or age * 1000 > self.params["stale_camera_ms"]:
                raise RuntimeError("no fresh camera frame")
            if self.cam.fps() < 5:
                raise RuntimeError(f"camera at {self.cam.fps():.1f} fps (< 5): fix the camera before running")
            _, arm_age = self.arm.read()
            if arm_age * 1000 > self.params["stale_arm_ms"]:
                raise RuntimeError("no live joint feedback from the arm")
            mode = self.arm.ctrl_mode()
            if not self.args.mock_arm and mode != 0x01:
                raise RuntimeError("the arm is in drag-teach mode: press Enable" if mode == TEACHING else
                                   f"the arm is not under CAN control (ctrl_mode {mode}): press Enable")
            ckpt = self.policy.path.name
            rid = datetime.now().strftime("%Y%m%d-%H%M%S") + "_" + ckpt
            rollout = None
            if self.params["record_rollouts"]:
                mock = self.args.mock_arm or self.args.mock_camera
                # dry runs never go to the synced folder: the Mac must not train on simulated episodes
                pt_dir = Path(os.environ.get("TEMP", HERE)) / "piper_posttrain_mock" if mock else self.posttrain_dir
                contract = read_contract(self.posttrain_dir)
                if contract is None or int(contract.get("contract_version", -1)) != CONTRACT_VERSION:
                    raise RuntimeError(f"posttrain/CONTRACT.json version {contract and contract.get('contract_version')} "
                                       f"does not match this portal ({CONTRACT_VERSION}). Do not record: set record_rollouts 0, "
                                       f"or update the portal (AUTOMATED POST-TRAINING.md §9)")
                rollout = RolloutRecorder(pt_dir, rid, {
                    "source": "dell_policy_portal", "mock": mock, "checkpoint": ckpt, "params": dict(self.params),
                    "task": contract.get("task"), "joint_names": JOINTS, "camera": self.args.camera_name}, self.FPS)
            self.rollout = rollout
            self.planner = Planner(self.policy)
            self.run = {"id": rid, "checkpoint": ckpt, "steps": 0, "elapsed_s": 0.0, "starved": 0, "skipped_avg": None,
                        "overruns": 0, "stop_reason": None, "interventions": 0, "recorded_frames": 0,
                        "params": dict(self.params), "dir": self.runs_dir / rid}
            self.run["dir"].mkdir(parents=True, exist_ok=True)
            self.last_heartbeat = now()
            self.stop_flag.clear()
            self.correct_flag.clear()
            self._set("running", "Running policy.")
        threading.Thread(target=self._loop, daemon=True, name="control").start()

    def stop(self, reason="stopped by operator"):
        with self.lock:
            if self.state == "running":
                self.stop_flag.set()
                if self.run is not None:
                    self.run["stop_reason"] = self.run.get("stop_reason") or reason
            elif self.state == "moving":
                self.stop_flag.set()

    def estop(self):
        if self.arm:
            self.stop_flag.set()
            self.arm.estop()
            self._set("estopped", "EMERGENCY STOP sent. Recover per README §4/§9 before enabling again.")

    def request_correction(self):
        """[DELL/MAC] explicit, deterministic intervention. The operator pressed the drag-teach button AND this
        button (or C). The loop switches segment -> human on its next tick; nothing is guessed from the firmware."""
        with self.lock:
            if self.state != "running":
                raise RuntimeError("not running")
            self.correct_flag.set()
            self.message = "Correction requested — finish the task by hand, then Stop (Space) and mark Success/Fail."

    def mark(self, outcome: str, case: str, note: str):
        r = self.run
        if not r:
            raise RuntimeError("no run to mark")
        self.results_path.parent.mkdir(parents=True, exist_ok=True)
        # [DELL/MAC] one row per run, first label wins (matches the rollout's READY). The portal used to append a
        # row on every Success/Fail click, so a double-click produced duplicate trials; the Mac had to dedupe by
        # run_id with "last label wins", which then contradicted READY ("first wins"). Recording once removes both.
        if self.results_path.exists():
            with open(self.results_path, newline="", encoding="utf-8") as f:
                for row in csv.DictReader(f):
                    if row.get("run_id") == r["id"]:
                        self.message = (f"Already recorded {row.get('outcome')} (case {row.get('case')}) for "
                                        f"{r['checkpoint']} — the first label stands.")
                        return
        new = not self.results_path.exists()
        with open(self.results_path, "a", newline="", encoding="utf-8") as f:
            w = csv.writer(f)
            if new:
                w.writerow(["run_id", "checkpoint", "case", "outcome", "duration_s", "steps", "stop_reason",
                            "replan_every", "crossfade", "speed_pct", "step_limit_x", "gripper_effort", "latency_ms", "note"])
            lat = [x * 1000 for x in self.planner.latencies] if self.planner else []
            p = r["params"]
            w.writerow([r["id"], r["checkpoint"], case, outcome, r["elapsed_s"], r["steps"], r["stop_reason"],
                        p["replan_every"], p["crossfade"], p["speed_pct"], p["step_limit_x"], p["gripper_effort"],
                        round(sum(lat) / len(lat)) if lat else "", note])
        (r["dir"] / "result.json").write_text(json.dumps({"outcome": outcome, "case": case, "note": note}, indent=2))
        self.message = f"Recorded {outcome} (case {case}) for {r['checkpoint']}."
        rec = self.rollout
        if rec is not None and rec.meta.get("run_id") == r["id"] and r.get("stop_reason"):
            if rec.finalize(outcome, case, note):
                r["labelled"] = outcome
                where = "local dry-run folder (not synced)" if rec.meta.get("mock") else "posttrain/rollouts → Mac"
                self.message += f" Rollout labelled and released to {where}."
            else:
                self.message += " (Rollout was already labelled; the first label stands.)"

    def _current_policy(self):
        try:
            return json.loads((Path(self.args.policies_dir) / "CURRENT.json").read_text(encoding="utf-8")).get("name")
        except Exception:
            return None

    def set_params(self, upd: dict):
        with self.lock:
            for k, v in upd.items():
                if k not in LIMITS:
                    continue
                lo, hi, typ = LIMITS[k]
                self.params[k] = typ(min(hi, max(lo, float(v))))  # float() first: int("1.5") would raise
            if self.policy:
                self.params["replan_every"] = min(self.params["replan_every"], self.policy.chunk_size)

    def results(self):
        if not self.results_path.exists():
            return []
        with open(self.results_path, newline="", encoding="utf-8") as f:
            return list(csv.DictReader(f))[-50:]

    def _need(self, connected=False, enabled=False, policy=False):
        if connected and self.arm is None:
            raise RuntimeError("connect first")
        if enabled and not self.arm.enabled:
            raise RuntimeError("enable the motors first")
        if policy and self.policy is None:
            raise RuntimeError("load a policy first")

    # ---- background ----
    def _monitor(self):
        while True:
            try:
                arm = self.arm
                if arm is not None and self.state != "running":
                    self.joints, self.arm_age = arm.read()
                    # [DELL, seen 09-14] Operator pressed the teach button after Enable to re-pose the arm: motors are
                    # free again but `enabled` stayed True, so the Enable button was greyed out while Run refused with
                    # "drag-teach: press Enable". Drop back to connected so Enable holds the NEW pose.
                    if (not self.args.mock_arm and arm.enabled and self.state in ("enabled", "stopped", "estopped")
                            and arm.ctrl_mode() == TEACHING):
                        arm.enabled = False
                        self._set("connected", "Arm is in drag-teach (motors free). Pose it, then press Enable to hold "
                                               "that pose.")
            except Exception as e:
                self.message = f"Arm read failed: {e}"
            time.sleep(0.1)

    def _loop(self):
        p, r, pl = self.params, self.run, self.planner
        dt = 1.0 / self.FPS
        log = open(r["dir"] / "steps.jsonl", "w", encoding="utf-8")
        t_start = now()
        next_t = t_start
        k = 0
        skipped = []
        last_sent = None
        grip = {}  # gripper latch state for this run
        segment = "policy"
        rec = self.rollout
        stale_since = None
        cam_stale_since = None
        reason = None
        try:
            state, _ = self.arm.read()
            img, _ = self.cam.latest()
            pl.request(0, state, img)
            while pl.ready is None and not self.stop_flag.is_set():  # first chunk synchronously
                if pl.error:
                    raise RuntimeError(pl.error)
                time.sleep(0.005)
            # [DELL] clock starts when the first plan exists (~350 ms here), otherwise the first ticks all
            # count as overruns and max_duration includes the wait
            t_start = now()
            next_t = t_start
            while not self.stop_flag.is_set():
                tick = now()
                state, arm_age = self.arm.read()
                img, cam_age = self.cam.latest()
                self.joints, self.arm_age = state, arm_age
                if pl.error:
                    reason = pl.error; break
                # [DELL/MAC] intervention FIRST — before any stale break and independent of feedback freshness.
                # Primary: the explicit "Start correction" button / C key (deterministic). Backup: auto-detect ctrl_mode.
                if segment == "policy":
                    explicit = self.correct_flag.is_set()
                    auto = (not self.args.mock_arm and self.arm.ctrl_mode_raw() == TEACHING)
                    if explicit or auto:
                        self.correct_flag.clear()
                        if rec is not None and p["intervene_on_teach"]:
                            segment = "human"
                            r["interventions"] += 1
                            r["intervened_at_s"] = round(tick - t_start, 2)
                            self.message = "HUMAN CORRECTION recording: drag the arm to finish, then Stop (Space) and mark Success/Fail."
                        else:
                            reason = "arm entered drag-teach mode"; break
                r["segment"] = segment  # persist immediately: the switch must survive even if a later check breaks
                if tick - self.last_heartbeat > p["heartbeat_s"]:
                    reason = "portal page stopped responding (heartbeat)"; break
                if tick - t_start > p["max_duration_s"]:
                    reason = f"reached max duration {p['max_duration_s']} s"; break
                fault = self.arm.fault()
                if fault:
                    reason = fault; break
                # Staleness is only FATAL during a policy segment. In a human segment the arm is free (drag-teach); a
                # feedback blip there is a data problem, not a reason to kill the correction (README §3: TEACHING still
                # streams, but treat a blip as expected — MAC request).
                arm_stale = arm_age * 1000 > p["stale_arm_ms"]
                if arm_stale:
                    if stale_since is None:
                        stale_since = tick
                    if segment == "policy" and tick - stale_since > p["stale_arm_grace_s"]:
                        reason = f"arm feedback {arm_age*1000:.0f} ms old for {tick - stale_since:.1f}s"; break
                else:
                    stale_since = None
                cam_stale = cam_age * 1000 > p["stale_camera_ms"]
                if cam_stale:
                    if cam_stale_since is None:
                        cam_stale_since = tick
                    if segment == "policy" and tick - cam_stale_since > p["stale_camera_grace_s"]:
                        reason = f"camera frame {cam_age*1000:.0f} ms old for {tick - cam_stale_since:.1f}s"; break
                else:
                    cam_stale_since = None
                hold_this_tick = arm_stale or cam_stale
                r["segment"] = segment

                raw, sent, gmode = None, None, "human"
                if segment == "policy":
                    if hold_this_tick:
                        # [DELL] stale feedback/camera blip: hold the last command, do NOT consume the plan. Sending on
                        # a stale read is the hazard the hold guard avoids; a sub-second CPU stall must not end the run
                        # (only a sustained one does, via the grace periods).
                        gmode = "stale"
                    else:
                        sk = pl.integrate(k, p["crossfade"])
                        if sk is not None:
                            skipped.append(sk)
                        due = pl.last_obs_step is None or k - pl.last_obs_step >= p["replan_every"] or pl.remaining(k) <= 3
                        if due:
                            pl.request(k, state, img)
                        raw = pl.take(k, p["ensemble"])
                        if raw is None:
                            tgt = dict(last_sent or state)  # starved: hold the last command
                            gmode = "starved"
                        else:
                            tgt = self._clamp({j: float(raw[i]) for i, j in enumerate(JOINTS)}, state, last_sent)
                            tgt["gripper"], gmode = self._gripper(tgt["gripper"], state["gripper"], grip)
                        sent = tgt
                        self.arm.send(sent, p["speed_pct"], p["gripper_effort"], p["gripper_open_raw"])
                        last_sent = sent
                        self.target = sent
                gf = self.arm.grip_feedback()  # [DELL] effort + FOC status bits: why the gripper vibrates (overcurrent/overheat)
                st = self.arm.status_raw()     # [DELL] [ctrl_mode, arm_status] per step: intervention + soft-fault diagnosis
                t_rel = round(tick - t_start, 4)
                log.write(json.dumps({"k": k, "t": t_rel, "segment": segment, "state": {j: round(state[j], 3) for j in JOINTS},
                                      "raw": None if raw is None else [round(float(x), 3) for x in raw],
                                      "target": None if sent is None else {j: round(sent[j], 3) for j in JOINTS},
                                      "cam_age_ms": round(cam_age * 1000), "arm_age_ms": round(arm_age * 1000),
                                      "obs_step": pl.last_obs_step, "ens": pl.ens_n, "grip": gmode, "gf": gf, "st": st}) + "\n")
                if rec is not None:
                    rec.add({"k": k, "t": t_rel, "segment": segment,
                             "state": [round(float(state[j]), 4) for j in JOINTS],
                             "sent": None if sent is None else [round(float(sent[j]), 4) for j in JOINTS],
                             "plan": None if raw is None else [round(float(x), 4) for x in raw],
                             "grip": gmode, "gf": gf, "st": st, "cam_age_ms": round(cam_age * 1000), "obs_step": pl.last_obs_step}, img)
                    r["recorded_frames"] = rec.frames
                k += 1
                r.update(steps=k, elapsed_s=round(now() - t_start, 2), starved=pl.starved,
                         skipped_avg=round(sum(skipped) / len(skipped), 1) if skipped else None)
                next_t += dt
                slack = next_t - now()
                if slack > 0:
                    time.sleep(slack)
                else:
                    r["overruns"] += 1
                    next_t = now()
        except Exception as e:
            reason = f"control loop error: {e}"
            traceback.print_exc()
        finally:
            log.close()
            try:
                hold, hold_age = self.arm.read()
                if segment == "human":
                    self.arm.enabled = False  # the operator holds the arm in drag-teach: send nothing (a mode
                    # command would pull the arm out of drag-teach and stiffen it in their hands)
                elif hold_age < 0.5:  # [DELL] never command a stale pose; without feedback the last target stands
                    if hold["gripper"] < 95.0:
                        hold["gripper"] = max(0.0, hold["gripper"] - p["gripper_squeeze"])  # keep a light grip from the MEASURED width
                    self.arm.send(hold, 20, p["gripper_effort"], p["gripper_open_raw"])  # freeze, torque stays on
                    self.target = hold
            except Exception:
                pass
            r["stop_reason"] = r.get("stop_reason") or reason or "stopped"
            lat = [x * 1000 for x in pl.latencies]
            summary = {k2: (str(v) if isinstance(v, Path) else v) for k2, v in r.items()}
            summary["latency_ms_mean"] = round(sum(lat) / len(lat)) if lat else None
            (r["dir"] / "summary.json").write_text(json.dumps(summary, indent=2))
            if rec is not None:
                try:
                    rec.close({k2: summary.get(k2) for k2 in ("steps", "elapsed_s", "stop_reason", "interventions",
                                                               "intervened_at_s", "latency_ms_mean", "overruns")})
                    r["recorded_frames"] = rec.frames
                except Exception as e:
                    print(f"rollout close failed: {e}", flush=True)
            if self.state != "estopped":
                held = "Motors free (drag-teach)." if segment == "human" else "Holding position."
                self._set("stopped", f"Run ended: {r['stop_reason']}. {held} Mark Success/Fail"
                                     f"{' to release the rollout for post-training' if rec is not None else ''}.")
            self.stop_flag.clear()


# =====================================================================================================================
# Web
# =====================================================================================================================
def make_handler(ctl: Controller):
    class H(BaseHTTPRequestHandler):
        def log_message(self, *a):
            pass

        def _json(self, obj, code=200):
            body = json.dumps(obj).encode()
            self.send_response(code)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.send_header("Cache-Control", "no-store")
            self.end_headers()
            self.wfile.write(body)

        def do_GET(self):
            if self.path in ("/", "/index.html"):
                body = PAGE.encode()
                self.send_response(200)
                self.send_header("Content-Type", "text/html; charset=utf-8")
                self.send_header("Content-Length", str(len(body)))
                self.end_headers()
                self.wfile.write(body)
            elif self.path.startswith("/api/state"):
                self._json(ctl.snapshot())
            elif self.path.startswith("/api/results"):
                self._json(ctl.results())
            elif self.path.startswith("/frame.jpg"):
                import cv2
                img = ctl.cam.latest()[0] if ctl.cam else None
                if img is None:
                    return self.send_error(HTTPStatus.NO_CONTENT)
                small = cv2.resize(img, (320, 240))
                ok, jpg = cv2.imencode(".jpg", cv2.cvtColor(small, cv2.COLOR_RGB2BGR), [cv2.IMWRITE_JPEG_QUALITY, 70])
                body = jpg.tobytes()
                self.send_response(200)
                self.send_header("Content-Type", "image/jpeg")
                self.send_header("Content-Length", str(len(body)))
                self.send_header("Cache-Control", "no-store")
                self.end_headers()
                self.wfile.write(body)
            else:
                self.send_error(404)

        def do_POST(self):
            n = int(self.headers.get("Content-Length") or 0)
            data = json.loads(self.rfile.read(n) or b"{}")
            route = self.path.split("?")[0].removeprefix("/api/")
            try:
                if route == "heartbeat":
                    ctl.last_heartbeat = now()
                elif route == "connect":
                    ctl.connect()
                elif route == "load":
                    threading.Thread(target=self._safe, args=(ctl.load, data["checkpoint"]), daemon=True).start()
                elif route == "preview":
                    return self._json({"ok": True, "preview": ctl.preview()})
                elif route == "enable":
                    ctl.enable()
                elif route == "start_pose":
                    ctl.goto_start()
                elif route == "run":
                    ctl.start_run()
                elif route == "stop":
                    ctl.stop()
                elif route == "correct":
                    ctl.request_correction()
                elif route == "estop":
                    ctl.estop()
                elif route == "params":
                    ctl.set_params(data)
                elif route == "mark":
                    ctl.mark(data["outcome"], data.get("case", "A"), data.get("note", ""))
                elif route == "shutdown":
                    ctl.stop("portal shutdown")
                    # [DELL, measured] NOT os.kill(getpid(), SIGINT): on Windows that is TerminateProcess(exit 2), the
                    # signal handler never runs, the CAN bus is never closed, and a hard-killed candleLight can wedge
                    # until replugged (UPDATE 09-12). Call the same clean shutdown the signal handlers use.
                    threading.Thread(target=lambda: (time.sleep(0.5), ctl.shutdown_fn()), daemon=True).start()
                else:
                    return self._json({"ok": False, "error": "unknown route"}, 404)
                self._json({"ok": True})
            except Exception as e:
                ctl.message = str(e)
                self._json({"ok": False, "error": str(e)}, 400)

        @staticmethod
        def _safe(fn, *a):
            try:
                fn(*a)
            except Exception as e:
                ctl.message = f"{fn.__name__} failed: {e}"
                traceback.print_exc()

    return H


PAGE = r"""<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>PiPER · Policy Execution</title>
<style>
:root{--page:#f2f1ec;--surface:#fbfaf7;--ink:#111110;--ink2:#55534e;--muted:#8a8882;--hair:#e3e1da;--axis:#c8c5bc;--accent:#e0461a;--blue:#2a5caa;--good:#0ca30c;--bad:#d03b3b;--warn:#c98500}
*{box-sizing:border-box}body{margin:0;background:var(--page);color:var(--ink);font:14px/1.45 "Helvetica Neue",Helvetica,Arial,system-ui,sans-serif;-webkit-font-smoothing:antialiased}
.page{max-width:1320px;margin:0 auto;padding:0 32px 48px}.grid{display:grid;grid-template-columns:repeat(12,minmax(0,1fr));gap:20px}
.label{font-size:11px;font-weight:500;letter-spacing:.08em;text-transform:uppercase;color:var(--muted)}
header{display:flex;justify-content:space-between;align-items:center;padding:18px 0;border-bottom:1px solid var(--ink)}
h1{margin:22px 0 4px;font-size:40px;letter-spacing:-.03em;font-weight:600}.sub{color:var(--ink2);margin:0 0 20px}
.state{display:inline-flex;gap:8px;align-items:center;font-weight:600}.dot{width:9px;height:9px;border-radius:50%;background:var(--muted)}
.msg{font-size:15px;padding:12px 0;border-top:1px solid var(--ink);border-bottom:1px solid var(--hair);margin-bottom:20px;min-height:48px}
.card{background:var(--surface);border:1px solid rgba(17,17,16,.08);padding:18px}.c3{grid-column:span 3}.c4{grid-column:span 4}.c5{grid-column:span 5}.c7{grid-column:span 7}.c8{grid-column:span 8}.c12{grid-column:span 12}
.kv{display:grid;grid-template-columns:1fr auto;gap:6px 12px;font-variant-numeric:tabular-nums}.kv b{font-weight:500}
.ok{color:var(--good)}.bad{color:var(--bad)}.warn{color:var(--warn)}
button{font:inherit;border:1px solid var(--ink);background:transparent;padding:9px 14px;cursor:pointer}button:hover:not(:disabled){background:var(--ink);color:var(--surface)}
button:disabled{opacity:.35;cursor:not-allowed}button.primary{background:var(--ink);color:var(--surface)}button.run{background:var(--accent);border-color:var(--accent);color:#fff;font-weight:600}
button.estop{background:var(--bad);border-color:var(--bad);color:#fff;font-weight:700;width:100%;padding:14px}
.steps{display:flex;flex-wrap:wrap;gap:8px;align-items:center}.steps .n{font-size:11px;color:var(--accent);margin-right:2px}
select,input{font:inherit;padding:7px;border:1px solid var(--axis);background:#fff}
.bars{display:grid;grid-template-columns:70px 1fr 64px;gap:6px 10px;align-items:center;font-variant-numeric:tabular-nums;font-size:12px}
.bar{position:relative;height:10px;background:var(--hair)}.bar i{position:absolute;top:0;bottom:0;width:2px;background:var(--ink)}.bar s{position:absolute;top:-3px;bottom:-3px;width:2px;background:var(--accent)}
.bar em{position:absolute;top:0;bottom:0;background:rgba(17,17,16,.06)}
.params{display:grid;grid-template-columns:1fr 90px;gap:8px 12px;align-items:center}.params small{display:block;color:var(--muted);font-size:11px}
img.cam{width:100%;aspect-ratio:4/3;background:#222;display:block}
table{width:100%;border-collapse:collapse;font-size:12px;font-variant-numeric:tabular-nums}th{text-align:left;border-bottom:1px solid var(--ink);padding:6px}td{padding:6px;border-bottom:1px solid var(--hair)}
.mock{background:var(--warn);color:#fff;padding:2px 8px;font-size:11px;font-weight:600;letter-spacing:.06em}
@media(max-width:1000px){.c3,.c4,.c5,.c7,.c8{grid-column:span 12}}
</style></head><body><div class="page">
<header><div class="label">PiPER <span style="color:var(--muted)">/</span> Policy execution <span id="mock"></span></div><div class="state"><span class="dot" id="dot"></span><span id="state">—</span></div></header>
<h1>Run a trained policy</h1><p class="sub">ACT checkpoint → wrist camera + joints → 15 Hz joint targets. Space = run / stop · Esc = stop · C = start correction.</p>
<div class="msg" id="msg">—</div>
<div class="grid">
 <div class="card c8">
  <div class="label" style="margin-bottom:10px">Sequence</div>
  <div class="steps">
   <span class="n">1</span><button id="b-connect">Connect</button>
   <span class="n">2</span><select id="ckpt"></select><button id="b-load">Load policy</button>
   <span class="n">3</span><button id="b-enable">Enable motors</button>
   <span class="n">4</span><button id="b-start">Start pose</button>
   <span class="n">5</span><button id="b-run" class="run">Run</button><button id="b-stop" class="primary">Stop</button><button id="b-correct">Start correction</button>
  </div>
  <div class="steps" style="margin-top:14px">
   <span class="label" style="margin-right:6px">Result</span>
   <select id="case"><option value="A">Case A · block visible</option><option value="B">Case B · search right</option></select>
   <button id="b-ok">Success</button><button id="b-fail">Fail</button><input id="note" placeholder="Note (optional)" style="flex:1;min-width:160px">
  </div>
 </div>
 <div class="card c4"><button class="estop" id="b-estop">EMERGENCY STOP</button><p style="font-size:12px;color:var(--ink2);margin:10px 0 0">Software stop only. Keep a hand on the physical e-stop during every run.</p></div>

 <div class="card c3"><div class="label">Arm</div><div class="kv" style="margin-top:10px" id="arm"></div></div>
 <div class="card c3"><div class="label">Camera</div><div class="kv" style="margin-top:10px" id="camkv"></div></div>
 <div class="card c3"><div class="label">Policy</div><div class="kv" style="margin-top:10px" id="pol"></div></div>
 <div class="card c3"><div class="label">Current run</div><div class="kv" style="margin-top:10px" id="run"></div></div>

 <div class="card c12"><div class="label" style="margin-bottom:10px">Scene check · no motion · runs the model on the live view and shows what it would do</div>
  <div class="steps"><button id="b-preview">Check scene</button><label style="display:flex;gap:6px;align-items:center"><input type="checkbox" id="pv-repeat"> repeat every 3 s (while adjusting light / block)</label>
  <span id="pv" style="font-size:15px;font-weight:600"></span></div>
  <div id="pv-detail" style="font-size:12px;color:var(--ink2);margin-top:8px;font-variant-numeric:tabular-nums"></div>
  <p style="font-size:12px;color:var(--ink2);margin:8px 0 0">At the rest pose the demos' own camera frames score ~57 (model rises); below 8 the model will likely sit still. Fix the scene first: same lamp light as recording, curtains closed, nothing new in view. Alternative start: in drag-teach, drag the arm up so the camera sees the table, check again, then Enable (holds there) and Run.</p></div>
 <div class="card c5"><div class="label" style="margin-bottom:10px">Wrist camera</div><img class="cam" id="cam" alt=""></div>
 <div class="card c7"><div class="label" style="margin-bottom:12px">Joints · <span style="color:var(--ink)">▮ measured</span> <span style="color:var(--accent)">▮ target</span> <span style="color:var(--muted)">▯ demonstrated range</span></div><div class="bars" id="bars"></div></div>

 <div class="card c7"><div class="label" style="margin-bottom:12px">Runtime parameters</div><div class="params" id="params"></div></div>
 <div class="card c5"><div class="label" style="margin-bottom:10px">Recent results</div><div style="max-height:360px;overflow:auto"><table><thead><tr><th>Checkpoint</th><th>Case</th><th>Outcome</th><th>s</th></tr></thead><tbody id="results"></tbody></table></div></div>
</div></div>
<script>
const PARAM_DOC={replan_every:"Steps between new plans. Lower reacts faster, costs more CPU",crossfade:"Steps blended into each new plan (0 = hard switch)",speed_pct:"Firmware joint speed limit, %",step_limit_x:"Per-step change cap × demo p99 (0 = off)",envelope_margin:"Allowed units beyond the demonstrated joint range",gripper_effort:"Grip force, 0.001 N·m. 1000 crushed a printed cube; 300-600 for printed parts",gripper_squeeze:"While holding: command this much tighter than the measured cube width",gripper_open_raw:"Raw width for 'fully open' (68000 = calibration cap, demos opened to ~100000)",gripper_release_steps:"While holding, the plan must ask to open this many steps in a row (stops drops; 0 = off)",lead_limit_x:"Max command lead over the measured arm, x demo p99 (0 = off)",ensemble:"Average the newest N overlapping plans for joints (1 = off). Smooths plan switches",record_rollouts:"1 = save each run (video + joints) for post-training; released to the Mac when you mark Success/Fail",intervene_on_teach:"1 = pressing the drag-teach button mid-run records your hand correction instead of stopping",max_duration_s:"Run stops by itself after this",stale_camera_ms:"Stop if camera frame older than",stale_arm_ms:"Stop if joint feedback older than",heartbeat_s:"Stop if this page goes silent for"};
const $=id=>document.getElementById(id);let S=null,paramsBuilt=false;
async function post(r,b){const x=await fetch('/api/'+r,{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(b||{})});const j=await x.json();if(!j.ok)$('msg').textContent=j.error;refresh();return j}
function kv(el,rows){el.innerHTML=rows.map(([k,v,c])=>`<span>${k}</span><b class="${c||''}">${v??'—'}</b>`).join('')}
function buildParams(p,l){$('params').innerHTML=Object.keys(p).map(k=>`<label>${k.replace(/_/g,' ')}<small>${PARAM_DOC[k]||''} · ${l[k][0]}–${l[k][1]}</small></label><input type="number" step="any" id="p-${k}" value="${p[k]}">`).join('');
 Object.keys(p).forEach(k=>$('p-'+k).addEventListener('change',e=>post('params',{[k]:e.target.value})));paramsBuilt=true}
async function refresh(){try{S=await (await fetch('/api/state')).json()}catch(e){$('state').textContent='portal offline';return}
 const st=S.state;$('state').textContent=st;$('dot').style.background={running:'var(--accent)',enabled:'var(--good)',stopped:'var(--ink)',estopped:'var(--bad)',moving:'var(--blue)'}[st]||'var(--muted)';
 $('msg').textContent=S.message;$('mock').innerHTML=(S.mock.arm||S.mock.camera)?' <span class="mock">MOCK '+[S.mock.arm?'ARM':'',S.mock.camera?'CAMERA':''].filter(Boolean).join(' + ')+'</span>':'';
 const a=S.arm,c=S.camera,p=S.policy,r=S.run;
 const MODES={0:'STANDBY (silent)',1:'CAN control',2:'DRAG-TEACH'};
 kv($('arm'),[['Connected',a.connected?'yes':'no',a.connected?'ok':''],['Motors',a.enabled?'enabled':'off',a.enabled?'ok':''],['Mode',a.ctrl_mode==null?(a.connected?'no status':'—'):(MODES[a.ctrl_mode]||'0x'+a.ctrl_mode.toString(16).padStart(2,'0')),a.ctrl_mode===1?'ok':(a.connected?'warn':'')],['Feedback',!a.connected?'—':(a.feedback_live?a.feedback_age_ms+' ms':'NONE / STALE'),a.connected&&!a.feedback_live?'bad':'ok']]);
 kv($('camkv'),[['Frame rate',c.fps+' fps',c.fps<5?'bad':(c.fps<10?'warn':'ok')],['Frame age',c.age_ms==null?'—':c.age_ms+' ms',c.age_ms>300?'bad':''],['Error',c.error||'none',c.error?'bad':'']]);
 kv($('pol'),[['Checkpoint',p.name||'—'],['Chunk',p.chunk_size?p.chunk_size+' steps':'—'],['Inference',p.latency_ms?p.latency_ms+' ms':'—',p.latency_ms>450?'warn':''],['Worst',p.latency_max_ms?p.latency_max_ms+' ms':'—']]);
 kv($('run'),[['Steps',r.steps],['Elapsed',r.elapsed_s!=null?r.elapsed_s+' s':null],['Stale actions skipped',r.skipped_avg],['Starved / overruns',r.steps!=null?(r.starved+' / '+r.overruns):null],['Segment',r.segment==='human'?'HUMAN CORRECTION':r.segment,r.segment==='human'?'warn':''],['Interventions',r.interventions],['Recorded frames',r.recorded_frames],['Rollout',r.labelled?('released: '+r.labelled):(r.recorded_frames?'waiting for Success/Fail':null),r.labelled?'ok':'warn'],['Stop reason',r.stop_reason]]);
 const sel=$('ckpt');const names=S.checkpoints.join('|');if(sel.dataset.n!==names){sel.innerHTML=S.checkpoints.map(n=>`<option>${n}</option>`).join('');sel.value=(S.current_policy&&S.checkpoints.includes(S.current_policy))?S.current_policy:(S.checkpoints[S.checkpoints.length-1]||'');sel.dataset.n=names}
 if(!paramsBuilt)buildParams(S.params,S.limits);else Object.keys(S.params).forEach(k=>{const el=$('p-'+k);if(document.activeElement!==el)el.value=S.params[k]});
 const J=['joint1','joint2','joint3','joint4','joint5','joint6','gripper'];
 $('bars').innerHTML=J.map(j=>{const lo=j==='gripper'?0:-100,span=100-lo,pc=v=>((v-lo)/span*100).toFixed(2)+'%';const m=a.joints?a.joints[j]:null,t=a.target?a.target[j]:null,e=S.envelope[j];
  return `<span>${j}</span><div class="bar"><em style="left:${pc(e[0])};width:calc(${pc(e[1])} - ${pc(e[0])})"></em>${m!=null?`<i style="left:${pc(m)}"></i>`:''}${t!=null?`<s style="left:${pc(t)}"></s>`:''}</div><span style="text-align:right">${m!=null?m.toFixed(1):'—'}</span>`}).join('');
 showPreview();$('b-preview').disabled=!(a.connected&&p.loaded)||st==='running'||st==='moving';
 const busy=st==='running'||st==='moving';$('b-connect').disabled=busy||(a.connected&&a.feedback_live);$('b-connect').textContent=a.connected&&!a.feedback_live?'Reconnect':'Connect';$('b-load').disabled=busy||!S.checkpoints.length;$('b-enable').disabled=!a.connected||a.enabled||busy;
 $('b-start').disabled=!a.enabled||busy;$('b-run').disabled=!a.enabled||!p.loaded||busy;$('b-stop').disabled=!busy;$('b-correct').disabled=!(st==='running'&&r.segment==='policy');$('b-ok').disabled=$('b-fail').disabled=!r.id||busy;}
async function results(){const rows=await (await fetch('/api/results')).json();$('results').innerHTML=rows.reverse().map(x=>`<tr><td>${x.checkpoint}</td><td>${x.case}</td><td class="${x.outcome==='success'?'ok':'bad'}">${x.outcome}</td><td>${x.duration_s}</td></tr>`).join('')}
$('b-connect').onclick=()=>post('connect');$('b-load').onclick=()=>post('load',{checkpoint:$('ckpt').value});$('b-enable').onclick=()=>{if(confirm('Enable motors? The arm stiffens and holds its current pose (this also leaves drag-teach). Hand on the e-stop.'))post('enable')};
$('b-start').onclick=()=>{if(confirm('Move slowly to the start pose (rest pose, forearm forward, gripper closed)? Keep the workspace clear.'))post('start_pose')};
let pvBusy=false;async function preview(){if(pvBusy||!S||!S.policy.loaded||!S.arm.connected||S.state==='running'||S.state==='moving')return;pvBusy=true;try{await post('preview')}finally{pvBusy=false}}
function showPreview(){const v=S.preview;if(!v){$('pv').textContent='';$('pv-detail').textContent='';return}
 $('pv').innerHTML=`<span class="${v.good?'ok':(v.weak?'warn':'bad')}">${v.score} · ${v.verdict}</span>`;
 $('pv-detail').textContent=`${v.at} · planned change in 2 s: `+Object.entries(v.change_2s).map(([k,x])=>`${k} ${x>0?'+':''}${x}`).join('  ')+` · image brightness ${v.brightness} (demos ≈0.65 at rest, ≈0.5 over the table) · ${v.ms} ms`}
$('b-preview').onclick=preview;setInterval(()=>{if($('pv-repeat').checked)preview()},3000);
$('b-run').onclick=()=>post('run');$('b-stop').onclick=()=>post('stop');$('b-correct').onclick=()=>post('correct');$('b-estop').onclick=()=>post('estop');
$('b-ok').onclick=()=>post('mark',{outcome:'success',case:$('case').value,note:$('note').value}).then(results);$('b-fail').onclick=()=>post('mark',{outcome:'fail',case:$('case').value,note:$('note').value}).then(results);
// [DELL] A clicked button keeps focus, and the browser "clicks" a focused button on Space/Enter keyup. Pressing Space
// to STOP right after clicking Run would therefore start a new run. Buttons drop focus after every click, and Space
// keyup is swallowed.
document.querySelectorAll('button').forEach(b=>b.addEventListener('click',()=>b.blur()));
document.addEventListener('keyup',e=>{if(e.code==='Space'&&e.target.tagName!=='INPUT')e.preventDefault()});
document.addEventListener('keydown',e=>{if(e.target.tagName==='INPUT'||e.target.tagName==='SELECT')return;if(e.code==='Space'){e.preventDefault();if(document.activeElement&&document.activeElement.blur)document.activeElement.blur();if(e.repeat||!S)return;if(S.state==='running')post('stop');else if(!$('b-run').disabled)post('run')}if(e.key==='Escape')post('stop');if((e.key==='c'||e.key==='C')&&S&&S.state==='running')post('correct')});
setInterval(refresh,250);setInterval(()=>post('heartbeat').catch(()=>{}),500);setInterval(()=>{$('cam').src='/frame.jpg?'+Date.now()},200);refresh();results();
</script></body></html>"""


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--policies-dir", default=str(ROOT / "policies"))
    ap.add_argument("--dataset-root", default=str(ROOT / "datasets" / "piper_pick_place"))
    ap.add_argument("--host", default="127.0.0.1")
    ap.add_argument("--port", type=int, default=8791)
    ap.add_argument("--can-port", default="candle")
    ap.add_argument("--shim-dir", default=os.environ.get("PIPER_SHIM_DIR", r"D:\Piper-CAN-Teleop\scripts"))
    ap.add_argument("--camera-name", default="Dabai DC1")
    ap.add_argument("--camera-index", type=int, default=None, help="only if name lookup is impossible")
    ap.add_argument("--threads", type=int, default=max(1, (os.cpu_count() or 2) // 2), help="torch CPU threads (physical cores)")
    ap.add_argument("--mock-arm", action="store_true")
    ap.add_argument("--mock-camera", action="store_true")
    ap.add_argument("--inprocess", action="store_true", help="run inference in the portal process (slower with a real arm)")
    ap.add_argument("--posttrain-dir", default=str(ROOT / "posttrain"), help="rollout packages for the Mac (CONTRACT.json)")
    args = ap.parse_args()

    ctl = Controller(args)
    srv = ThreadingHTTPServer((args.host, args.port), make_handler(ctl))
    print(f"PiPER policy portal on http://{args.host}:{args.port}  (policies: {args.policies_dir})", flush=True)

    def shutdown(*_):
        ctl.stop("portal shutdown")
        time.sleep(0.3)
        if ctl.cam:
            ctl.cam.stop()
        if ctl.arm:
            ctl.arm.close()
        os._exit(0)

    ctl.shutdown_fn = shutdown

    for sig in ("SIGINT", "SIGTERM", "SIGBREAK"):
        if hasattr(signal, sig):
            signal.signal(getattr(signal, sig), shutdown)
    srv.serve_forever()


if __name__ == "__main__":
    main()
