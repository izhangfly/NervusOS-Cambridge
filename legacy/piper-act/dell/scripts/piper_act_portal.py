#!/usr/bin/env python3
"""PiPER ACT — unified portal: dashboard + data collection + policy execution.

One process, one CAN adapter, one camera set. Three pages on one HTTP server:

    /            dashboard   (arm live, current policy, dataset counts, recent results)
    /record      data collection (drag-teach recording straight into the Syncthing dataset)
    /policy      policy execution (load a checkpoint, Enable, Run, Correct, mark Success/Fail)

Run locally:      python piper_act_portal.py
Dry run:          python piper_act_portal.py --mock-arm --mock-camera

The arm and cameras are SHARED by the recorder and the policy controller, so they can
never fight over the single candleLight adapter the way the two old portals did. Recording
(drag-teach, arm read-only) and running (motors enabled) are mutually exclusive and the
portal enforces it. All heavy compute — ACT inference, camera capture, CAN parsing, video
encoding — happens here on the Dell; the remote server only relays this HTTP surface.

SAFETY: like the recorder, this never calls robot.connect() (which enables torque and drives
home) or disconnect() (which parks and drops). Torque is enabled only by the explicit Enable
button, and motion only by the explicit Run / Start-pose buttons.
"""
from __future__ import annotations

import argparse
import csv
import io
import json
import math
import os
import queue
import shutil
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
sys.path.insert(0, str(HERE))
sys.path.insert(0, str(HERE.parent / "vla"))
sys.path.insert(0, str(HERE.parent / "deploy"))
sys.path.append(str(HERE.parent))  # repo layout: dell/piper_policy_portal.py (lowest priority; the Dell imports the synced copy)

SHARED = Path(r"C:\Users\zhy10\Shared\Piper Arm")
DEFAULT_ROOT = SHARED / "datasets"
JOINTS = ["joint1", "joint2", "joint3", "joint4", "joint5", "joint6", "gripper"]
TEACHING = 0x02

DEMO_STEP_P99 = {"joint1": 3.70, "joint2": 6.51, "joint3": 6.75, "joint4": 4.76, "joint5": 11.39, "joint6": 4.84, "gripper": 16.66}

DEFAULT_PARAMS = {
    "replan_every": 10, "crossfade": 4, "speed_pct": 40, "step_limit_x": 1.5, "lead_limit_x": 4.0,
    "ensemble": 2, "envelope_margin": 5.0, "gripper_effort": 500, "gripper_squeeze": 2.0,
    "gripper_open_raw": 95000, "gripper_release_steps": 8, "record_rollouts": 1, "intervene_on_teach": 1,
    "max_duration_s": 90, "stale_camera_ms": 500, "stale_arm_ms": 300, "stale_arm_grace_s": 1.0,
    "stale_camera_grace_s": 1.0, "heartbeat_s": 2.0,
}
LIMITS = {
    "replan_every": (1, 30, int), "crossfade": (0, 10, int), "speed_pct": (10, 100, int), "step_limit_x": (0.0, 5.0, float),
    "lead_limit_x": (0.0, 10.0, float), "ensemble": (1, 3, int),
    "envelope_margin": (0.0, 30.0, float), "gripper_effort": (100, 5000, int), "gripper_squeeze": (0.0, 30.0, float),
    "gripper_open_raw": (68000, 101000, int), "gripper_release_steps": (0, 45, int),
    "record_rollouts": (0, 1, int), "intervene_on_teach": (0, 1, int),
    "max_duration_s": (5, 600, int), "stale_camera_ms": (100, 3000, int), "stale_arm_ms": (100, 3000, int),
    "stale_arm_grace_s": (0.2, 3.0, float), "stale_camera_grace_s": (0.2, 3.0, float), "heartbeat_s": (0.5, 10.0, float),
}

CONTRACT_VERSION = 1


def now():
    return time.monotonic()


