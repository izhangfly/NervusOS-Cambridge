#!/usr/bin/env python3
"""
Click-to-record portal for kinesthetic (drag-teach) demos on a single PiPER.

Open http://127.0.0.1:8790, press Start, drag the arm through the task by hand,
press Stop. Every camera records in lockstep and each episode lands straight in
the Syncthing dataset folder, so the Mac sees it without a copy step.

WHY THIS EXISTS
    `lerobot.record` drives a leader-follower pair: a human moves a *leader* arm
    and the follower mirrors it, which is where the `action` stream comes from.
    With one arm there is no leader, so this records the arm's own joint
    feedback while you move it by hand, and derives the action stream from it.

ACTION
    A LeRobot frame needs both `observation.state` (where the arm is) and
    `action` (where it was told to go). Drag-teach has no command stream, so the
    action for frame t is the state at t+1 -- "the pose to move to next", which
    is exactly what the policy must output at deploy time, where the action goes
    to `send_action` -> `JointCtrl`. The loop therefore lags one tick behind and
    the final frame of each episode is dropped (it has no successor).

CAMERAS
    Any number, each named. The name becomes the dataset key, so the wrist and
    third-person views land as observation.images.wrist / .scene and the Mac
    trains on both with no config change. Devices are matched BY NAME, never by
    index: the laptop's built-in webcam enumerates between the two USB cameras
    here, and indices shift whenever anything is replugged.

SAFETY
    Torque is never enabled and `parking()` is never called, so the arm is not
    commanded to move at any point. This process only ever reads.

    python piper_record_portal.py --dataset piper_push_block \
        --camera wrist="Dabai DC1" --camera scene="Logi C270" \
        --task "push the red block to the marked square"
"""

import argparse
import json
import shutil
import signal
import sys
import threading
import time
import traceback
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

import numpy as np

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE))
sys.path.insert(0, str(HERE.parent / "vla"))

DEFAULT_ROOT = Path(r"C:\Users\zhy10\Shared\Piper Arm\datasets")
JOINTS = ["joint1", "joint2", "joint3", "joint4", "joint5", "joint6", "gripper"]


# --------------------------------------------------------------------------
# hardware
# --------------------------------------------------------------------------

