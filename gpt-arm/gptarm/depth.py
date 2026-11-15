"""The wrist camera's depth: what is under the gripper, in the arm's millimetres.

The Orbbec Dabai DC1 on the wrist has a depth sensor beside its colour camera. Orbbec's current SDK does
not drive the DC1; its OpenNI2 driver does. The OpenNI2 runtime and the Orbbec driver come from Orbbec's
ROS 2 camera package (github.com/orbbec/ros2_astra_camera, astra_camera/openni2_redist/x64, Apache-2.0),
read through the `openni` Python bindings (MIT). The USB device needs Orbbec's udev rule
(astra_camera/scripts/56-orbbec-usb.rules in /etc/udev/rules.d).

Where the depth camera sits on the gripper is measured by `python -m gptarm.depth calibrate X Y TOP`: with
a bottle standing at a known spot, the arm looks at its cap from a dozen poses; each view gives the cap
top's centre in the camera (from depth) and in the flange frame (from the arm's pose), and the rigid
transform between the two sets is a closed-form fit (Kabsch). Saved to ~/.config/gptarm/wrist_depth.json.

What the model gets each step (`summary`): the height of whatever is straight under the fingertips, and the
nearest object below the gripper: its top's centre (x, y), its top height and its size, all in the arm's
frame, or nothing if the depth camera sees only the table.
"""
from __future__ import annotations

import json
import math
import os
import threading
import time
from pathlib import Path

import numpy as np

from . import arm as A

HOME = Path(os.environ.get("GPTARM_HOME", "~/.config/gptarm")).expanduser()
CAL_PATH = HOME / "wrist_depth.json"
REDIST = [Path(os.environ.get("GPTARM_OPENNI", "~/gpt-arm/vendor-openni")).expanduser(),
          Path("~/src/ros2_astra_camera/astra_camera/openni2_redist/x64").expanduser()]
W, H = 640, 480
HFOV, VFOV = 67.7, 45.5        # degrees, as the driver reports them for the 640x480 depth mode


class DepthError(Exception):
    pass


def intrinsics(w=W, h=H):
    fx = (w / 2.0) / math.tan(math.radians(HFOV) / 2.0)
    fy = (h / 2.0) / math.tan(math.radians(VFOV) / 2.0)
    return fx, fy, w / 2.0, h / 2.0


def backproject(u, v, z_mm, K=None):
    fx, fy, cx, cy = K or intrinsics()
    return np.array([(u - cx) * z_mm / fx, (v - cy) * z_mm / fy, z_mm], float)


def cloud(depth, step=4, K=None):
    """(N, 3) camera-frame points (mm) from a depth image, every `step` pixels."""
    fx, fy, cx, cy = K or intrinsics(depth.shape[1], depth.shape[0])
    d = depth[::step, ::step].astype(float)
    vs, us = np.mgrid[0:depth.shape[0]:step, 0:depth.shape[1]:step]
    ok = (d > 60) & (d < 1200)
    z = d[ok]
    return np.column_stack([(us[ok] - cx) * z / fx, (vs[ok] - cy) * z / fy, z])


class WristDepth:
    """Keeps the depth stream open in a thread; `frame()` is the newest depth image (uint16 mm)."""

    def __init__(self):
        self.lock = threading.Lock()
        self.latest, self.t, self.error = None, 0.0, None
        self._stop = threading.Event()
        threading.Thread(target=self._run, daemon=True, name="wrist-depth").start()

    def _run(self):
        try:
            from openni import openni2
        except ImportError:
            self.error = "the openni package is not installed (pip install openni)"
            return
        redist = next((str(p) for p in REDIST if (p / "libOpenNI2.so").exists()), None)
        if redist is None:
            self.error = "the OpenNI2 runtime is missing (see gptarm/depth.py)"
            return
        while not self._stop.is_set():
            try:
                openni2.initialize(redist)
                dev = openni2.Device.open_any()
                s = dev.create_depth_stream()
                s.set_mirroring_enabled(False)
                s.start()
                self.error = None
                while not self._stop.is_set():
                    f = s.read_frame()
                    a = np.frombuffer(f.get_buffer_as_uint16(), dtype=np.uint16).reshape(f.height, f.width).copy()
                    with self.lock:
                        self.latest, self.t = a, time.time()
                s.stop()
            except Exception as e:                           # noqa: BLE001 (unplugged, busy: try again)
                self.error = f"wrist depth: {e}"
                try:
                    openni2.unload()
                except Exception:                            # noqa: BLE001
                    pass
                time.sleep(2.0)

    def frame(self, max_age_s=1.0):
        t_end = time.time() + 3.0
        while time.time() < t_end:
            with self.lock:
                if self.latest is not None and time.time() - self.t < max_age_s:
                    return self.latest.copy()
            time.sleep(0.03)
        raise DepthError(self.error or "wrist depth: no frame")

    def settled(self, n=5):
        """The per-pixel median of n frames (zeros ignored)."""
        frames = []
        for _ in range(n):
            frames.append(self.frame().astype(float))
            time.sleep(0.06)
        a = np.stack(frames)
        a[a == 0] = np.nan
        return np.nan_to_num(np.nanmedian(a, axis=0)).astype(np.uint16)

    def close(self):
        self._stop.set()