# =====================================================================================================================
# Arm — read-only recording surface (rx_count liveness, FK pose) + policy control surface (enable/send/estop).
# =====================================================================================================================
class Arm:
    """One arm object shared by the recorder and the policy controller.

    Read-only half (from the recorder): `connect` publishes the bus atomically and retires the old
    one first, liveness is the shim's rx_count (a cached get_action() reads as zeros and would
    otherwise pass for a live arm), and read_pose() computes FK once per frame instead of EnableFkCal.
    Control half (from the policy portal): enable() holds the measured pose and leaves drag-teach;
    send() commands joints + a throttled gripper with the 68 mm -> wide-open stretch.
    """

    RX_TIMEOUT = 2.0
    STATUS_FRESH_S = 0.5
    GRIP_KNEE = 90.0

    def __init__(self, port="candle", mock=False):
        self.mock = mock
        self.port = port
        self.robot = None
        self.error = None
        self.rx_rate = 0.0
        self.enabled = False
        self._t0 = time.time()
        self._last_rx = 0
        self._last_rx_t = 0.0
        self._last_seen = 0.0
        self._connecting = threading.Lock()
        self._fk = None
        self._grip_last = None

    # ---- accessors the control methods need ----
    @property
    def bus(self):
        return self.robot.bus if self.robot is not None else None

    @property
    def piper(self):
        return self.robot.bus.piper if self.robot is not None else None

    @staticmethod
    def _rx_count(robot):
        try:
            can = robot.bus.piper._C_PiperInterface_V2__arm_can
            return getattr(can.bus, "rx_count", None)
        except Exception:
            return None

    def read_ctrl_mode(self):
        robot = self.robot
        if self.mock or robot is None:
            return None
        try:
            s = robot.bus.piper.GetArmStatus().arm_status
            return getattr(s.ctrl_mode, "value", None)
        except Exception:
            return None

    def connect(self):
        if self.mock:
            self._last_seen = time.time()
            return True
        if not self._connecting.acquire(blocking=False):
            return False
        robot = None
        try:
            old, self.robot = self.robot, None
            if old is not None:
                try:
                    old.bus.disconnect(disable_torque=False)
                except Exception:
                    pass
                time.sleep(0.3)

            import piper_mac_can
            piper_mac_can.install()
            from lerobot_robot_piper.config_piper import PiperFollowerConfig
            from lerobot_robot_piper.piper_follower import PiperFollower

            robot = PiperFollower(PiperFollowerConfig(port=self.port))
            robot.bus.connect()  # opens CAN only; deliberately NOT robot.connect() (torque + park)

            start = self._rx_count(robot)
            if start is None:
                raise RuntimeError("cannot read shim rx_count — unexpected SDK layout")
            deadline = time.time() + 3.0
            while time.time() < deadline:
                time.sleep(0.15)
                now_n = self._rx_count(robot) or 0
                if now_n > start:
                    self._last_rx, self._last_rx_t = now_n, time.time()
                    self._last_seen = time.time()
                    self.error = None
                    self.robot = robot
                    return True
            try:
                s = robot.bus.piper.GetArmStatus().arm_status
                mode = getattr(s.ctrl_mode, "value", None)
            except Exception:
                mode = None
            hint = ("the arm is in STANDBY (0x00) — press the drag-teach button to put it in TEACHING."
                    if mode == 0x00 else "check 24V power, the base LED, and the CAN cable.")
            raise RuntimeError(f"CAN open but no frames from the arm — {hint}")
        except Exception as e:
            self.error = f"{type(e).__name__}: {e}"
            try:
                if robot is not None:
                    robot.bus.disconnect(disable_torque=False)
            except Exception:
                pass
            return False
        finally:
            self._connecting.release()

    def poll(self):
        robot = self.robot
        if self.mock or robot is None:
            return
        now_, n = time.time(), self._rx_count(robot)
        if n is None:
            return
        if n < self._last_rx:
            self._last_rx, self._last_rx_t = n, now_
            self._last_seen = now_
            return
        dt = now_ - self._last_rx_t
        if dt >= 0.5:
            self.rx_rate = (n - self._last_rx) / dt
            self._last_rx, self._last_rx_t = n, now_
            if self.rx_rate > 0:
                self._last_seen = now_

    def resync(self):
        if self.mock:
            self._last_seen = time.time()
            return
        robot = self.robot
        if robot is None:
            return
        n = self._rx_count(robot)
        if n is None:
            return
        if n != self._last_rx:
            self._last_seen = time.time()
        self._last_rx, self._last_rx_t = n, time.time()

    def read(self):
        """(normalized joints, age_s). age is inf when no frame has ever arrived (SDK cache would read as zeros)."""
        if self.mock:
            t = time.time() - self._t0
            return {n: 40.0 * np.sin(t * 0.7 + i) for i, n in enumerate(JOINTS)}, 0.0
        robot = self.robot
        if robot is None:
            return None, float("inf")
        vals = robot.bus.get_action()
        ts = getattr(robot.bus.piper.GetArmJointMsgs(), "time_stamp", 0) or 0
        age = max(0.0, time.time() - ts) if ts > 1e9 else float("inf")
        return vals, age

    def read_pose(self):
        if self.mock:
            t = time.time() - self._t0
            return {"x": 300 + 50 * np.sin(t), "y": 20 * np.cos(t), "z": 200.0,
                    "rx": 0.0, "ry": 85.0, "rz": 0.0, "gripper_raw": 0, "gripper_effort": 0, "fk": []}
        robot = self.robot
        if robot is None:
            return None
        try:
            piper = robot.bus.piper
            e = piper.GetArmEndPoseMsgs().end_pose
            g = piper.GetArmGripperMsgs().gripper_state
            try:
                if self._fk is None:
                    from piper_sdk import C_PiperForwardKinematics
                    self._fk = C_PiperForwardKinematics(0x01)
                j = piper.GetArmJointMsgs().joint_state
                k = 1000.0 * self._fk.RADIAN
                rad = [j.joint_1 / k, j.joint_2 / k, j.joint_3 / k, j.joint_4 / k, j.joint_5 / k, j.joint_6 / k]
                fk = [[round(float(v), 3) for v in link] for link in self._fk.CalFK(rad)]
            except Exception:
                fk = []
            return {"x": e.X_axis / 1000.0, "y": e.Y_axis / 1000.0, "z": e.Z_axis / 1000.0,
                    "rx": e.RX_axis / 1000.0, "ry": e.RY_axis / 1000.0, "rz": e.RZ_axis / 1000.0,
                    "gripper_raw": int(g.grippers_angle), "gripper_effort": int(g.grippers_effort), "fk": fk}
        except Exception:
            return None

    @property
    def connected(self):
        if self.mock:
            return True
        robot = self.robot
        return (robot is not None and robot.bus.is_connected and time.time() - self._last_seen <= self.RX_TIMEOUT)

    # ---- control surface (from the policy portal) ----
    def _status(self):
        st = self.piper.GetArmStatus()
        if not st.time_stamp or time.time() - st.time_stamp > self.STATUS_FRESH_S:
            return None
        return st.arm_status

    def ctrl_mode(self):
        try:
            s = self._status()
            return None if s is None else int(s.ctrl_mode)
        except Exception:
            return None

    def ctrl_mode_raw(self):
        try:
            return int(self.piper.GetArmStatus().arm_status.ctrl_mode)
        except Exception:
            return None

    def status_raw(self):
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
                return None
            code = int(s.arm_status)
            return None if code in (0x00, 0x02, 0x03, 0x04) else f"arm status 0x{code:02X}"
        except Exception:
            return None

    def grip_feedback(self):
        try:
            gs = self.piper.GetArmGripperMsgs().gripper_state
            return [int(gs.grippers_effort), int(gs.status_code)]
        except Exception:
            return None

    def enable(self, gripper_effort: int):
        if self.mock:
            self.enabled, self._last_seen = True, time.time()
            return
        _, age = self.read()
        if age > 0.3:
            raise RuntimeError("no live joint feedback (the arm stops streaming in STANDBY). Press the drag-teach "
                               "button once so its light is ON, then Enable again - the portal takes it out of drag-teach")
        self.bus.enable_torque()
        time.sleep(1.2)
        vals, age = self.read()
        if age > 0.3:
            raise RuntimeError("joint feedback stopped while enabling; not commanding anything")
        self.send(vals, 20, gripper_effort)
        deadline = time.time() + 3.0
        while time.time() < deadline:
            mode = self.ctrl_mode()
            if mode == 0x01:
                self.enabled = True
                return
            time.sleep(0.1)
        raise RuntimeError(f"arm did not switch to CAN control (ctrl_mode {mode}); press the drag-teach button "
                           f"so its light is off, then Enable again")

    def send(self, target: dict, speed_pct: int, gripper_effort: int, gripper_open_raw: int | None = None):
        if self.mock:
            return
        raw = self.bus._unnormalize(target)
        g = int(raw["gripper"])
        cal_max = self.bus.calibration["gripper"].range_max
        if gripper_open_raw and gripper_open_raw > cal_max and target["gripper"] > self.GRIP_KNEE:
            knee_raw = cal_max * self.GRIP_KNEE / 100.0
            g = int(knee_raw + (target["gripper"] - self.GRIP_KNEE) / (100.0 - self.GRIP_KNEE) * (gripper_open_raw - knee_raw))
        self.piper.ModeCtrl(0x01, 0x01, int(speed_pct), 0x00)
        self.piper.JointCtrl(*(int(raw[j]) for j in JOINTS[:6]))
        # gripper only on change (>= 0.3 mm / new effort / every 2 s): re-sending the same setpoint with 0x03
        # 15x/s while the fingers are stalled on the cube coincided with heavy gripper vibration.
        t = time.monotonic()
        if self._grip_last is None or abs(g - self._grip_last[0]) >= 300 or int(gripper_effort) != self._grip_last[1] or t - self._grip_last[2] >= 2.0:
            self.piper.GripperCtrl(abs(g), int(gripper_effort), 0x03, 0)
            self._grip_last = (g, int(gripper_effort), t)

    def estop(self):
        if not self.mock:
            self.piper.EmergencyStop(0x01)
        self.enabled = False

    def close(self):
        if self.robot is not None:
            try:
                self.robot.bus.disconnect(disable_torque=False)
            except Exception:
                pass
            self.robot = None


