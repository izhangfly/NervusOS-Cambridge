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
W, H = 320, 200               # the depth mode used: 320x200 at 10 fps, about 10 Mbit/s (640x480 at 30 fps is about
MODE = (320, 200, 10)          # 150 Mbit/s and starved the arm's CAN adapter on the shared USB 2 hub)
HFOV, VFOV = 67.7, 45.5        # degrees; replaced by what the driver reports for the mode in use


class DepthError(Exception):
    pass


def intrinsics(w=W, h=H):
    fx = (w / 2.0) / math.tan(math.radians(HFOV) / 2.0)
    fy = (h / 2.0) / math.tan(math.radians(VFOV) / 2.0)
    return fx, fy, w / 2.0, h / 2.0


def backproject(u, v, z_mm, K=None):
    fx, fy, cx, cy = K or intrinsics()
    return np.array([(u - cx) * z_mm / fx, (v - cy) * z_mm / fy, z_mm], float)


def cloud(depth, step=None, K=None):
    """(N, 3) camera-frame points (mm) from a depth image, every `step` pixels."""
    step = step or max(1, depth.shape[1] // 160)
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
        global HFOV, VFOV
        while not self._stop.is_set():
            try:
                openni2.initialize(redist)
                dev = openni2.Device.open_any()
                s = dev.create_depth_stream()
                modes = dev.get_sensor_info(openni2.SENSOR_DEPTH).videoModes
                want = [m for m in modes if (m.resolutionX, m.resolutionY, m.fps) == MODE
                        and m.pixelFormat == openni2.PIXEL_FORMAT_DEPTH_1_MM]
                if want:
                    s.set_video_mode(want[0])
                s.set_mirroring_enabled(False)
                s.start()
                HFOV, VFOV = math.degrees(s.get_horizontal_fov()), math.degrees(s.get_vertical_fov())
                self.error = None
                while not self._stop.is_set():
                    # read_frame() blocks for ever once the device is gone (a USB reset): wait with a timeout
                    if openni2.wait_for_any_stream([s], 3.0) is None:
                        raise DepthError("no depth frames for 3 s")
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
        import warnings
        with warnings.catch_warnings():
            warnings.simplefilter("ignore", RuntimeWarning)          # pixels with no reading in any frame
            return np.nan_to_num(np.nanmedian(a, axis=0)).astype(np.uint16)

    def close(self):
        self._stop.set()


class Mount:
    """Where the depth camera sits in the flange frame: p_flange = R @ p_cam + t. focal_scale corrects the
    driver's field of view (fitted with the mount)."""

    def __init__(self, R, t, residual_mm=None, when=None, focal_scale=1.0):
        self.R, self.t = np.asarray(R, float).reshape(3, 3), np.asarray(t, float).reshape(3)
        self.residual_mm, self.when = residual_mm, when or time.time()
        self.focal_scale = float(focal_scale)

    def to_base(self, pts_cam, flange_pose):
        p, R = A.K.pose_to_rt(flange_pose)
        return (np.asarray(pts_cam) @ self.R.T + self.t) @ np.asarray(R).T + np.asarray(p, float)

    def save(self, path=None):
        path = Path(path or CAL_PATH)
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(json.dumps({"R": self.R.tolist(), "t": self.t.tolist(), "residual_mm": self.residual_mm,
                                    "when": self.when, "focal_scale": self.focal_scale}, indent=1))

    @classmethod
    def load(cls, path=None):
        try:
            d = json.loads(Path(path or CAL_PATH).read_text())
            return cls(d["R"], d["t"], d.get("residual_mm"), d.get("when"), d.get("focal_scale", 1.0))
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


def nearest_top(depth, max_range_mm=None, min_px=None):
    """The nearest compact surface in the depth image (a cap seen from above): (u, v, depth mm, pixels), or None."""
    d = depth.astype(float)
    min_px = min_px or int(150 * d.size / (640 * 480))
    ok = d > 60
    if ok.sum() < 0.002 * d.size:
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


def colourise(depth, near=100.0, far=600.0):
    """A depth image as a picture: near is warm, far is cool, no reading is black; with a scale bar."""
    import cv2
    d = depth.astype(float)
    v = np.clip((d - near) / (far - near), 0.0, 1.0)
    img = cv2.applyColorMap((255 * (1.0 - v)).astype(np.uint8), cv2.COLORMAP_TURBO)
    img[d <= 0] = 0
    h, w = img.shape[:2]
    bar = cv2.applyColorMap(np.linspace(255, 0, w - 40).astype(np.uint8)[None, :].repeat(10, axis=0), cv2.COLORMAP_TURBO)
    img[h - 26:h - 16, 20:w - 20] = bar
    for mm in range(int(near), int(far) + 1, 100):
        x = int(20 + (mm - near) / (far - near) * (w - 41))
        cv2.putText(img, f"{mm}", (x - 12, h - 32), cv2.FONT_HERSHEY_SIMPLEX, 0.4, (255, 255, 255), 1, cv2.LINE_AA)
    ok = d > 0
    if ok.any():
        nearest = float(np.percentile(d[ok], 0.3))
        cv2.putText(img, f"nearest {nearest:.0f} mm", (10, 22), cv2.FONT_HERSHEY_SIMPLEX, 0.6, (255, 255, 255), 2, cv2.LINE_AA)
    return img


def summary(depth, flange_pose, mount, table_z=0.0):
    """What the model is told: the surface under the fingertips and the nearest object below the gripper."""
    pts = cloud(depth)
    pts[:, :2] /= getattr(mount, "focal_scale", 1.0)
    pts = mount.to_base(pts, flange_pose)
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


def fit_plane(pts, max_angle_deg=60.0, iters=300, tol_mm=6.0, rng=None):
    """The dominant plane facing the camera in camera-frame points: (unit normal pointing at the camera, distance
    from the camera, inlier count), or None. RANSAC, then a least-squares refit on the inliers."""
    pts = np.asarray(pts, float)
    if len(pts) < 100:
        return None
    rng = rng or np.random.default_rng(0)
    best = None
    cos_max = math.cos(math.radians(max_angle_deg))
    for _ in range(iters):
        a, b, c = pts[rng.choice(len(pts), 3, replace=False)]
        n = np.cross(b - a, c - a)
        if np.linalg.norm(n) < 1e-6:
            continue
        n /= np.linalg.norm(n)
        if n[2] > 0:
            n = -n                                         # point from the plane towards the camera
        if -n[2] < cos_max:                                # the camera looks along +z: the table must face it
            continue
        d = -float(n @ a)
        inl = np.abs(pts @ n + d) < tol_mm
        if best is None or inl.sum() > best[1].sum():
            best = (n, inl)
    if best is None or best[1].sum() < 0.3 * len(pts):
        return None
    q = pts[best[1]]
    c = q.mean(axis=0)
    n = np.linalg.svd(q - c)[2][-1]
    if n[2] > 0:
        n = -n
    return n, float(-(n @ c)), int(best[1].sum())


def plane_fit_mount(views):
    """views: [(R_flange (3x3), p_flange (3,), n_cam, d_cam)] -> Mount. The table is z = 0. In the flange frame
    'up' is a = R_flange^T e_z; the camera sees it as n_cam, so R @ n_cam = a (direction pairs: Kabsch); and the
    camera's height above the table, d_cam, is a . t + p_z (one linear equation per view)."""
    A_dirs = np.array([np.asarray(R).T @ np.array([0.0, 0.0, 1.0]) for R, _, _, _ in views])
    N = np.array([n for _, _, n, _ in views])
    U, _, Vt = np.linalg.svd(N.T @ A_dirs)
    dsign = np.sign(np.linalg.det(Vt.T @ U.T))
    R = Vt.T @ np.diag([1.0, 1.0, dsign]) @ U.T
    b = np.array([d - float(p[2]) for _, p, _, d in views])
    t, *_ = np.linalg.lstsq(A_dirs, b, rcond=None)
    ang = [math.degrees(math.acos(max(-1.0, min(1.0, float((R @ n) @ a))))) for n, a in zip(N, A_dirs)]
    res_h = np.abs(A_dirs @ t - b)
    sv = np.linalg.svd(A_dirs, compute_uv=False)
    return Mount(R, t, round(float(np.sqrt((res_h ** 2).mean())), 1)), {"angle_err_deg": round(float(np.mean(ang)), 2),
                                                                        "height_err_mm": round(float(res_h.mean()), 1),
                                                                        "conditioning": round(float(sv[-1] / sv[0]), 3)}


def table_poses():
    """Tool points over free table (y > 0: away from where the basket usually stands), with leans and twists of
    the gripper about its own axis, so that 'up' takes many directions in the flange frame."""
    out = []
    for x, y, z in ((260.0, 150.0, 260.0), (320.0, 220.0, 280.0), (220.0, 60.0, 280.0)):
        for lean in (30.0, 45.0):
            for twist in (-60.0, 0.0, 60.0):
                out.append((x, y, z, lean, twist))
    return out


def _pose_with_twist(x, y, z, lean, twist):
    yaw = math.degrees(math.atan2(y, x))
    R = A.tool_rotation(yaw, lean) @ A._rz(math.radians(twist))
    p = np.array([x, y, z]) - R @ np.array([0.0, 0.0, A.GRASP_OFFSET_MM])
    return p, R


def _fit_views(raw, scale):
    """Planes from each view's points with the focal length scaled by `scale`, then the mount."""
    views = []
    for R, p, pts in raw:
        q = pts.copy()
        q[:, :2] /= scale                                   # x = (u - cx) z / (scale * f)
        n = np.linalg.svd(q - q.mean(axis=0))[2][-1]
        if n[2] > 0:
            n = -n
        views.append((R, p, n, float(-(n @ q.mean(axis=0)))))
    return plane_fit_mount(views)


def solve_mount(raw, log=print):
    """raw: [(R_flange, p_flange, table points in the camera frame)]. Drops views that disagree, fits the focal
    length's scale (the driver's field of view for a mode is approximate) and the mount. -> (Mount, quality, scale)."""
    raw = list(raw)
    best = None
    while True:
        best = None
        for scale in np.arange(0.70, 1.36, 0.01):
            m, q = _fit_views(raw, scale)
            cost = q["angle_err_deg"] + q["height_err_mm"] / 3.0
            if best is None or cost < best[0]:
                best = (cost, m, q, float(scale))
        _, m, q, scale = best
        if len(raw) <= 6:
            break
        # each view's own disagreement with the fit; drop the worst if it stands out
        errs = []
        for R, p, pts in raw:
            qq = pts.copy()
            qq[:, :2] /= scale
            n = np.linalg.svd(qq - qq.mean(axis=0))[2][-1]
            n = -n if n[2] > 0 else n
            a = np.asarray(R).T @ np.array([0.0, 0.0, 1.0])
            ang = math.degrees(math.acos(max(-1.0, min(1.0, float((m.R @ n) @ a)))))
            h = float(-(n @ qq.mean(axis=0)))
            errs.append(ang + abs(a @ m.t + p[2] - h) / 3.0)
        worst = int(np.argmax(errs))
        if errs[worst] > max(3.0 * float(np.median(errs)), 4.0):
            log(f"  dropped a view that disagrees ({errs[worst]:.1f})")
            raw.pop(worst)
            continue
        break
    return m, q, scale


def calibrate_on_table(arm, depth, log=print, save=True):
    """Calibrate the mount from views of the bare table (no object needed). The arm moves over free table."""
    raw = []
    for x, y, z, lean, twist in table_poses():
        p, R = _pose_with_twist(x, y, z, lean, twist)
        q = A.KIN.nearest_ik(A.K.rt_to_pose(p, R), arm.state()["joints"])
        if q is None:
            continue
        try:
            arm.move_joints([float(v) for v in q], max_deg_s=25.0)
        except A.ArmError as e:
            log(f"  ({x:.0f}, {y:.0f}, {z:.0f}) lean {lean:.0f} twist {twist:.0f}: {e}")
            continue
        time.sleep(0.8)
        pose = arm.raw_state()["pose_current"]
        pf, Rf = A.K.pose_to_rt(pose)
        pts = cloud(depth.settled(5))
        fit = fit_plane(pts)
        if fit is None:
            log(f"  ({x:.0f}, {y:.0f}, {z:.0f}) lean {lean:.0f} twist {twist:.0f}: no table plane in view")
            continue
        n, d, k = fit
        inl = pts[np.abs(pts @ n + d) < 6.0]
        inl = inl[np.random.default_rng(0).choice(len(inl), min(1500, len(inl)), replace=False)]
        raw.append((np.asarray(Rf), np.asarray(pf, float), inl))
        log(f"  ({x:.0f}, {y:.0f}, {z:.0f}) lean {lean:.0f} twist {twist:.0f}: table {d:.0f} mm away, {k} points")
    if len(raw) < 6:
        raise DepthError(f"only {len(raw)} views of the table; at least 6 are needed")
    HOME.mkdir(parents=True, exist_ok=True)
    np.savez_compressed(HOME / "depth_views.npz", R=np.array([r[0] for r in raw]), p=np.array([r[1] for r in raw]),
                        **{f"pts{i}": r[2] for i, r in enumerate(raw)}, hfov=HFOV, vfov=VFOV)
    m, q, scale = solve_mount(raw, log)
    log(f"mount: camera at {np.round(m.t).tolist()} mm in the flange frame; up agrees to {q['angle_err_deg']} deg, "
        f"heights to {q['height_err_mm']} mm (focal scale {scale:.2f}, driver FOV {HFOV:.1f}x{VFOV:.1f}, "
        f"conditioning {q['conditioning']})")
    if q["conditioning"] < 0.05:
        raise DepthError("the views did not vary enough to fix the camera's position")
    m.focal_scale = scale
    if save:
        m.save()
    return m


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
    sub.add_parser("table", help="calibrate from views of the bare table (no object needed); the arm moves")
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
        elif a.cmd == "table":
            calibrate_on_table(arm, dep)
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