class Arm:
    """Live joint feedback, read-only. Never enables torque, never moves.

    LIVENESS, and why the obvious check is wrong: `bus.get_action()` returns the
    SDK's cached joint state and raises nothing when the arm has never spoken --
    it just hands back zeros, which normalize to a plausible-looking pose
    (joint2=-100, joint3=+100 for this calibration). So a successful read proves
    only that the object exists. The honest signal is the shim's rx_count: real
    CAN frames arriving. `connected` therefore means "frames are flowing".

    Note the arm only streams feedback in an active ctrl_mode. Sitting in
    STANDBY (0x00) it answers queries but does not broadcast, which looks
    exactly like a dead cable from up here -- see the ctrl_mode readout.
    """

    RX_TIMEOUT = 2.0        # no frames for this long -> not live

    def __init__(self, port="candle", mock=False):
        self.mock = mock
        self.port = port
        self.robot = None
        self.error = None
        self.rx_rate = 0.0
        self.ctrl_mode = None
        self._t0 = time.time()
        self._last_rx = 0
        self._last_rx_t = 0.0
        self._last_seen = 0.0
        # Held only by connect(), never by readers, so a slow reconnect can
        # never starve the tick loop. Guarantees one bus on the adapter at a
        # time: two GsUsbRawBus objects on one candleLight fight exactly the
        # way two processes do, and neither then receives anything.
        self._connecting = threading.Lock()
        self._fk = None             # our own FK solver, called once per frame

    @staticmethod
    def _rx_count(robot):
        try:
            can = robot.bus.piper._C_PiperInterface_V2__arm_can
            return getattr(can.bus, "rx_count", None)
        except Exception:
            return None

    def read_ctrl_mode(self):
        """0x00 STANDBY / 0x01 CAN / 0x02 TEACHING, or None."""
        robot = self.robot
        if self.mock or robot is None:
            return None
        try:
            s = robot.bus.piper.GetArmStatus().arm_status
            return getattr(s.ctrl_mode, "value", None)
        except Exception:
            return None

    def connect(self):
        """Build a connection off to the side, then publish it atomically.

        Connecting takes seconds (the shim re-opens a stalled endpoint up to
        four times). Doing that behind a lock the tick loop also needs starves
        the loop: reads return nothing, liveness goes stale, `connected` flips
        false, and the watchdog reconnects again -- a stall that feeds itself
        and silently refuses to start episodes. So nothing here is locked; the
        finished robot is assigned to self.robot in one atomic store, and
        readers take a single reference before using it.
        """
        if self.mock:
            self._last_seen = time.time()
            return True
        if not self._connecting.acquire(blocking=False):
            return False                    # an attempt is already in flight
        robot = None
        try:
            # Retire the previous connection first. Leaving it open means two
            # buses on one adapter, which silently starves both.
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
            # bus.connect() only opens the CAN port. Deliberately NOT
            # robot.connect(), which enables torque and parks the arm.
            robot.bus.connect()
            # Deliberately NOT EnableFkCal(). That makes the SDK solve forward
            # kinematics twice inside ParseCANFrame -- for every one of ~2300
            # CAN frames a second, in pure Python, holding the GIL. It starved
            # the camera thread until the wrist camera collapsed from 12.5 fps
            # to 1 fps. read_pose() computes FK itself, once per recorded frame.

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
                    self.robot = robot          # publish only once it works
                    return True

            try:
                s = robot.bus.piper.GetArmStatus().arm_status
                mode = getattr(s.ctrl_mode, "value", None)
            except Exception:
                mode = None
            hint = ("the arm is in STANDBY (0x00) — it answers queries but does "
                    "not stream. Press the drag-teach button to put it in "
                    "TEACHING mode." if mode == 0x00 else
                    "check 24V power, the base LED, and the CAN cable.")
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
        now, n = time.time(), self._rx_count(robot)
        if n is None:
            return
        if n < self._last_rx:
            # The shim built a fresh bus, so its counter restarted. Without this
            # the delta goes negative, liveness looks dead, the watchdog
            # reconnects, and that makes another fresh counter -- a loop that
            # aborts every take. A reset means frames ARE flowing, so re-base
            # and treat the arm as alive.
            self._last_rx, self._last_rx_t = n, now
            self._last_seen = now
            return
        dt = now - self._last_rx_t
        if dt >= 0.5:
            self.rx_rate = (n - self._last_rx) / dt
            self._last_rx, self._last_rx_t = n, now
            if self.rx_rate > 0:
                self._last_seen = now

    def resync(self):
        """Re-base liveness after the tick loop was blocked for a while.

        Video encoding runs in the recorder thread and takes many seconds, so
        poll() cannot run and _last_seen goes stale -- the arm then looks dead
        purely because we were busy, and the next episode is refused. The shim's
        I/O thread kept receiving throughout, so a grown rx_count proves the arm
        is fine.
        """
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
        if self.mock:
            t = time.time() - self._t0
            return {n: 40.0 * np.sin(t * 0.7 + i) for i, n in enumerate(JOINTS)}
        robot = self.robot
        if robot is None:
            return None
        return robot.bus.get_action()

    def read_pose(self):
        """Cartesian tool pose + per-link FK + raw gripper, or None.

        Recorded to a sidecar file, never into the LeRobot features: ACT is
        trained from joints and images, and an unexpected feature key would
        change what the policy sees. Kept alongside so the tool path can be
        drawn in 3D later. Units: mm and degrees.
        """
        if self.mock:
            t = time.time() - self._t0
            return {"x": 300 + 50 * np.sin(t), "y": 20 * np.cos(t), "z": 200.0,
                    "rx": 0.0, "ry": 85.0, "rz": 0.0,
                    "gripper_raw": 0, "gripper_effort": 0, "fk": []}
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
                    # 0x01 matches the dh_is_offset the plugin's SDK instance uses.
                    self._fk = C_PiperForwardKinematics(0x01)
                j = piper.GetArmJointMsgs().joint_state
                # Raw joint units are 0.001 deg; CalFK wants radians.
                k = 1000.0 * self._fk.RADIAN
                rad = [j.joint_1 / k, j.joint_2 / k, j.joint_3 / k,
                       j.joint_4 / k, j.joint_5 / k, j.joint_6 / k]
                fk = [[round(float(v), 3) for v in link]
                      for link in self._fk.CalFK(rad)]
            except Exception:
                fk = []
            return {
                "x": e.X_axis / 1000.0, "y": e.Y_axis / 1000.0, "z": e.Z_axis / 1000.0,
                "rx": e.RX_axis / 1000.0, "ry": e.RY_axis / 1000.0,
                "rz": e.RZ_axis / 1000.0,
                "gripper_raw": int(g.grippers_angle),
                "gripper_effort": int(g.grippers_effort),
                "fk": fk,
            }
        except Exception:
            return None

    @property
    def connected(self):
        if self.mock:
            return True
        robot = self.robot
        return (robot is not None
                and robot.bus.is_connected
                and time.time() - self._last_seen <= self.RX_TIMEOUT)

    def close(self):
        if self.robot is not None:
            try:
                # disable_torque=False skips parking(), which would MOVE the arm.
                self.robot.bus.disconnect(disable_torque=False)
            except Exception:
                pass
            self.robot = None