# =====================================================================================================================
# Camera — one RGB stream by name, reader thread, latest-frame + MJPEG.
# =====================================================================================================================
class Cam:
    STALE_AFTER = 1.0

    def __init__(self, key, name, width=640, height=480, mock=False):
        self.key, self.name = key, name
        self.width, self.height, self.mock = width, height, mock
        self.cap = None
        self.error = None
        self.jpeg = None
        self._latest = None
        self.rate = 0.0
        self._started = None
        self._t0 = time.time()
        self._stop = threading.Event()
        self._thread = None

    def start(self):
        if self._thread is None:
            self._started = time.time()
            self._thread = threading.Thread(target=self._loop, name=f"cam-{self.key}", daemon=True)
            self._thread.start()

    def degraded(self, min_fps):
        if self.mock or self._started is None or time.time() - self._started < 3.0:
            return False
        return self.rate < min_fps

    def _loop(self):
        n, t0 = 0, time.time()
        while not self._stop.is_set():
            if not self.connected:
                time.sleep(0.2)
                continue
            try:
                rgb = self._grab()
                self._latest = (rgb, time.time())
                n += 1
            except Exception as e:
                self.error = f"{type(e).__name__}: {e}"
                time.sleep(0.1)
            if time.time() - t0 >= 1.0:
                self.rate = n / (time.time() - t0)
                n, t0 = 0, time.time()

    def read_latest(self):
        got = self._latest
        if got is None:
            return None
        rgb, ts = got
        return None if time.time() - ts > self.STALE_AFTER else rgb

    def latest(self):
        """(rgb, age_s) for the policy controller; age inf when there is no frame yet."""
        got = self._latest
        if got is None:
            return None, float("inf")
        rgb, ts = got
        return rgb, time.time() - ts

    def fps(self):
        return self.rate

    @property
    def stale(self):
        return self.read_latest() is None

    def connect(self):
        if self.mock:
            return True
        try:
            import cv2
            from camera import find_camera
            idx = find_camera(self.name)
            self.cap = cv2.VideoCapture(idx, cv2.CAP_DSHOW)
            if not self.cap.isOpened():
                raise RuntimeError(f"could not open {self.name} (index {idx})")
            self.cap.set(cv2.CAP_PROP_FOURCC, cv2.VideoWriter_fourcc(*"MJPG"))
            self.cap.set(cv2.CAP_PROP_FRAME_WIDTH, self.width)
            self.cap.set(cv2.CAP_PROP_FRAME_HEIGHT, self.height)
            for _ in range(5):
                self.cap.read()
            self.error = None
            return True
        except Exception as e:
            self.error = f"{type(e).__name__}: {e}"
            self.cap = None
            return False

    def _grab(self):
        if self.mock:
            t = time.time() - self._t0
            img = np.zeros((self.height, self.width, 3), np.uint8)
            x = int((np.sin(t + hash(self.key) % 7) * 0.5 + 0.5) * (self.width - 60))
            y = int((np.cos(t * 0.7) * 0.5 + 0.5) * (self.height - 60))
            img[y:y + 60, x:x + 60] = (220, 60, 60)
            time.sleep(0.05)  # cap the synthetic rate so a mock run doesn't peg the CPU
            return img
        import cv2
        self.cap.grab()
        ok, frame = self.cap.read()
        if not ok or frame is None:
            raise RuntimeError(f"frame grab failed on {self.name}")
        if frame.shape[0] != self.height or frame.shape[1] != self.width:
            frame = cv2.resize(frame, (self.width, self.height))
        return cv2.cvtColor(frame, cv2.COLOR_BGR2RGB)

    @property
    def connected(self):
        return self.mock or self.cap is not None

    def close(self):
        self._stop.set()
        if self._thread is not None:
            self._thread.join(timeout=2.0)
            self._thread = None
        if self.cap is not None:
            try:
                self.cap.release()
            except Exception:
                pass
            self.cap = None