class Mount:
    """Where the depth camera sits in the flange frame: p_flange = R @ p_cam + t."""

    def __init__(self, R, t, residual_mm=None, when=None):
        self.R, self.t = np.asarray(R, float).reshape(3, 3), np.asarray(t, float).reshape(3)
        self.residual_mm, self.when = residual_mm, when or time.time()

    def to_base(self, pts_cam, flange_pose):
        p, R = A.K.pose_to_rt(flange_pose)
        return (np.asarray(pts_cam) @ self.R.T + self.t) @ np.asarray(R).T + np.asarray(p, float)

    def save(self, path=None):
        path = Path(path or CAL_PATH)
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(json.dumps({"R": self.R.tolist(), "t": self.t.tolist(), "residual_mm": self.residual_mm,
                                    "when": self.when}, indent=1))

    @classmethod
    def load(cls, path=None):
        try:
            d = json.loads(Path(path or CAL_PATH).read_text())
            return cls(d["R"], d["t"], d.get("residual_mm"), d.get("when"))
        except (OSError, ValueError, KeyError):
            return None


def kabsch(P, Q):
    """R, t minimising |R @ P_i + t - Q_i| (rows are points), and each point's error."""
    P, Q = np.asarray(P, float), np.asarray(Q, float)
    cp, cq = P.mean(axis=0), Q.mean(axis=0)
    U, _, Vt = np.linalg.svd((P - cp).T @ (Q - cq))
    d = np.sign(np.linalg.det(Vt.T @ U.T))
    R = Vt.T @ np.diag([1.0, 1.0, d]) @ U.T
    t = cq - R @ cp
    return R, t, np.linalg.norm(P @ R.T + t - Q, axis=1)


def nearest_top(depth, max_range_mm=None, min_px=150):
    """The nearest compact surface in the depth image (a cap seen from above): (u, v, depth mm, pixels), or None."""
    d = depth.astype(float)
    ok = d > 60
    if ok.sum() < 500:
        return None
    near = np.percentile(d[ok], 0.3)               # the nearest surface, robust to a few noisy pixels
    band = ok & (d < near + 18.0)
    if max_range_mm is not None:
        band &= d < max_range_mm
    import cv2
    n, lab, st, cen = cv2.connectedComponentsWithStats(band.astype(np.uint8))
    best = None
    for i in range(1, n):
        a = st[i, cv2.CC_STAT_AREA]
        x, y, w, h = st[i, :4]
        if a < min_px or x == 0 or y == 0 or x + w >= d.shape[1] or y + h >= d.shape[0]:
            continue                                       # touching the edge: not a whole top
        if best is None or a > best[3]:
            best = (float(cen[i][0]), float(cen[i][1]), float(np.median(d[lab == i])), int(a))
    return best