class Cam:
    """One RGB stream, opened by device NAME (never by index).

    Each camera grabs in its own thread and publishes only its latest frame.
    Reading cameras inline instead would pin the whole recording loop to the
    slowest device -- a 3 fps webcam dragged a 15 fps loop down to 1 fps and
    produced 5-frame episodes. Decoupled, the loop always ticks at its target
    rate and simply reuses a slow camera's most recent frame; `stale` says when
    that frame is too old to trust.
    """

    STALE_AFTER = 1.0       # seconds; older than this counts as not delivering

    def __init__(self, key, name, width=640, height=480, mock=False):
        self.key, self.name = key, name
        self.width, self.height, self.mock = width, height, mock
        self.cap = None
        self.error = None
        self.jpeg = None
        self.latest = None          # (rgb, timestamp)
        self.fps = 0.0
        self._started = None
        self._t0 = time.time()
        self._stop = threading.Event()
        self._thread = None

    def start(self):
        if self._thread is None:
            self._started = time.time()
            self._thread = threading.Thread(target=self._loop, name=f"cam-{self.key}",
                                            daemon=True)
            self._thread.start()

    def degraded(self, min_fps):
        """True once the measured rate has collapsed below min_fps.

        Staleness alone cannot catch this: at 1 fps a new frame still lands
        just inside STALE_AFTER, so the camera looked "ok" while the recorder
        would have filled 14 of every 15 dataset frames with repeats. The
        first seconds are ignored while the rate is still being measured.
        """
        if self.mock or self._started is None or time.time() - self._started < 3.0:
            return False
        return self.fps < min_fps

    def _loop(self):
        n, t0 = 0, time.time()
        while not self._stop.is_set():
            if not self.connected:
                time.sleep(0.2)
                continue
            try:
                rgb = self._grab()
                self.latest = (rgb, time.time())
                n += 1
            except Exception as e:
                self.error = f"{type(e).__name__}: {e}"
                time.sleep(0.1)
            if time.time() - t0 >= 1.0:
                self.fps = n / (time.time() - t0)
                n, t0 = 0, time.time()

    def read_latest(self):
        """Most recent frame, or None if there is none or it has gone stale."""
        got = self.latest
        if got is None:
            return None
        rgb, ts = got
        return None if time.time() - ts > self.STALE_AFTER else rgb

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
        """HxWx3 uint8 RGB. LeRobot stores RGB; cv2 hands back BGR."""
        if self.mock:
            t = time.time() - self._t0
            img = np.zeros((self.height, self.width, 3), np.uint8)
            x = int((np.sin(t + hash(self.key) % 7) * 0.5 + 0.5) * (self.width - 60))
            y = int((np.cos(t * 0.7) * 0.5 + 0.5) * (self.height - 60))
            img[y:y + 60, x:x + 60] = (220, 60, 60)
            return img
        import cv2
        self.cap.grab()      # drop the buffered frame; stale images look like bad policy
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


# --------------------------------------------------------------------------
# recorder
# --------------------------------------------------------------------------