# =====================================================================================================================
# Recorder — drag-teach data collection, shared arm + cameras.
# =====================================================================================================================
class Recorder(threading.Thread):
    def __init__(self, args, arm, cams):
        super().__init__(daemon=True)
        self.args = args
        self.arm = arm
        self.cams = cams
        self.lock = threading.Lock()
        self.dataset = None
        self.dataset_error = None
        self.recording = False
        self.state = "starting"
        self.task = args.task
        self.ep_frames = 0
        self.ep_started = 0.0
        self.saved_episodes = 0
        self.last_msg = ""
        self.joints = {n: 0.0 for n in JOINTS}
        self.measured_fps = 0.0
        self.ctrl_mode = None
        self.cam_problem = None
        self._want_start = threading.Event()
        self._want_stop = threading.Event()
        self._discard = False
        self._stop_thread = threading.Event()
        self.arm_attempts = 0

    def _features(self):
        f = {"observation.state": {"dtype": "float32", "shape": (len(JOINTS),), "names": list(JOINTS)},
             "action": {"dtype": "float32", "shape": (len(JOINTS),), "names": list(JOINTS)}}
        for key in self.cams:
            f[f"observation.images.{key}"] = {"dtype": "video",
                                              "shape": (self.args.height, self.args.width, 3),
                                              "names": ["height", "width", "channels"]}
        return f

    def _open_dataset(self, quiet=False):
        from lerobot.datasets.lerobot_dataset import LeRobotDataset
        root = Path(self.args.root) / self.args.dataset
        repo_id = f"local/{self.args.dataset}"
        ep_dir = root / "meta" / "episodes"
        exists = ep_dir.exists() and any(ep_dir.rglob("*.parquet"))
        if not exists and (root / "meta" / "info.json").exists():
            payload = (any((root / "data").rglob("*.parquet")) or any((root / "videos").rglob("*.mp4")))
            if payload:
                raise RuntimeError(f"{root} holds recorded data but its episode metadata is missing. Move it aside.")
            shutil.rmtree(root)
        if exists:
            ds = LeRobotDataset(repo_id, root=root, vcodec=self.args.vcodec)
            have = {k for k in ds.features if k.startswith("observation.images.")}
            want = {f"observation.images.{k}" for k in self.cams}
            if have != want:
                raise RuntimeError(f"{root} was recorded with cameras {sorted(have)} but this run has {sorted(want)}. Use a new --dataset.")
            ds.start_image_writer(num_processes=0, num_threads=4 * len(self.cams))
            self.saved_episodes = ds.meta.total_episodes
            if not quiet:
                self.last_msg = f"resumed — {self.saved_episodes} episode(s) already recorded"
        else:
            ds = LeRobotDataset.create(repo_id, fps=self.args.fps, features=self._features(), root=root,
                                       robot_type="piper_follower", use_videos=True, image_writer_processes=0,
                                       image_writer_threads=4 * len(self.cams), vcodec=self.args.vcodec)
            self.last_msg = f"created new dataset at {root}"
        return ds

    def start_episode(self, task=None):
        with self.lock:
            if self.state != "idle":
                return False, f"cannot start while {self.state}"
            if task:
                self.task = task
        self._want_start.set()
        return True, "starting episode"

    def stop_episode(self, discard=False):
        with self.lock:
            if self.state != "recording":
                return False, "not recording"
            self._discard = discard
        self._want_stop.set()
        return True, "discarding episode" if discard else "saving episode"

    def shutdown(self):
        self._stop_thread.set()

    def status(self):
        with self.lock:
            return {
                "state": self.state, "recording": self.recording, "task": self.task, "dataset": self.args.dataset,
                "root": str(Path(self.args.root) / self.args.dataset),
                "episode_frames": self.ep_frames,
                "episode_seconds": round(time.time() - self.ep_started, 1) if self.recording else 0.0,
                "saved_episodes": self.saved_episodes, "target_episodes": self.args.target,
                "fps_target": self.args.fps, "fps_measured": round(self.measured_fps, 1),
                "arm_connected": self.arm.connected, "arm_error": self.arm.error, "arm_attempts": self.arm_attempts,
                "arm_rx_rate": round(self.arm.rx_rate), "ctrl_mode": self.ctrl_mode,
                "ctrl_mode_name": {0x00: "STANDBY", 0x01: "CAN", 0x02: "TEACHING"}.get(self.ctrl_mode),
                "cameras": [{"key": c.key, "name": c.name, "connected": c.connected and not c.stale,
                             "fps": round(c.fps(), 1), "error": c.error} for c in self.cams.values()],
                "cams_ok": all(c.connected and not c.stale and not c.degraded(self.args.min_cam_fps) for c in self.cams.values()),
                "cam_problem": self.cam_problem, "dataset_error": self.dataset_error, "message": self.last_msg,
                "joints": {k: round(v, 2) for k, v in self.joints.items()},
                "mock": bool(self.args.mock_arm or self.args.mock_camera),
            }

    def _arm_watch(self):
        while not self._stop_thread.is_set():
            self.arm.resync()
            busy = self.recording or self.state == "saving"
            if not self.arm.connected and not busy:
                was_up = self.arm.robot is not None
                self.arm_attempts += 1
                if self.arm.connect() and not was_up:
                    with self.lock:
                        self.last_msg = "arm came online"
            self._stop_thread.wait(3.0)

    def run(self):
        try:
            self._run()
        except Exception:
            traceback.print_exc()
            with self.lock:
                self.state = "error"
                self.last_msg = "recorder thread died — see console"

    def _write_trace(self, episode_index, rows):
        if not rows:
            return
        out = Path(self.args.root) / self.args.dataset / "traces" / f"episode_{episode_index:06d}.jsonl"
        try:
            out.parent.mkdir(parents=True, exist_ok=True)
            with open(out, "w", encoding="utf-8") as fh:
                for r in rows:
                    fh.write(json.dumps(r, separators=(",", ":")) + "\n")
        except Exception:
            traceback.print_exc()

    def _read_cameras(self):
        out = {}
        self.cam_problem = None
        for k, c in self.cams.items():
            if c.degraded(self.args.min_cam_fps):
                self.cam_problem = f"{k} camera collapsed to {c.fps():.1f} fps (minimum {self.args.min_cam_fps})"
                return {}
            rgb = c.read_latest()
            if rgb is None:
                self.cam_problem = f"{k} camera is not delivering frames"
                return {}
            out[k] = rgb
        return out

    def _run(self):
        import cv2
        self.arm.connect()
        threading.Thread(target=self._arm_watch, daemon=True).start()
        for c in self.cams.values():
            c.connect()
            c.start()
        try:
            self.dataset = self._open_dataset()
        except Exception as e:
            traceback.print_exc()
            self.dataset_error = f"{type(e).__name__}: {e}"

        with self.lock:
            self.state = "idle"

        period = 1.0 / self.args.fps
        prev = None
        gap = 0
        ep_trace = []
        next_tick = time.perf_counter()
        fps_n, fps_t0 = 0, time.time()
        mode_t = 0.0

        while not self._stop_thread.is_set():
            next_tick += period
            sleep = next_tick - time.perf_counter()
            if sleep > 0:
                time.sleep(sleep)
            else:
                next_tick = time.perf_counter()

            try:
                self.arm.poll()
                joints = self.arm.read()[0] if self.arm.connected else None
                if time.time() - mode_t > 1.0:
                    mode_t = time.time()
                    self.ctrl_mode = self.arm.read_ctrl_mode()
            except Exception as e:
                joints = None
                self.arm.error = f"{type(e).__name__}: {e}"

            frames = self._read_cameras()
            if joints is not None:
                with self.lock:
                    self.joints = joints
            for k, rgb in frames.items():
                ok, buf = cv2.imencode(".jpg", cv2.cvtColor(rgb, cv2.COLOR_RGB2BGR), [int(cv2.IMWRITE_JPEG_QUALITY), 70])
                if ok:
                    self.cams[k].jpeg = buf.tobytes()

            fps_n += 1
            if time.time() - fps_t0 >= 1.0:
                with self.lock:
                    self.measured_fps = fps_n / (time.time() - fps_t0)
                fps_n, fps_t0 = 0, time.time()

            if self._want_start.is_set():
                self._want_start.clear()
                if self.dataset is None:
                    msg = "no dataset — cannot record"
                elif joints is None:
                    msg = "arm not delivering frames — cannot record"
                elif not frames:
                    msg = f"{self.cam_problem or 'a camera is not delivering'} — cannot record"
                else:
                    msg = None
                if msg:
                    with self.lock:
                        self.last_msg = msg
                else:
                    prev, gap, ep_trace = None, 0, []
                    with self.lock:
                        self.recording = True
                        self.state = "recording"
                        self.ep_frames = 0
                        self.ep_started = time.time()
                        self.last_msg = "recording…"

            if self.recording:
                if joints is None or not frames:
                    gap += 1
                    if gap * period < self.args.max_gap_s:
                        continue
                    with self.lock:
                        self.recording = False
                        self.state = "idle"
                        self.last_msg = (f"{self.cam_problem or 'lost the arm or a camera'} for {gap * period:.1f}s — episode discarded")
                    try:
                        self.dataset.clear_episode_buffer()
                    except Exception:
                        pass
                    prev, gap = None, 0
                    continue
                gap = 0
                vec = np.array([joints[n] for n in JOINTS], dtype=np.float32)
                if prev is not None:
                    try:
                        frame = {"observation.state": prev[0], "action": vec, "task": self.task}
                        for k, img in prev[1].items():
                            frame[f"observation.images.{k}"] = img
                        self.dataset.add_frame(frame)
                        if prev[2] is not None:
                            row = dict(prev[2])
                            row["frame_index"] = len(ep_trace)
                            row["joints"] = [round(float(v), 3) for v in prev[0]]
                            ep_trace.append(row)
                        with self.lock:
                            self.ep_frames += 1
                    except Exception as e:
                        traceback.print_exc()
                        with self.lock:
                            self.recording = False
                            self.state = "idle"
                            self.last_msg = f"add_frame failed: {e}"
                        prev = None
                        continue
                prev = (vec, frames, self.arm.read_pose())

            if self._want_stop.is_set():
                self._want_stop.clear()
                discard = self._discard
                self._discard = False
                n = self.ep_frames
                with self.lock:
                    self.recording = False
                    self.state = "saving"
                prev = None
                try:
                    if discard or n < self.args.min_frames:
                        self.dataset.clear_episode_buffer()
                        msg = (f"discarded ({n} frames)" if discard else f"discarded — only {n} frames, need {self.args.min_frames}")
                    else:
                        t0 = time.time()
                        self.dataset.save_episode(parallel_encoding=False)
                        self.dataset.finalize()
                        self.dataset = self._open_dataset(quiet=True)
                        with self.lock:
                            self.saved_episodes = self.dataset.meta.total_episodes
                        self._write_trace(self.saved_episodes - 1, ep_trace)
                        msg = f"saved episode {self.saved_episodes} — {n} frames ({n / self.args.fps:.1f}s) in {time.time() - t0:.1f}s"
                except Exception as e:
                    traceback.print_exc()
                    msg = f"save failed: {e}"
                self.arm.resync()
                with self.lock:
                    self.ep_frames = 0
                    self.state = "idle"
                    self.last_msg = msg
                next_tick = time.perf_counter()

        if self.dataset is not None:
            try:
                if self.recording:
                    self.dataset.clear_episode_buffer()
                self.dataset.finalize()
            except Exception:
                traceback.print_exc()
        for c in self.cams.values():
            c.close()
        self.arm.close()