def summary(depth, flange_pose, mount, table_z=0.0):
    """What the model is told: the surface under the fingertips and the nearest object below the gripper."""
    pts = mount.to_base(cloud(depth), flange_pose)
    if len(pts) < 200:
        return {"seen": False}
    p, R = A.K.pose_to_rt(flange_pose)
    tool = np.asarray(p, float) + np.asarray(R) @ np.array([0.0, 0.0, A.GRASP_OFFSET_MM])
    tip_z = min(float(t[2]) for t in A.tip_points(flange_pose))
    below = pts[np.hypot(pts[:, 0] - tool[0], pts[:, 1] - tool[1]) < 12.0]
    out = {"seen": True}
    if len(below) >= 5:
        top = float(np.percentile(below[:, 2], 90))
        out["surface_under_tool_z_mm"] = round(top)
        out["fingertips_above_it_mm"] = round(tip_z - top)
    objs = pts[(pts[:, 2] > table_z + 15.0) & (pts[:, 2] < tool[2] - 20.0)]
    objs = objs[np.hypot(objs[:, 0] - tool[0], objs[:, 1] - tool[1]) < 200.0]
    if len(objs) >= 20:
        topz = float(np.percentile(objs[:, 2], 95))
        cap = objs[objs[:, 2] > topz - 15.0]
        c = cap[:, :2].mean(axis=0)
        ext = np.ptp(objs[:, :2], axis=0)
        out["nearest_object"] = {"top_centre_xy_mm": [round(float(c[0])), round(float(c[1]))], "top_z_mm": round(topz),
                                 "extent_xy_mm": [round(float(ext[0])), round(float(ext[1]))],
                                 "offset_from_tool_xy_mm": [round(float(c[0] - tool[0])), round(float(c[1] - tool[1]))]}
    return out


def calibrate(arm, depth, x, y, top_z, log=print, save=True):
    """Look at a standing object's top (centre (x, y), height top_z, mm) from several poses; fit the mount."""
    P, Q = [], []
    poses = [(dx, dy, z, lean) for z in (230.0, 280.0) for (dx, dy) in ((0, 0), (40, 0), (-40, 0), (0, 40), (0, -40), (30, 30))
             for lean in (None,)]
    target = np.array([x, y, top_z], float)
    for dx, dy, z, lean in poses:
        try:
            arm.move_tool(x + dx, y + dy, z, lean=lean)
        except A.ArmError as e:
            log(f"  ({x + dx:.0f}, {y + dy:.0f}, {z:.0f}): {e}")
            continue
        arm.hold()
        time.sleep(0.8)
        d = depth.settled(5)
        hit = nearest_top(d)
        if hit is None:
            log(f"  ({x + dx:.0f}, {y + dy:.0f}, {z:.0f}): no top seen")
            continue
        u, v, zc, n = hit
        pose = arm.raw_state()["pose_current"]
        pf, Rf = A.K.pose_to_rt(pose)
        q = np.asarray(Rf).T @ (target - np.asarray(pf, float))      # the cap's centre in the flange frame
        P.append(backproject(u, v, zc))
        Q.append(q)
        log(f"  ({x + dx:.0f}, {y + dy:.0f}, {z:.0f}): top at px ({u:.0f}, {v:.0f}), {zc:.0f} mm away, {n} px")
    if len(P) < 4:
        raise DepthError(f"only {len(P)} views of the object; at least 4 are needed")
    R, t, err = kabsch(np.array(P), np.array(Q))
    keep = err < max(3.0 * np.median(err), 10.0)
    if keep.sum() >= 4 and not keep.all():
        R, t, err = kabsch(np.array(P)[keep], np.array(Q)[keep])
    m = Mount(R, t, round(float(np.sqrt((err ** 2).mean())), 1))
    log(f"mount: camera at {np.round(t).tolist()} mm in the flange frame, {m.residual_mm} mm rms over {len(err)} views")
    if save:
        m.save()
    return m


def main():
    import argparse
    ap = argparse.ArgumentParser(description="Wrist depth camera: calibrate its mount, or print what it sees")
    sub = ap.add_subparsers(dest="cmd", required=True)
    c = sub.add_parser("calibrate", help="a standing object's top at (x, y), top_z mm high; the arm moves")
    c.add_argument("x", type=float)
    c.add_argument("y", type=float)
    c.add_argument("top_z", type=float)
    sub.add_parser("show", help="print the summary at the current pose (the arm does not move)")
    a = ap.parse_args()
    arm, dep = A.Arm(speed=20, floor_z_mm=60.0), WristDepth()
    try:
        if a.cmd == "calibrate":
            calibrate(arm, dep, a.x, a.y, a.top_z)
        else:
            m = Mount.load()
            if m is None:
                raise SystemExit("not calibrated: run `python -m gptarm.depth calibrate X Y TOP` first")
            print(json.dumps(summary(dep.settled(5), arm.raw_state()["pose_current"], m)))
    finally:
        arm.hold()
        dep.close()


if __name__ == "__main__":
    main()