class Recorder(threading.Thread):
    """Owns the arm and cameras. Ticks at fps; records between start/stop."""

    def __init__(self, args):
        super().__init__(daemon=True)
        self.args = args
        self.arm = Arm(args.port, mock=args.mock_arm)
        self.cams = {k: Cam(k, n, args.width, args.height, mock=args.mock_camera)
                     for k, n in args.cameras}

        self.lock = threading.Lock()
        self.dataset = None
        self.dataset_error = None

        self.recording = False
        self.state = "starting"      # starting|idle|recording|saving|error
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

    # ---- dataset -------------------------------------------------------

    def _features(self):
        f = {
            "observation.state": {
                "dtype": "float32", "shape": (len(JOINTS),), "names": list(JOINTS),
            },
            "action": {
                "dtype": "float32", "shape": (len(JOINTS),), "names": list(JOINTS),
            },
        }
        for key in self.cams:
            f[f"observation.images.{key}"] = {
                "dtype": "video",
                "shape": (self.args.height, self.args.width, 3),
                "names": ["height", "width", "channels"],
            }
        return f

    def _open_dataset(self, quiet=False):
        from lerobot.datasets.lerobot_dataset import LeRobotDataset

        root = Path(self.args.root) / self.args.dataset
        repo_id = f"local/{self.args.dataset}"

        # A run that died before saving its first episode leaves info.json with
        # no meta/episodes/ behind. Resuming that makes LeRobot fall through to
        # the Hub and die on a 401, so treat a stub as "not a dataset" -- but
        # only scrap it once we are sure it holds no recorded frames.
        ep_dir = root / "meta" / "episodes"
        exists = ep_dir.exists() and any(ep_dir.rglob("*.parquet"))
        if not exists and (root / "meta" / "info.json").exists():
            payload = (any((root / "data").rglob("*.parquet"))
                       or any((root / "videos").rglob("*.mp4")))
            if payload:
                raise RuntimeError(
                    f"{root} holds recorded data but its episode metadata is "
                    "missing (a run was killed before finalize). Move the folder "
                    "aside and start a new dataset rather than lose it silently.")
            shutil.rmtree(root)

        if exists:
            # vcodec must be passed here too: LeRobotDataset.__init__ defaults to
            # libsvtav1, so every finalize+reopen silently switched episodes 1-49
            # of piper_pick_place to AV1 (found by the Mac, UPDATE 09-13 19:09 §5).
            ds = LeRobotDataset(repo_id, root=root, vcodec=self.args.vcodec)
            # A dataset recorded with a different camera set cannot be appended
            # to -- the Mac would get episodes with mismatched keys.
            have = {k for k in ds.features if k.startswith("observation.images.")}
            want = {f"observation.images.{k}" for k in self.cams}
            if have != want:
                raise RuntimeError(
                    f"{root} was recorded with cameras {sorted(have)} but this "
                    f"run has {sorted(want)}. Use a new --dataset name.")
            ds.start_image_writer(num_processes=0, num_threads=4 * len(self.cams))
            self.saved_episodes = ds.meta.total_episodes
            if not quiet:
                self.last_msg = f"resumed — {self.saved_episodes} episode(s) already recorded"
        else:
            ds = LeRobotDataset.create(
                repo_id,
                fps=self.args.fps,
                features=self._features(),
                root=root,
                robot_type="piper_follower",
                use_videos=True,
                image_writer_processes=0,
                image_writer_threads=4 * len(self.cams),
                vcodec=self.args.vcodec,
            )
            self.last_msg = f"created new dataset at {root}"
        return ds

    # ---- control -------------------------------------------------------

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
                "state": self.state,
                "recording": self.recording,
                "task": self.task,
                "dataset": self.args.dataset,
                "root": str(Path(self.args.root) / self.args.dataset),
                "episode_frames": self.ep_frames,
                "episode_seconds": round(time.time() - self.ep_started, 1) if self.recording else 0.0,
                "saved_episodes": self.saved_episodes,
                "target_episodes": self.args.target,
                "fps_target": self.args.fps,
                "fps_measured": round(self.measured_fps, 1),
                "arm_connected": self.arm.connected,
                "arm_error": self.arm.error,
                "arm_attempts": self.arm_attempts,
                "arm_rx_rate": round(self.arm.rx_rate),
                "ctrl_mode": self.ctrl_mode,
                "ctrl_mode_name": {0x00: "STANDBY", 0x01: "CAN", 0x02: "TEACHING"}
                                  .get(self.ctrl_mode),
                "cameras": [
                    {"key": c.key, "name": c.name,
                     "connected": c.connected and not c.stale,
                     "fps": round(c.fps, 1), "error": c.error}
                    for c in self.cams.values()
                ],
                "cams_ok": all(c.connected and not c.stale
                               and not c.degraded(self.args.min_cam_fps)
                               for c in self.cams.values()),
                "cam_problem": self.cam_problem,
                "dataset_error": self.dataset_error,
                "message": self.last_msg,
                "joints": {k: round(v, 2) for k, v in self.joints.items()},
                "mock": bool(self.args.mock_arm or self.args.mock_camera),
            }

    # ---- main loop -----------------------------------------------------

    def _arm_watch(self):
        """Keep trying to bring the arm up, so fixing it needs no restart."""
        while not self._stop_thread.is_set():
            # Count frames ourselves first. Video encoding blocks the tick loop
            # for ~12 s, during which poll() cannot run and the arm looks dead
            # from here -- reconnecting then would tear down a perfectly good
            # bus mid-session. The shim's I/O thread keeps receiving throughout,
            # so its counter is the honest check.
            self.arm.resync()
            busy = self.recording or self.state == "saving"
            if not self.arm.connected and not busy:
                was_up = self.arm.robot is not None
                self.arm_attempts += 1
                if self.arm.connect() and not was_up:
                    # Only announce a genuine offline->online transition, so
                    # this never overwrites the "saved episode N" message.
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
        """Cartesian trace for one episode, beside the dataset.

        JSON Lines, one object per recorded frame, in the dataset folder so
        Syncthing carries it to the Mac with everything else. Deliberately NOT
        a LeRobot feature: adding an unexpected key changes what the policy is
        handed, and this data is for visualisation, not training.
        """
        if not rows:
            return
        out = (Path(self.args.root) / self.args.dataset / "traces"
               / f"episode_{episode_index:06d}.jsonl")
        try:
            out.parent.mkdir(parents=True, exist_ok=True)
            with open(out, "w", encoding="utf-8") as fh:
                for r in rows:
                    fh.write(json.dumps(r, separators=(",", ":")) + "\n")
        except Exception:
            traceback.print_exc()      # never let this kill a good episode

    def _read_cameras(self):
        """Latest frame from every camera; {} unless ALL are fresh.

        Never returns a partial set: an episode with one view missing for some
        frames is worse than a shorter episode, because the Mac would train on
        a dataset whose keys are not populated consistently.
        """
        out = {}
        self.cam_problem = None
        for k, c in self.cams.items():
            if c.degraded(self.args.min_cam_fps):
                self.cam_problem = (f"{k} camera collapsed to {c.fps:.1f} fps "
                                    f"(minimum {self.args.min_cam_fps})")
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
        prev = None                  # (state_vec, {key: rgb}) awaiting its action
        gap = 0                      # consecutive ticks with no usable read
        ep_trace = []                # Cartesian rows for the take in progress
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

            # --- read hardware ---
            try:
                self.arm.poll()
                joints = self.arm.read() if self.arm.connected else None
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
                ok, buf = cv2.imencode(".jpg", cv2.cvtColor(rgb, cv2.COLOR_RGB2BGR),
                                       [int(cv2.IMWRITE_JPEG_QUALITY), 70])
                if ok:
                    self.cams[k].jpeg = buf.tobytes()

            fps_n += 1
            if time.time() - fps_t0 >= 1.0:
                with self.lock:
                    self.measured_fps = fps_n / (time.time() - fps_t0)
                fps_n, fps_t0 = 0, time.time()

            # --- start requested ---
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
                    # Every buffer that belongs to a take starts empty HERE, the
                    # one place all takes pass through. Clearing only after a
                    # save missed the discard path and, as recorded, made each
                    # trace file carry every earlier episode as well.
                    prev, gap, ep_trace = None, 0, []
                    with self.lock:
                        self.recording = True
                        self.state = "recording"
                        self.ep_frames = 0
                        self.ep_started = time.time()
                        self.last_msg = "recording…"

            # --- record one frame (lagged one tick, see ACTION) ---
            if self.recording:
                if joints is None or not frames:
                    # Skip the tick rather than the take: a single dropped read
                    # is normal, and throwing away a 20 s demo over one hiccup
                    # is worse than a one-frame gap. Only a sustained outage
                    # means the data is actually untrustworthy.
                    gap += 1
                    if gap * period < self.args.max_gap_s:
                        continue
                    with self.lock:
                        self.recording = False
                        self.state = "idle"
                        self.last_msg = (
                            f"{self.cam_problem or 'lost the arm or a camera'} "
                            f"for {gap * period:.1f}s — episode discarded")
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
                        frame = {"observation.state": prev[0],
                                 "action": vec,
                                 "task": self.task}
                        for k, img in prev[1].items():
                            frame[f"observation.images.{k}"] = img
                        self.dataset.add_frame(frame)
                        # Sidecar trace: same timestep as the frame just added,
                        # so row N here lines up with frame N in the dataset.
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

            # --- stop requested ---
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
                        msg = (f"discarded ({n} frames)" if discard else
                               f"discarded — only {n} frames, need {self.args.min_frames}")
                    else:
                        t0 = time.time()
                        # parallel_encoding spawns a ProcessPoolExecutor per
                        # camera; on Windows each child re-imports torch, which
                        # cost 19.9s for a 3.6s take. Encoding both streams in
                        # this process is far quicker. (LeRobot already defaults
                        # this off on macOS for the same reason.)
                        self.dataset.save_episode(parallel_encoding=False)
                        # Episode metadata is buffered in RAM and the parquet
                        # footer is only written on finalize(), so a dataset left
                        # open is unreadable -- a crash would lose every episode
                        # recorded so far. Close and reopen in resume mode after
                        # each take: the file index advances, nothing is
                        # overwritten, and what is on disk is always valid and
                        # safe for Syncthing to carry to the Mac.
                        self.dataset.finalize()
                        self.dataset = self._open_dataset(quiet=True)
                        with self.lock:
                            self.saved_episodes = self.dataset.meta.total_episodes
                        self._write_trace(self.saved_episodes - 1, ep_trace)
                        msg = (f"saved episode {self.saved_episodes} — {n} frames "
                               f"({n / self.args.fps:.1f}s) in {time.time() - t0:.1f}s")
                except Exception as e:
                    traceback.print_exc()
                    msg = f"save failed: {e}"

                # Encoding blocked this thread for seconds; tell the arm and the
                # cameras that the silence was ours, not theirs.
                self.arm.resync()
                with self.lock:
                    self.ep_frames = 0
                    self.state = "idle"
                    self.last_msg = msg
                next_tick = time.perf_counter()

        # --- shutdown ---
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