# =====================================================================================================================
# Policy — ACT inference. In-process, or (default) in a separate process so the CAN-parsing GIL doesn't slow it.
# =====================================================================================================================
class Policy:
    def __init__(self, path, threads):
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
        self.pre, self.post = make_pre_post_processors(self.model.config, pretrained_path=str(path), preprocessor_overrides=cpu, postprocessor_overrides=cpu)
        self.chunk_size = int(self.model.config.chunk_size)
        self.image_keys = [k for k in self.model.config.input_features if k.startswith("observation.images.")]
        self.last_ms = None

    def chunk(self, state, image_rgb):
        from lerobot.policies.utils import prepare_observation_for_inference
        obs = {"observation.state": np.array([state[j] for j in JOINTS], dtype=np.float32)}
        for k in self.image_keys:
            obs[k] = image_rgb
        t = time.perf_counter()
        with self.torch.inference_mode():
            batch = self.pre(prepare_observation_for_inference(obs, self.torch.device("cpu"), None, "piper_follower"))
            actions = self.model.predict_action_chunk(batch)
            out = np.stack([self.post(actions[:, i, :]).squeeze(0).cpu().numpy() for i in range(actions.shape[1])])
        self.last_ms = (time.perf_counter() - t) * 1000
        return out


def _policy_worker(conn, path, threads):
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
    def __init__(self, path, threads):
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

    def chunk(self, state, image_rgb):
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
            self.last_ms = (time.perf_counter() - t) * 1000
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
    def __init__(self, policy):
        self.policy = policy
        self.plan = {}
        self.history = deque(maxlen=3)
        self.ens_n = 1
        self.inflight = None
        self.ready = None
        self.last_obs_step = None
        self.lock = threading.Lock()
        self.latencies = deque(maxlen=20)
        self.starved = 0
        self.error = None

    def request(self, step, state, image):
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

    def integrate(self, k, crossfade):
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

    def take(self, k, ensemble=1):
        a = self.plan.pop(k, None)
        for old in [x for x in self.plan if x < k]:
            self.plan.pop(old)
        if a is None:
            self.starved += 1
            return None
        self.ens_n = 1
        if ensemble > 1:
            vals = [c[k - s] for s, c in list(self.history)[-ensemble:] if 0 <= k - s < len(c)]
            if len(vals) > 1:
                m = np.mean(vals, axis=0)
                m[6] = a[6]
                self.ens_n = len(vals)
                return m
        return a

    def remaining(self, k):
        return sum(1 for x in self.plan if x >= k)


def read_contract(posttrain_dir):
    try:
        return json.loads((Path(posttrain_dir) / "CONTRACT.json").read_text(encoding="utf-8"))
    except Exception:
        return None


class RolloutRecorder:
    def __init__(self, posttrain_dir, run_id, meta, fps):
        self.dir = Path(posttrain_dir) / "rollouts" / run_id
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

    def add(self, row, image_rgb):
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

    def close(self, summary):
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

    def finalize(self, outcome, case, note):
        if (self.dir / "READY").exists():
            return False
        self.meta.update(outcome=outcome, case=case, note=note, labelled=datetime.now().isoformat(timespec="seconds"))
        self._write_meta()
        (self.dir / "READY").write_text(outcome, encoding="utf-8")
        return True