# --------------------------------------------------------------------------
# web
# --------------------------------------------------------------------------

PAGE = r"""<!doctype html>
<meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>PiPER recorder</title>
<style>
 :root{--bg:#0f1115;--card:#171a21;--line:#262b36;--fg:#e7eaf0;--dim:#9aa3b2;
       --ok:#3fb950;--rec:#f85149;--warn:#d29922;--acc:#58a6ff}
 *{box-sizing:border-box} body{margin:0;background:var(--bg);color:var(--fg);
   font:14px/1.5 system-ui,-apple-system,Segoe UI,sans-serif}
 .wrap{max-width:1120px;margin:0 auto;padding:20px 16px}
 h1{font-size:17px;margin:0 0 2px} .sub{color:var(--dim);font-size:12px}
 .grid{display:grid;grid-template-columns:1fr 300px;gap:16px;margin-top:14px}
 @media(max-width:860px){.grid{grid-template-columns:1fr}}
 .card{background:var(--card);border:1px solid var(--line);border-radius:10px;padding:14px}
 .cams{display:grid;grid-template-columns:repeat(auto-fit,minmax(240px,1fr));gap:12px}
 .cam img{width:100%;aspect-ratio:4/3;background:#000;border-radius:8px;
          object-fit:contain;display:block}
 .cam .lbl{display:flex;justify-content:space-between;font-size:12px;margin-top:6px}
 .bigbtn{width:100%;padding:18px;font-size:17px;font-weight:600;border:0;border-radius:10px;
   cursor:pointer;color:#fff;background:var(--ok);margin-bottom:8px}
 .bigbtn.rec{background:var(--rec)} .bigbtn:disabled{opacity:.4;cursor:not-allowed}
 .ghost{width:100%;padding:9px;border-radius:8px;border:1px solid var(--line);
   background:transparent;color:var(--dim);cursor:pointer}
 .ghost:disabled{opacity:.35;cursor:not-allowed}
 .row{display:flex;justify-content:space-between;gap:8px;padding:4px 0;font-size:13px}
 .row span:first-child{color:var(--dim)} .mono{font-variant-numeric:tabular-nums}
 .dot{display:inline-block;width:8px;height:8px;border-radius:50%;margin-right:6px}
 .j{display:grid;grid-template-columns:52px 1fr 50px;gap:8px;align-items:center;
    font-size:11px;color:var(--dim);margin-bottom:3px}
 .bar{height:6px;background:#0b0d11;border-radius:3px;position:relative;overflow:hidden}
 .bar i{position:absolute;top:0;bottom:0;background:var(--acc);border-radius:3px}
 input{width:100%;background:#0b0d11;border:1px solid var(--line);color:var(--fg);
   border-radius:8px;padding:8px;font:inherit}
 .msg{margin-top:10px;padding:9px;border-radius:8px;background:#0b0d11;color:var(--dim);
   font-size:12px;min-height:34px;word-break:break-word}
 .err{color:var(--rec)} .kbd{color:var(--dim);font-size:11px;text-align:center;margin-top:8px}
 .prog{height:5px;background:#0b0d11;border-radius:3px;overflow:hidden;margin-top:6px}
 .prog i{display:block;height:100%;background:var(--ok)}
 .banner{background:#2d2410;border:1px solid var(--warn);color:var(--warn);padding:9px 11px;
   border-radius:8px;font-size:12px;margin-top:12px}
</style>
<div class="wrap">
  <h1>PiPER drag-teach recorder</h1>
  <div class="sub">Press the arm's drag-teach button, move it by hand, record.</div>
  <div id="banner" class="banner" hidden></div>
  <div class="grid">
    <div class="card">
      <div class="cams" id="cams"></div>
      <div style="margin-top:12px">
        <div class="sub" style="margin-bottom:4px">Task (stored with every frame)</div>
        <input id="task" placeholder="push the red block to the marked square">
      </div>
    </div>
    <div>
      <div class="card">
        <button id="go" class="bigbtn">Start recording</button>
        <button id="discard" class="ghost" disabled>Discard this take</button>
        <div class="kbd">space = start/stop &nbsp;·&nbsp; esc = discard</div>
        <div class="msg" id="msg"></div>
      </div>
      <div class="card" style="margin-top:12px">
        <div class="row"><span>Episodes</span><b class="mono" id="eps">0</b></div>
        <div class="prog"><i id="bar" style="width:0%"></i></div>
        <div class="row" style="margin-top:8px"><span>This take</span><b class="mono" id="cur">—</b></div>
        <div class="row"><span>Loop</span><b class="mono" id="fps">—</b></div>
        <div class="row"><span>Arm</span><b id="arm">—</b></div>
        <div class="row"><span>Mode</span><b id="mode">—</b></div>
      </div>
      <div class="card" style="margin-top:12px">
        <div class="sub" style="margin:0 0 8px">Joints (normalized)</div>
        <div id="joints"></div>
      </div>
    </div>
  </div>
</div>
<script>
const $=i=>document.getElementById(i);
let st={}, taskDirty=false, camsBuilt="";
$('task').addEventListener('input',()=>taskDirty=true);
const dot=ok=>`<span class="dot" style="background:${ok?'var(--ok)':'var(--rec)'}"></span>`;

async function poll(){
  try{
    const r=await fetch('/api/status'); st=await r.json();
    const rec=st.state==='recording', busy=st.state==='saving'||st.state==='starting';
    const go=$('go');
    go.textContent=rec?'Stop & save':(busy?'Working…':'Start recording');
    go.className='bigbtn'+(rec?' rec':''); go.disabled=busy;
    $('discard').disabled=!rec;
    $('eps').textContent=`${st.saved_episodes} / ${st.target_episodes}`;
    $('bar').style.width=Math.min(100,100*st.saved_episodes/st.target_episodes)+'%';
    $('cur').textContent=rec?`${st.episode_frames} f · ${st.episode_seconds}s`:'—';
    $('fps').textContent=`${st.fps_measured} / ${st.fps_target} fps`;
    $('arm').innerHTML=dot(st.arm_connected)+(st.arm_connected
      ?`live · ${st.arm_rx_rate} f/s`:'no frames');
    $('mode').innerHTML=st.ctrl_mode_name
      ? dot(st.ctrl_mode===2)+st.ctrl_mode_name : '—';

    const sig=st.cameras.map(c=>c.key).join(',');
    if(sig!==camsBuilt){ camsBuilt=sig;
      $('cams').innerHTML=st.cameras.map(c=>`<div class="cam">
        <img src="/preview/${c.key}.mjpg" alt="${c.key}">
        <div class="lbl"><b>${c.key}</b><span id="cam-${c.key}"></span></div></div>`).join(''); }
    st.cameras.forEach(c=>{const e=$('cam-'+c.key);
      if(e)e.innerHTML=dot(c.connected)+(c.connected
        ?`${c.name} · ${c.fps} fps`:(c.error||'no frames'));});

    const err=st.dataset_error||st.arm_error||(st.cameras.find(c=>c.error)||{}).error;
    $('msg').innerHTML=(err?`<span class="err">${err}</span><br>`:'')+(st.message||'');
    if(!taskDirty&&st.task&&$('task').value!==st.task)$('task').value=st.task;

    const b=$('banner');
    if(st.mock){b.hidden=false;b.innerHTML='MOCK MODE — synthetic data, not real demos.';}
    else if(st.ctrl_mode===0){b.hidden=false;b.innerHTML=
      '<b>Arm is in STANDBY</b> — it answers queries but does not stream joint '+
      'feedback, so recording is blocked. <b>Press the drag-teach button</b> to '+
      'enter TEACHING mode, which both frees the joints and starts reporting.';}
    else if(!st.arm_connected){b.hidden=false;b.innerHTML=
      `<b>Arm sending no frames</b> — retried ${st.arm_attempts}×, still trying `+
      `(no restart needed). Check 24&nbsp;V power, base LED, CAN cable, and that `+
      `the teleop server on :8770 is not running.`;}
    else if(!st.cams_ok){b.hidden=false;b.innerHTML=
      `<b>${st.cam_problem||'A camera is offline'}</b> — recording is blocked, so `+
      `no episode is saved with a frozen or missing view. Restart the portal; `+
      `if it persists, replug that camera.`;}
    else b.hidden=true;

    $('joints').innerHTML=Object.entries(st.joints||{}).map(([k,v])=>{
      const w=Math.min(50,Math.abs(v)/2);
      return `<div class="j"><span>${k}</span><div class="bar"><i style="${v<0
        ?`right:50%;width:${w}%`:`left:50%;width:${w}%`}"></i></div>
        <span class="mono" style="text-align:right">${v.toFixed(1)}</span></div>`}).join('');
  }catch(e){$('msg').innerHTML='<span class="err">portal unreachable</span>'}
}
async function post(u,b){await fetch(u,{method:'POST',headers:{'Content-Type':'application/json'},
  body:JSON.stringify(b||{})}); taskDirty=false; poll();}
function toggle(){ st.state==='recording' ? post('/api/stop',{discard:false})
                                          : post('/api/start',{task:$('task').value}); }
$('go').onclick=toggle;
$('discard').onclick=()=>post('/api/stop',{discard:true});
addEventListener('keydown',e=>{
  if(e.target.tagName==='INPUT')return;
  if(e.code==='Space'){e.preventDefault();toggle()}
  if(e.code==='Escape'&&st.state==='recording')post('/api/stop',{discard:true});
});
poll(); setInterval(poll,300);
</script>
"""


def make_handler(rec, on_shutdown):
    class H(BaseHTTPRequestHandler):
        protocol_version = "HTTP/1.1"

        def log_message(self, *a):
            pass

        def _send(self, code, body, ctype="application/json"):
            if isinstance(body, str):
                body = body.encode()
            self.send_response(code)
            self.send_header("Content-Type", ctype)
            self.send_header("Content-Length", str(len(body)))
            self.send_header("Cache-Control", "no-store")
            self.end_headers()
            self.wfile.write(body)

        def do_GET(self):
            if self.path == "/":
                return self._send(200, PAGE, "text/html; charset=utf-8")
            if self.path == "/api/status":
                return self._send(200, json.dumps(rec.status()))
            if self.path.startswith("/preview/") and self.path.endswith(".mjpg"):
                key = self.path[len("/preview/"):-len(".mjpg")]
                if key in rec.cams:
                    return self._mjpeg(rec.cams[key])
            return self._send(404, json.dumps({"error": "not found"}))

        def _mjpeg(self, cam):
            self.send_response(200)
            self.send_header("Content-Type",
                             "multipart/x-mixed-replace; boundary=frame")
            self.send_header("Cache-Control", "no-store")
            self.end_headers()
            try:
                while True:
                    jpg = cam.jpeg
                    if jpg:
                        self.wfile.write(b"--frame\r\nContent-Type: image/jpeg\r\n"
                                         b"Content-Length: " + str(len(jpg)).encode()
                                         + b"\r\n\r\n" + jpg + b"\r\n")
                    time.sleep(1.0 / 15)
            except (BrokenPipeError, ConnectionResetError, OSError):
                pass

        def do_POST(self):
            n = int(self.headers.get("Content-Length") or 0)
            try:
                body = json.loads(self.rfile.read(n) or b"{}")
            except Exception:
                body = {}
            if self.path == "/api/start":
                ok, msg = rec.start_episode(body.get("task"))
            elif self.path == "/api/stop":
                ok, msg = rec.stop_episode(bool(body.get("discard")))
            elif self.path == "/api/shutdown":
                # Lets a caller stop this cleanly instead of hard-killing it,
                # which would leave the candleLight's buffers full and wedge it.
                self._send(200, json.dumps({"ok": True, "message": "shutting down"}))
                on_shutdown()
                return
            else:
                return self._send(404, json.dumps({"error": "not found"}))
            return self._send(200, json.dumps({"ok": ok, "message": msg}))

    return H