# =====================================================================================================================
# Controller — policy execution, sharing the same Arm + wrist camera as the Recorder.
# =====================================================================================================================
class Controller:
    FPS = 15
    PREVIEW_GOOD, PREVIEW_WEAK = 20.0, 8.0

    def __init__(self, args, arm, cam):
        self.args = args
        self.arm = arm
        self.cam = cam
        self.params = dict(DEFAULT_PARAMS)
        self.state = "disconnected"
        self.message = "Connect to the arm to begin."
        self.policy = None
        self.planner = None
        self.lock = threading.RLock()
        self.last_heartbeat = now()
        self.joints, self.arm_age, self.target = None, None, None
        self.run = None
        self.runs_dir = (Path(os.environ.get("TEMP", HERE)) / "piper_posttrain_mock" / "runs"
                         if (args.mock_arm or args.mock_camera) else SHARED / "deploy" / "runs")
        self.results_path = self.runs_dir / "results.csv"
        self.posttrain_dir = Path(args.posttrain_dir)
        self.rollout = None
        self.info = self._dataset_info()
        self.envelope = self.info["envelope"]
        self.start_pose = self.info["start_pose"]
        self.stop_flag = threading.Event()
        self.correct_flag = threading.Event()
        self.preview_result = None
        self.shutdown_fn = None
        threading.Thread(target=self._monitor, daemon=True, name="monitor").start()

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

    def _clamp(self, target, current, last=None):
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

    def _gripper(self, plan_g, meas_g, g):
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
        cmd = plan_g if plan_g >= 95.0 else max(0.0, plan_g - sq)
        g["last"] = cmd
        return cmd, "follow"

    def _set(self, state, message):
        self.state, self.message = state, message

    def snapshot(self):
        cam_fps = self.cam.fps() if self.cam else 0.0
        _, cam_age = self.cam.latest() if self.cam else (None, None)
        run = self.run or {}
        pl = self.planner
        lat = [x * 1000 for x in pl.latencies] if pl else []
        mode = self.arm.ctrl_mode_raw() if self.arm else None
        age = self.arm_age
        live = age is not None and not math.isinf(age)
        return {
            "state": self.state, "message": self.message, "mock": {"arm": self.args.mock_arm, "camera": self.args.mock_camera},
            "params": self.params, "limits": {k: [v[0], v[1]] for k, v in LIMITS.items()},
            "arm": {"connected": self.arm is not None and self.arm.connected, "enabled": bool(self.arm and self.arm.enabled),
                    "ctrl_mode": mode, "teaching": mode == TEACHING,
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

    def connect(self):
        with self.lock:
            if self.state in ("running", "moving"):
                return
            self.message = "Connecting…"
            if self.arm.robot is None and not self.arm.mock:
                self.arm.connect()
            if self.cam is not None and self.cam._thread is None:
                self.cam.connect()
                self.cam.start()
            _, age = self.arm.read()
            if age < 0.3 or self.arm.mock:
                self._set("connected", "Connected (motors not enabled). Load a policy.")
            else:
                self._set("connected", "CAN open but NO live feedback from the arm — it is in STANDBY or unpowered. "
                                       "Press the drag-teach button so its light is ON (the portal exits drag-teach itself).")

    def load(self, name):
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
            for _ in range(3):
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
            self.arm.enable(self.params["gripper_effort"])
            self._set("enabled", "Motors enabled and holding. Move to the start pose.")

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
            steps = 60
            for i in range(1, steps + 1):
                if self.stop_flag.is_set():
                    break
                w = 0.5 - 0.5 * math.cos(math.pi * i / steps)
                tgt = {j: cur[j] + (pose[j] - cur[j]) * w for j in JOINTS}
                self.arm.send(tgt, 20, self.params["gripper_effort"], self.params["gripper_open_raw"])
                self.target = tgt
                time.sleep(1 / 15)
            time.sleep(1.0)
            self._set("enabled", "At the start pose. Place the block, then Run.")
        except Exception as e:
            self._set("stopped", f"Start-pose move failed: {e}")
        finally:
            self.stop_flag.clear()

    def preview(self):
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
            mode = self.arm.ctrl_mode_raw()
            if not self.args.mock_arm and mode != 0x01:
                raise RuntimeError("the arm is in drag-teach mode: press Enable" if mode == TEACHING else
                                   f"the arm is not under CAN control (ctrl_mode {mode}): press Enable")
            ckpt = self.policy.path.name
            rid = datetime.now().strftime("%Y%m%d-%H%M%S") + "_" + ckpt
            rollout = None
            if self.params["record_rollouts"]:
                mock = self.args.mock_arm or self.args.mock_camera
                pt_dir = Path(os.environ.get("TEMP", HERE)) / "piper_posttrain_mock" if mock else self.posttrain_dir
                contract = read_contract(self.posttrain_dir)
                if contract is None or int(contract.get("contract_version", -1)) != CONTRACT_VERSION:
                    raise RuntimeError(f"posttrain/CONTRACT.json version mismatch. Set record_rollouts 0 or update the portal.")
                rollout = RolloutRecorder(pt_dir, rid, {"source": "dell_piper_act_portal", "mock": mock, "checkpoint": ckpt,
                                                        "params": dict(self.params), "task": contract.get("task"),
                                                        "joint_names": JOINTS, "camera": self.args.camera_name}, self.FPS)
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
        with self.lock:
            if self.state != "running":
                raise RuntimeError("not running")
            self.correct_flag.set()
            self.message = "Correction requested — finish the task by hand, then Stop (Space) and mark Success/Fail."

    def mark(self, outcome, case, note):
        r = self.run
        if not r:
            raise RuntimeError("no run to mark")
        self.results_path.parent.mkdir(parents=True, exist_ok=True)
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

    def set_params(self, upd):
        with self.lock:
            for k, v in upd.items():
                if k not in LIMITS:
                    continue
                lo, hi, typ = LIMITS[k]
                self.params[k] = typ(min(hi, max(lo, float(v))))
            if self.policy:
                self.params["replan_every"] = min(self.params["replan_every"], self.policy.chunk_size)

    def results(self):
        if not self.results_path.exists():
            return []
        with open(self.results_path, newline="", encoding="utf-8") as f:
            return list(csv.DictReader(f))[-50:]

    def _current_policy(self):
        try:
            return json.loads((Path(self.args.policies_dir) / "CURRENT.json").read_text(encoding="utf-8")).get("name")
        except Exception:
            return None

    def _need(self, connected=False, enabled=False, policy=False):
        if connected and self.arm is None:
            raise RuntimeError("connect first")
        if enabled and not self.arm.enabled:
            raise RuntimeError("enable the motors first")
        if policy and self.policy is None:
            raise RuntimeError("load a policy first")

    def _monitor(self):
        while True:
            try:
                arm = self.arm
                if arm is not None and self.state != "running":
                    self.joints, self.arm_age = arm.read()
                    if (not self.args.mock_arm and arm.enabled and self.state in ("enabled", "stopped", "estopped")
                            and arm.ctrl_mode() == TEACHING):
                        arm.enabled = False
                        self._set("connected", "Arm is in drag-teach (motors free). Pose it, then press Enable to hold that pose.")
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
        grip = {}
        segment = "policy"
        rec = self.rollout
        stale_since = None
        cam_stale_since = None
        reason = None
        try:
            state, _ = self.arm.read()
            img, _ = self.cam.latest()
            pl.request(0, state, img)
            while pl.ready is None and not self.stop_flag.is_set():
                if pl.error:
                    raise RuntimeError(pl.error)
                time.sleep(0.005)
            t_start = now()
            next_t = t_start
            while not self.stop_flag.is_set():
                tick = now()
                state, arm_age = self.arm.read()
                img, cam_age = self.cam.latest()
                self.joints, self.arm_age = state, arm_age
                if pl.error:
                    reason = pl.error; break
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
                r["segment"] = segment
                if tick - self.last_heartbeat > p["heartbeat_s"]:
                    reason = "portal page stopped responding (heartbeat)"; break
                if tick - t_start > p["max_duration_s"]:
                    reason = f"reached max duration {p['max_duration_s']} s"; break
                fault = self.arm.fault()
                if fault:
                    reason = fault; break
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

                raw, sent, gmode = None, None, "human"
                if segment == "policy":
                    if hold_this_tick:
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
                            tgt = dict(last_sent or state)
                            gmode = "starved"
                        else:
                            tgt = self._clamp({j: float(raw[i]) for i, j in enumerate(JOINTS)}, state, last_sent)
                            tgt["gripper"], gmode = self._gripper(tgt["gripper"], state["gripper"], grip)
                        sent = tgt
                        self.arm.send(sent, p["speed_pct"], p["gripper_effort"], p["gripper_open_raw"])
                        last_sent = sent
                        self.target = sent
                gf = self.arm.grip_feedback()
                st = self.arm.status_raw()
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
                    self.arm.enabled = False
                elif hold_age < 0.5:
                    if hold["gripper"] < 95.0:
                        hold["gripper"] = max(0.0, hold["gripper"] - p["gripper_squeeze"])
                    self.arm.send(hold, 20, p["gripper_effort"], p["gripper_open_raw"])
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
# Pages — the two tested templates are reused verbatim (recorder endpoints renamed to /api/record/*).
# =====================================================================================================================
sys.path.insert(0, str(SHARED / "deploy"))
import piper_record_portal as _rec
import piper_policy_portal as _pol

RECORD_PAGE = (_rec.PAGE
               .replace("/api/status", "/api/record/status")
               .replace("/api/start", "/api/record/start")
               .replace("/api/stop", "/api/record/stop")
               .replace("<h1>PiPER drag-teach recorder</h1>",
                        "<h1>PiPER drag-teach recorder</h1><div class=\"sub\" style=\"margin-top:4px\">"
                        "<a href=\"/\">‹ dashboard</a> · <a href=\"/policy\">policy execution →</a></div>"))
POLICY_PAGE = _pol.PAGE.replace(
    "<h1>Run a trained policy</h1>",
    "<h1>Run a trained policy</h1><p class=\"sub\" style=\"margin-top:4px\">"
    "<a href=\"/\" style=\"color:#2a5caa\">‹ dashboard</a> · <a href=\"/record\" style=\"color:#2a5caa\">data collection →</a></p>")

DASHBOARD = r"""<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>PiPER ACT</title>
<style>:root{--bg:#0f1115;--card:#171a21;--line:#262b36;--fg:#e7eaf0;--dim:#9aa3b2;--ok:#3fb950;--bad:#f85149;--warn:#d29922;--acc:#58a6ff}
*{box-sizing:border-box}body{margin:0;background:var(--bg);color:var(--fg);font:14px/1.5 system-ui,Segoe UI,sans-serif}
.wrap{max-width:1080px;margin:0 auto;padding:28px 16px}
h1{margin:0;font-size:22px}.sub{color:var(--dim);font-size:12px;margin:2px 0 20px}
.grid{display:grid;grid-template-columns:repeat(3,1fr);gap:14px}@media(max-width:860px){.grid{grid-template-columns:1fr}}
.card{background:var(--card);border:1px solid var(--line);border-radius:10px;padding:16px}
.card h2{font-size:13px;text-transform:uppercase;letter-spacing:.06em;color:var(--dim);margin:0 0 10px}
.big{font-size:26px;font-variant-numeric:tabular-nums;font-weight:600}
.row{display:flex;justify-content:space-between;padding:4px 0;font-size:13px}.row span:first-child{color:var(--dim)}
a.btn{display:block;background:var(--acc);color:#fff;text-align:center;text-decoration:none;padding:14px;border-radius:8px;font-weight:600;margin-top:8px}
a.btn.ghost{background:transparent;border:1px solid var(--line);color:var(--fg)}
.dot{display:inline-block;width:8px;height:8px;border-radius:50%;margin-right:6px}
.ok{color:var(--ok)}.bad{color:var(--bad)}.warn{color:var(--warn)}
table{width:100%;border-collapse:collapse;font-size:12px;margin-top:8px}td{padding:5px;border-bottom:1px solid var(--line)}
</style></head><body><div class="wrap">
<h1>PiPER ACT</h1><div class="sub">unified portal — dashboard</div>
<div class="grid">
 <div class="card"><h2>Arm</h2><div id="arm"></div><div class="row"><span>Mode</span><b id="mode">—</b></div></div>
 <div class="card"><h2>Policy</h2><div class="row"><span>Current</span><b id="cur">—</b></div><div class="row"><span>Loaded</span><b id="loaded">—</b></div><a class="btn" href="/policy">Policy execution →</a></div>
 <div class="card"><h2>Dataset</h2><div class="row"><span>Episodes</span><b id="eps">—</b></div><div class="row"><span>Recorder</span><b id="recstate">—</b></div><a class="btn ghost" href="/record">Data collection →</a></div>
</div>
<div class="card" style="margin-top:14px"><h2>Recent results</h2><table id="results"><tbody><tr><td>—</td></tr></tbody></table></div>
</div>
<script>
const $=i=>document.getElementById(i), dot=o=>`<span class="dot" style="background:${o?'var(--ok)':'var(--bad)'}"></span>`;
async function poll(){
 try{
  const [st,rec,res]=await Promise.all([fetch('/api/state').then(r=>r.json()),fetch('/api/record/status').then(r=>r.json()),fetch('/api/results').then(r=>r.json())]);
  const a=st.arm,c=st.camera;
  $('arm').innerHTML=dot(a.connected)+(a.connected?`live · feedback ${a.feedback_age_ms??'—'} ms`:'no frames');
  $('mode').textContent=a.ctrl_mode==null?'—':({0:'STANDBY (silent)',1:'CAN control',2:'DRAG-TEACH'}[a.ctrl_mode]||('0x'+a.ctrl_mode.toString(16)));
  $('cur').textContent=st.current_policy||'—'; $('loaded').textContent=st.policy.name||'—';
  $('eps').textContent=`${rec.saved_episodes} / ${rec.target_episodes}`; $('recstate').textContent=rec.state;
  $('results').innerHTML=res.slice().reverse().slice(0,12).map(x=>`<tr><td>${x.checkpoint}</td><td>${x.case}</td><td class="${x.outcome==='success'?'ok':'bad'}">${x.outcome}</td><td>${x.duration_s}s</td><td>${x.note||''}</td></tr>`).join('')||'<tr><td>—</td></tr>';
 }catch(e){$('arm').textContent='portal unreachable'}
}
poll();setInterval(poll,1000);
</script></body></html>"""


# =====================================================================================================================
# Web — one handler dispatching recorder + policy routes.
# =====================================================================================================================
def make_handler(rec, ctl, on_shutdown):
    class H(BaseHTTPRequestHandler):
        protocol_version = "HTTP/1.1"

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

        def _html(self, body):
            b = body.encode()
            self.send_response(200)
            self.send_header("Content-Type", "text/html; charset=utf-8")
            self.send_header("Content-Length", str(len(b)))
            self.send_header("Cache-Control", "no-store")
            self.end_headers()
            self.wfile.write(b)

        def do_GET(self):
            p = self.path.split("?")[0]
            if p in ("/", "/dashboard"):
                return self._html(DASHBOARD)
            if p == "/record":
                return self._html(RECORD_PAGE)
            if p == "/policy":
                return self._html(POLICY_PAGE)
            if p == "/api/record/status":
                return self._json(rec.status())
            if p == "/api/state":
                return self._json(ctl.snapshot())
            if p == "/api/results":
                return self._json(ctl.results())
            if p == "/frame.jpg":
                return self._frame()
            if p.startswith("/preview/") and p.endswith(".mjpg"):
                key = p[len("/preview/"):-len(".mjpg")]
                if key in rec.cams:
                    return self._mjpeg(rec.cams[key])
            return self._json({"error": "not found"}, 404)

        def do_POST(self):
            n = int(self.headers.get("Content-Length") or 0)
            try:
                data = json.loads(self.rfile.read(n) or b"{}")
            except Exception:
                data = {}
            p = self.path.split("?")[0]
            if p == "/api/shutdown":
                self._json({"ok": True, "message": "shutting down"})
                on_shutdown()
                return
            if p == "/api/record/start":
                ok, msg = rec.start_episode(data.get("task"))
                return self._json({"ok": ok, "message": msg})
            if p == "/api/record/stop":
                ok, msg = rec.stop_episode(bool(data.get("discard")))
                return self._json({"ok": ok, "message": msg})
            route = p.removeprefix("/api/")
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
                else:
                    return self._json({"ok": False, "error": "unknown route"}, 404)
                self._json({"ok": True})
            except Exception as e:
                ctl.message = str(e)
                self._json({"ok": False, "error": str(e)}, 400)

        def _frame(self):
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

        def _mjpeg(self, cam):
            self.send_response(200)
            self.send_header("Content-Type", "multipart/x-mixed-replace; boundary=frame")
            self.send_header("Cache-Control", "no-store")
            self.end_headers()
            try:
                while True:
                    jpg = cam.jpeg
                    if jpg:
                        self.wfile.write(b"--frame\r\nContent-Type: image/jpeg\r\nContent-Length: "
                                         + str(len(jpg)).encode() + b"\r\n\r\n" + jpg + b"\r\n")
                    time.sleep(1.0 / 15)
            except (BrokenPipeError, ConnectionResetError, OSError):
                pass

        @staticmethod
        def _safe(fn, *a):
            try:
                fn(*a)
            except Exception as e:
                ctl.message = f"{fn.__name__} failed: {e}"
                traceback.print_exc()

    return H


def parse_camera(spec):
    if "=" not in spec:
        raise argparse.ArgumentTypeError(f"--camera needs key=name (got {spec!r})")
    key, name = spec.split("=", 1)
    key, name = key.strip(), name.strip()
    if not key.isidentifier():
        raise argparse.ArgumentTypeError(f"camera key {key!r} must be a plain identifier")
    return key, name


def main():
    p = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    # shared
    p.add_argument("--host", default="127.0.0.1")
    p.add_argument("--port", dest="http_port", type=int, default=8790)
    p.add_argument("--can-port", dest="port", default="candle")
    p.add_argument("--mock-arm", action="store_true")
    p.add_argument("--mock-camera", action="store_true")
    # recorder
    p.add_argument("--dataset", default="piper_pick_place")
    p.add_argument("--task", default="pick up the white block and place it on the red area")
    p.add_argument("--root", default=str(DEFAULT_ROOT))
    p.add_argument("--fps", type=int, default=15)
    p.add_argument("--target", type=int, default=50)
    p.add_argument("--min-cam-fps", type=float, default=5.0)
    p.add_argument("--max-gap-s", type=float, default=1.5)
    p.add_argument("--min-frames", type=int, default=30)
    p.add_argument("--camera", dest="cameras", action="append", type=parse_camera, metavar="KEY=NAME")
    p.add_argument("--width", type=int, default=640)
    p.add_argument("--height", type=int, default=480)
    p.add_argument("--vcodec", default="h264", choices=["h264", "hevc", "libsvtav1"])
    # policy
    p.add_argument("--policies-dir", default=str(SHARED / "policies"))
    p.add_argument("--dataset-root", default=str(SHARED / "datasets" / "piper_pick_place"))
    p.add_argument("--shim-dir", default=os.environ.get("PIPER_SHIM_DIR", r"D:\Piper-CAN-Teleop\scripts"))
    p.add_argument("--camera-name", default="Dabai DC1")
    p.add_argument("--camera-index", type=int, default=None)
    p.add_argument("--threads", type=int, default=max(1, (os.cpu_count() or 2) // 2))
    p.add_argument("--inprocess", action="store_true")
    p.add_argument("--posttrain-dir", default=str(SHARED / "posttrain"))
    args = p.parse_args()
    if not args.cameras:
        args.cameras = [("wrist", "Dabai DC1")]
    if args.mock_arm or args.mock_camera:
        # never let a mock run write into the real Syncthing dataset
        args.root = os.path.join(os.environ.get("TEMP", str(HERE)), "piper_act_mock_datasets")

    # one arm + one camera set shared by both engines
    arm = Arm(args.port, mock=args.mock_arm)
    cams = {k: Cam(k, n, args.width, args.height, mock=args.mock_camera) for k, n in args.cameras}
    wrist = cams.get("wrist") or next(iter(cams.values()))

    rec = Recorder(args, arm, cams)
    rec.start()
    for _ in range(300):
        if rec.status()["state"] != "starting":
            break
        time.sleep(0.1)

    ctl = Controller(args, arm, wrist)

    srv = ThreadingHTTPServer((args.host, args.http_port), make_handler(rec, ctl, lambda: _bye()))
    srv.daemon_threads = True

    def _bye(*_):
        threading.Thread(target=srv.shutdown, daemon=True).start()

    for signame in ("SIGTERM", "SIGINT", "SIGBREAK"):
        sig = getattr(signal, signame, None)
        if sig is not None:
            try:
                signal.signal(sig, _bye)
            except (ValueError, OSError):
                pass

    s = rec.status()
    print(f"\nPiPER ACT on http://{args.host}:{args.http_port}")
    print(f"  /        dashboard\n  /record  data collection ({args.dataset})\n  /policy  policy execution")
    print(f"  arm      : {'live' if s['arm_connected'] else 'NO FRAMES — ' + str(s['arm_error'])}")
    if s["ctrl_mode_name"]:
        print(f"  ctrl     : {s['ctrl_mode_name']}")
    for c in s["cameras"]:
        print(f"  camera   : {c['key']:<8} {c['name']:<20} {'ok' if c['connected'] else 'OFFLINE — ' + str(c['error'])}")
    if s["dataset_error"]:
        print(f"  DATASET  : {s['dataset_error']}")
    print(f"  policies : {len(ctl.list_checkpoints())} checkpoints, current = {ctl._current_policy() or '—'}")

    try:
        srv.serve_forever()
    except KeyboardInterrupt:
        pass
    finally:
        print("\nshutting down — releasing adapter, finalizing dataset…")
        rec.shutdown()
        rec.join(timeout=30)
        arm.close()
        print("done.")


if __name__ == "__main__":
    main()