def parse_camera(spec):
    if "=" not in spec:
        raise argparse.ArgumentTypeError(
            f"--camera needs key=name, e.g. wrist=\"Dabai DC1\" (got {spec!r})")
    key, name = spec.split("=", 1)
    key, name = key.strip(), name.strip()
    if not key.isidentifier():
        raise argparse.ArgumentTypeError(
            f"camera key {key!r} must be a plain identifier — it becomes the "
            "dataset key observation.images.<key>")
    return key, name


def main():
    p = argparse.ArgumentParser(description=__doc__,
                                formatter_class=argparse.RawDescriptionHelpFormatter)
    p.add_argument("--dataset", default="piper_push_block")
    p.add_argument("--task", default="push the block to the target")
    p.add_argument("--root", default=str(DEFAULT_ROOT))
    p.add_argument("--fps", type=int, default=15)
    p.add_argument("--target", type=int, default=50, help="episode goal, for the progress bar")
    p.add_argument("--min-cam-fps", type=float, default=5.0,
                   help="refuse/abort recording if a camera's rate falls below this")
    p.add_argument("--max-gap-s", type=float, default=1.5,
                   help="abort a take only after this long with no usable read")
    p.add_argument("--min-frames", type=int, default=30,
                   help="takes shorter than this are auto-discarded (30 = 2s at 15fps)")
    p.add_argument("--port", dest="http_port", type=int, default=8790)
    p.add_argument("--can-port", dest="port", default="candle")
    p.add_argument("--camera", dest="cameras", action="append", type=parse_camera,
                   metavar='KEY=NAME',
                   help='repeatable, e.g. --camera wrist="Dabai DC1" '
                        '--camera scene="Logi C270". Matched by NAME, never index.')
    p.add_argument("--width", type=int, default=640)
    p.add_argument("--height", type=int, default=480)
    p.add_argument("--vcodec", default="h264", choices=["h264", "hevc", "libsvtav1"],
                   help="h264 encodes far faster than av1 on this CPU")
    p.add_argument("--mock-arm", action="store_true")
    p.add_argument("--mock-camera", action="store_true")
    args = p.parse_args()
    if not args.cameras:
        args.cameras = [("wrist", "Dabai DC1")]

    rec = Recorder(args)
    rec.start()
    for _ in range(300):
        if rec.status()["state"] != "starting":
            break
        time.sleep(0.1)

    s = rec.status()
    print(f"\n  dataset : {s['root']}")
    print(f"  arm     : {'live' if s['arm_connected'] else 'NO FRAMES — ' + str(s['arm_error'])}")
    if s["ctrl_mode_name"]:
        print(f"  ctrl    : {s['ctrl_mode_name']}")
    for c in s["cameras"]:
        print(f"  camera  : {c['key']:<8} {c['name']:<22} "
              f"{'ok' if c['connected'] else 'OFFLINE — ' + str(c['error'])}")
    if s["dataset_error"]:
        print(f"  DATASET : {s['dataset_error']}")
    print(f"\n  ->  http://127.0.0.1:{args.http_port}\n")

    srv = ThreadingHTTPServer(("127.0.0.1", args.http_port),
                              make_handler(rec, lambda: _bye()))
    srv.daemon_threads = True

    # A hard kill leaves the candleLight's buffers full, which wedges it until
    # it is physically replugged. Catch the polite termination signals so the
    # adapter is always released properly.
    def _bye(*_):
        threading.Thread(target=srv.shutdown, daemon=True).start()

    for signame in ("SIGTERM", "SIGINT", "SIGBREAK"):
        sig = getattr(signal, signame, None)
        if sig is not None:
            try:
                signal.signal(sig, _bye)
            except (ValueError, OSError):
                pass

    try:
        srv.serve_forever()
    except KeyboardInterrupt:
        pass
    finally:
        print("\nshutting down — releasing adapter, finalizing dataset…")
        rec.shutdown()
        rec.join(timeout=30)
        print("done.")


if __name__ == "__main__":
    main()
