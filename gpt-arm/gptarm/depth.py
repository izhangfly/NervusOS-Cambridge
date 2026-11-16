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


def project(points_base, flange_pose, mount, shape=(H, W)):
    """Base-frame points (mm) -> pixels in the depth image (None for points behind the camera)."""
    p, R = A.K.pose_to_rt(flange_pose)
    fx, fy, cx, cy = intrinsics(shape[1], shape[0])
    fx, fy = fx * mount.focal_scale, fy * mount.focal_scale
    out = []
    for P in np.atleast_2d(np.asarray(points_base, float)):
        q = mount.R.T @ (np.asarray(R).T @ (P - np.asarray(p, float)) - mount.t)
        out.append(None if q[2] <= 1.0 else (float(fx * q[0] / q[2] + cx), float(fy * q[1] / q[2] + cy)))
    return out


def model_view(depth, flange_pose, mount, scale=3):
    """The depth picture the model sees: colours for distance, the exact spot between the fingertips (white
    ring), the two open fingertips (white dots), and the point on the surface straight below the tool point
    (white cross), with its height."""
    import cv2
    img = colourise(depth)
    # everything below the table top (the floor past the table's edge) in flat grey: only the table and what is
    # on it keep their colours
    fx, fy, cx, cy = intrinsics(depth.shape[1], depth.shape[0])
    fx, fy = fx * mount.focal_scale, fy * mount.focal_scale
    vs, us = np.mgrid[0:depth.shape[0], 0:depth.shape[1]]
    z = depth.astype(float)
    cam = np.stack([(us - cx) * z / fx, (vs - cy) * z / fy, z], axis=-1).reshape(-1, 3)
    base_z = mount.to_base(cam, flange_pose)[:, 2].reshape(depth.shape)
    floor = (z > 0) & (base_z < -25.0)
    img[floor] = (70, 70, 70)
    if floor.any():
        cv2.putText(img, "grey: below the table", (10, 44), cv2.FONT_HERSHEY_SIMPLEX, 0.45, (200, 200, 200), 1, cv2.LINE_AA)
    img = cv2.resize(img, (img.shape[1] * scale, img.shape[0] * scale), interpolation=cv2.INTER_NEAREST)
    p, R = A.K.pose_to_rt(flange_pose)
    tool = np.asarray(p, float) + np.asarray(R) @ np.array([0.0, 0.0, A.GRASP_OFFSET_MM])
    tips = A.tip_points(flange_pose)
    s = summary(depth, flange_pose, mount)
    below = np.array([tool[0], tool[1], float(s.get("surface_under_tool_z_mm") or 0.0)])
    for P, kind in [(tool, "tool")] + [(t, "tip") for t in tips[1:]] + [(below, "below")]:
        uv = project(P, flange_pose, mount, depth.shape)[0]
        if uv is None:
            continue
        u, v = int(uv[0] * scale), int(uv[1] * scale)
        if kind == "tool":
            cv2.circle(img, (u, v), 14, (255, 255, 255), 2, cv2.LINE_AA)
            cv2.putText(img, "between fingertips", (u + 18, v - 8), cv2.FONT_HERSHEY_SIMPLEX, 0.55, (255, 255, 255), 2, cv2.LINE_AA)
        elif kind == "tip":
            cv2.circle(img, (u, v), 6, (255, 255, 255), -1, cv2.LINE_AA)
        else:
            cv2.drawMarker(img, (u, v), (255, 255, 255), cv2.MARKER_CROSS, 22, 2, cv2.LINE_AA)
            cv2.putText(img, f"surface z={below[2]:.0f} mm", (u + 14, v + 22), cv2.FONT_HERSHEY_SIMPLEX, 0.55,
                        (255, 255, 255), 2, cv2.LINE_AA)
    return img


def base_points(depth, flange_pose, mount, step=None):
    """The depth image as base-frame points (mm), without the gripper's own fingers and body."""
    pts = cloud(depth, step=step)
    pts[:, :2] /= getattr(mount, "focal_scale", 1.0)
    pts = mount.to_base(pts, flange_pose)
    p, R = A.K.pose_to_rt(flange_pose)
    q = (pts - np.asarray(p, float)) @ np.asarray(R)                # flange frame
    grip = ((q[:, 2] > -20.0) & (q[:, 2] < A.GRASP_OFFSET_MM + A.TIP_OFFSET_MM + 8.0)
            & (np.abs(q[:, 0]) < 40.0) & (np.abs(q[:, 1]) < 60.0))
    return pts[~grip]


MAP_HALF_MM, MAP_CELL_MM = 160.0, 2.0


class Map:
    """A height map of the table seen from straight above: H[row, col] = the top height (mm) of what the
    depth camera saw in that cell, NaN where it saw nothing. Row 0 is the far side (+x), column 0 the left
    (+y), as on the overhead picture; cell (0, 0)'s outer corner is at (x_top, y_left). `fresh` marks the cells
    seen in the newest frame (the others are remembered from earlier frames)."""

    def __init__(self, H, x_top, y_left, fresh=None, cell=MAP_CELL_MM):
        self.H, self.x_top, self.y_left, self.cell = H, float(x_top), float(y_left), cell
        self.fresh = np.ones(H.shape, bool) if fresh is None else fresh

    def xy(self, rows, cols):
        return (self.x_top - (np.asarray(rows) + 0.5) * self.cell, self.y_left - (np.asarray(cols) + 0.5) * self.cell)

    def rc(self, x, y):
        return (self.x_top - x) / self.cell, (self.y_left - y) / self.cell


def _bin(pts, x_top, y_left, shape, cell=MAP_CELL_MM):
    """Per-cell maximum height of the points (-1e4 where none)."""
    r = np.floor((x_top - pts[:, 0]) / cell).astype(int)
    c = np.floor((y_left - pts[:, 1]) / cell).astype(int)
    ok = (r >= 0) & (r < shape[0]) & (c >= 0) & (c < shape[1])
    H = np.full(shape, -1e4, np.float32)
    np.maximum.at(H, (r[ok], c[ok]), pts[ok, 2].astype(np.float32))
    return H


def _fill(H):
    """NaN cells next to seen ones take their neighbours' height (one cell: the camera's sampling gaps)."""
    import cv2
    raw = np.nan_to_num(H, nan=-1e4).astype(np.float32)
    grown = cv2.dilate(raw, np.ones((3, 3), np.uint8))
    out = np.where(np.isnan(H), grown, raw)
    out[out < -1e3] = np.nan
    return out


def height_map(pts, centre_xy, half_mm=MAP_HALF_MM, cell_mm=MAP_CELL_MM):
    """The Map of one frame's points around centre_xy (2 * half_mm square)."""
    n = int(round(2 * half_mm / cell_mm))
    H = _bin(pts, centre_xy[0] + half_mm, centre_xy[1] + half_mm, (n, n), cell_mm)
    H[H < -1e3] = np.nan
    return Map(_fill(H), centre_xy[0] + half_mm, centre_xy[1] + half_mm, cell=cell_mm)


class DepthMemory:
    """What the depth camera has seen during a run, kept in the arm's x/y: each new frame overwrites the cells
    it sees, the rest keep their last height. Forget it when something on the table may have moved (the
    gripper opened or closed)."""
    X_TOP, Y_LEFT, SHAPE = 700.0, 550.0, (400, 550)            # x 700..-100, y 550..-550, 2 mm cells

    def __init__(self):
        self.H = np.full(self.SHAPE, np.nan, np.float32)
        self.fresh = np.zeros(self.SHAPE, bool)

    def add(self, pts):
        cur = _bin(pts, self.X_TOP, self.Y_LEFT, self.SHAPE)
        cur[cur < -1e3] = np.nan
        cur = _fill(cur)                  # the frame's own sampling gaps count as seen, or old heights survive in them
        seen = ~np.isnan(cur)
        self.H[seen] = cur[seen]
        self.fresh = seen

    def mark_stale(self):
        """The newest frame saw nothing: everything in the memory is remembered."""
        self.fresh = np.zeros(self.SHAPE, bool)

    def crop(self, centre_xy, half_mm=MAP_HALF_MM):
        n = int(round(2 * half_mm / MAP_CELL_MM))
        r0 = int(round((self.X_TOP - (centre_xy[0] + half_mm)) / MAP_CELL_MM))
        c0 = int(round((self.Y_LEFT - (centre_xy[1] + half_mm)) / MAP_CELL_MM))
        H = np.full((n, n), np.nan, np.float32)
        F = np.zeros((n, n), bool)
        rs, cs = max(0, r0), max(0, c0)
        re, ce = min(self.SHAPE[0], r0 + n), min(self.SHAPE[1], c0 + n)
        if rs < re and cs < ce:
            H[rs - r0:re - r0, cs - c0:ce - c0] = self.H[rs:re, cs:ce]
            F[rs - r0:re - r0, cs - c0:ce - c0] = self.fresh[rs:re, cs:ce]
        return Map(_fill(H), self.X_TOP - r0 * MAP_CELL_MM, self.Y_LEFT - c0 * MAP_CELL_MM, F)


def map_objects(m, tool_xy, rise_mm=18.0, min_area_mm2=300.0, limit=5):
    """Things that rise above the floor around them in a Map: [{centre, top, length, width, long axis,
    offset from the tool point}], nearest first. The floor is the map's low tenth (the table, or a basket's
    floor)."""
    import cv2
    H = m.H
    valid = ~np.isnan(H)
    if valid.sum() < 100:
        return []
    floor = float(np.nanpercentile(H, 10))
    mask = (valid & (np.nan_to_num(H, nan=-1e4) > floor + rise_mm)).astype(np.uint8)
    mask = cv2.morphologyEx(mask, cv2.MORPH_CLOSE, np.ones((3, 3), np.uint8))
    n, lab, st, _ = cv2.connectedComponentsWithStats(mask)
    out = []
    for k in range(1, n):
        if st[k, cv2.CC_STAT_AREA] * m.cell ** 2 < min_area_mm2:
            continue
        rr, cc = np.nonzero(lab == k)
        xs, ys = m.xy(rr, cc)
        zs = H[rr, cc]
        zs = zs[~np.isnan(zs)]
        P = np.column_stack([xs, ys])
        c = P.mean(axis=0)
        if len(P) >= 3:
            major = np.linalg.eigh(np.cov((P - c).T))[1][:, 1]
        else:
            major = np.array([1.0, 0.0])
        minor = np.array([-major[1], major[0]])
        length = float(np.ptp((P - c) @ major)) + m.cell
        width = float(np.ptp((P - c) @ minor)) + m.cell
        ang = math.degrees(math.atan2(major[1], major[0]))
        ang = ang - 180.0 if ang > 90.0 else (ang + 180.0 if ang <= -90.0 else ang)
        edge = bool(rr.min() == 0 or cc.min() == 0 or rr.max() == H.shape[0] - 1 or cc.max() == H.shape[1] - 1)
        out.append({"centre_xy_mm": [round(float(c[0])), round(float(c[1]))],
                    "top_z_mm": round(float(np.percentile(zs, 95))), "floor_around_z_mm": round(floor),
                    "length_mm": round(length), "width_mm": round(width), "long_axis_deg": round(ang),
                    "offset_from_tool_xy_mm": [round(float(c[0] - tool_xy[0])), round(float(c[1] - tool_xy[1]))],
                    "cut_off_by_view": edge, "remembered": bool(m.fresh[rr, cc].mean() < 0.5)})
    out.sort(key=lambda o: math.hypot(*o["offset_from_tool_xy_mm"]))
    for i, o in enumerate(out[:limit], 1):
        o["label"] = f"D{i}"
    return out[:limit]


def tool_point(flange_pose):
    p, R = A.K.pose_to_rt(flange_pose)
    return np.asarray(p, float) + np.asarray(R) @ np.array([0.0, 0.0, A.GRASP_OFFSET_MM])


NO_READING = "almost no depth reading: the camera is closer than about 15 cm to what it faces, or it faces black fabric"


def summary(depth, flange_pose, mount, table_z=0.0, memory=None):
    """What the model is told: the surface under the tool point, and the things that stand up from the floor
    around it (from the top-down height map the model also sees). With a DepthMemory the frame is added to it
    and the map includes what earlier frames saw."""
    pts = base_points(depth, flange_pose, mount)
    tool = tool_point(flange_pose)
    tip_z = min(float(t[2]) for t in A.tip_points(flange_pose))
    seen = len(pts) >= 200
    if memory is not None:
        memory.add(pts) if seen else memory.mark_stale()
    out = {"seen": seen} if seen else {"seen": False, "why": NO_READING}
    if not seen and memory is None:
        return out
    m = memory.crop(tool[:2]) if memory is not None else height_map(pts, tool[:2])
    below = pts[np.hypot(pts[:, 0] - tool[0], pts[:, 1] - tool[1]) < 12.0] if seen else pts[:0]
    if len(below) >= 5:
        top = float(np.percentile(below[:, 2], 90))
        out["surface_under_tool_z_mm"] = round(top)
        out["fingertips_above_it_mm"] = round(tip_z - top)
    else:
        r, c = m.rc(tool[0], tool[1])
        r0, r1, c0, c1 = int(r) - 6, int(r) + 7, int(c) - 6, int(c) + 7
        near = m.H[max(0, r0):max(0, r1), max(0, c0):max(0, c1)]
        if near.size and not np.isnan(near).all():
            top = float(np.nanpercentile(near, 90))
            out["surface_under_tool_z_mm"] = round(top)       # from an earlier view
            out["fingertips_above_it_mm"] = round(tip_z - top)
            out["surface_under_tool_remembered"] = True
        else:
            out["surface_under_tool_z_mm"] = None           # not seen from here, nor earlier
    out["objects_below"] = map_objects(m, tool[:2])
    return out


def top_view(depth, flange_pose, mount, scale=4, objects=None, memory=None):
    """The picture the model gets as image 3: the depth camera's view turned into a map seen from straight
    above, centred on the tool point, +x up and +y left, coloured by height above the table, with a 50 mm grid
    in the arm's coordinates, the tool point (white ring), the open fingertips (white dots) and the line the
    fingers close along, and the objects of `objects_below` labelled D1, D2, ... With a DepthMemory (the frame
    already added by `summary`), what earlier frames saw is drawn dimmer."""
    import cv2
    tool = tool_point(flange_pose)
    m = memory.crop(tool[:2]) if memory is not None else height_map(base_points(depth, flange_pose, mount), tool[:2])
    H = m.H
    n = H.shape[0]
    v = np.clip(np.nan_to_num(H, nan=0.0) / 160.0, 0.0, 1.0)
    img = cv2.applyColorMap((255 * v).astype(np.uint8), cv2.COLORMAP_TURBO)
    old = ~m.fresh & ~np.isnan(H)
    img[old] = (img[old] * 0.45).astype(np.uint8)
    img[np.isnan(H)] = (0, 0, 0)
    img = cv2.resize(img, (n * scale, n * scale), interpolation=cv2.INTER_NEAREST)
    k = scale / m.cell                                               # pixels per mm

    def px(x, y):
        return (int(round((m.y_left - y) * k)), int(round((m.x_top - x) * k)))

    font = cv2.FONT_HERSHEY_SIMPLEX
    x_lo, y_lo = m.x_top - n * m.cell, m.y_left - n * m.cell
    for x in range(int(math.ceil(x_lo / 50.0)) * 50, int(m.x_top) + 1, 50):
        _, row = px(x, 0)
        cv2.line(img, (0, row), (img.shape[1], row), (210, 210, 210), 1, cv2.LINE_AA)
        cv2.putText(img, f"x={x}", (4, row - 4), font, 0.45, (255, 255, 255), 1, cv2.LINE_AA)
    for y in range(int(math.ceil(y_lo / 50.0)) * 50, int(m.y_left) + 1, 50):
        col, _ = px(0, y)
        cv2.line(img, (col, 0), (col, img.shape[0]), (210, 210, 210), 1, cv2.LINE_AA)
        cv2.putText(img, f"y={y}", (col + 3, 14), font, 0.45, (255, 255, 255), 1, cv2.LINE_AA)
    for o in objects or []:
        c = px(*o["centre_xy_mm"])
        cv2.putText(img, f"{o['label']} top {o['top_z_mm']}", (c[0] - 30, c[1] + 5), font, 0.55, (0, 0, 0), 3, cv2.LINE_AA)
        cv2.putText(img, f"{o['label']} top {o['top_z_mm']}", (c[0] - 30, c[1] + 5), font, 0.55, (255, 255, 255), 1, cv2.LINE_AA)
    tips = [px(t[0], t[1]) for t in A.tip_points(flange_pose)[1:]]
    cv2.line(img, tips[0], tips[1], (255, 255, 255), 1, cv2.LINE_AA)
    for t in tips:
        cv2.circle(img, t, 6, (255, 255, 255), -1, cv2.LINE_AA)
        cv2.circle(img, t, 6, (0, 0, 0), 1, cv2.LINE_AA)
    c = px(tool[0], tool[1])
    cv2.circle(img, c, 10, (255, 255, 255), 2, cv2.LINE_AA)
    cv2.circle(img, c, 12, (0, 0, 0), 1, cv2.LINE_AA)
    # the legend: height above the table
    h, w = img.shape[:2]
    bar = cv2.applyColorMap(np.linspace(0, 255, w - 80).astype(np.uint8)[None, :].repeat(10, axis=0), cv2.COLORMAP_TURBO)
    pad = np.zeros((70, w, 3), np.uint8)
    pad[8:18, 40:w - 40] = bar
    for mm in range(0, 161, 40):
        xx = int(40 + mm / 160.0 * (w - 81))
        cv2.putText(pad, f"{mm}", (xx - 10, 34), font, 0.45, (255, 255, 255), 1, cv2.LINE_AA)
    cv2.putText(pad, f"height above the table (mm); seen from above, +x up, +y left; tool z={tool[2]:.0f}", (8, 50),
                font, 0.42, (220, 220, 220), 1, cv2.LINE_AA)
    if old.any():
        cv2.putText(pad, "dim = remembered from earlier views (not in the camera's view now)", (8, 66), font, 0.42,
                    (220, 220, 220), 1, cv2.LINE_AA)
    return np.vstack([img, pad])


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


def wall_poses():
    """Where the arm looks at the backboard from: well back from it, the tool leaning outward so the wrist
    camera faces the far edge of the table. (x, y, z, lean) with yaw 0."""
    return [(300.0, y, 330.0, 55.0) for y in (-150.0, 0.0, 150.0)]


def fit_wall(pts, iters=500, tol_mm=10.0, max_tilt_deg=20.0, rng=None):
    """The dominant near-vertical plane in base-frame points: (unit normal, horizontal and pointing +x,
    offset mm, inlier mask), or None."""
    pts = np.asarray(pts, float)
    if len(pts) < 100:
        return None
    rng = rng or np.random.default_rng(0)
    best = None
    sin_max = math.sin(math.radians(max_tilt_deg))
    for _ in range(iters):
        a, b, c = pts[rng.choice(len(pts), 3, replace=False)]
        n = np.cross(b - a, c - a)
        if np.linalg.norm(n) < 1e-6:
            continue
        n /= np.linalg.norm(n)
        if abs(n[2]) > sin_max:                            # a wall is vertical
            continue
        inl = np.abs((pts - a) @ n) < tol_mm
        if best is None or inl.sum() > best.sum():
            best = inl
    if best is None or best.sum() < 0.3 * len(pts):
        return None
    q = pts[best]
    c = q.mean(axis=0)
    n = np.linalg.svd(q - c)[2][-1]
    n = np.array([n[0], n[1], 0.0])                        # vertical by definition
    n /= np.linalg.norm(n)
    if n[0] < 0:
        n = -n                                             # from the table towards the wall
    return n, float(n @ c), best


def measure_wall(arm, depth, log=print, save=True, x_min=430.0, z_min=70.0, y_max=350.0):
    """Look at the backboard from a few poses, fit its plane, and save it to workspace.json as the arm's wall.
    The saved plane is pushed in to the nearest 2% of the backboard's points, so a curve at its foot counts."""
    mount = Mount.load()
    if mount is None:
        raise SystemExit("the depth camera's mount is not calibrated: run `python -m gptarm.depth table` first")
    found = []
    for x, y, z, lean in wall_poses():
        arm.move_tool(x, y, z, lean=lean, yaw_deg=0.0)
        time.sleep(0.8)
        d = depth.settled(7)
        pose = arm.raw_state()["pose_current"]
        pts = cloud(d)
        pts[:, :2] /= mount.focal_scale
        pts = mount.to_base(pts, pose)
        keep = (pts[:, 0] > x_min) & (pts[:, 2] > z_min) & (np.abs(pts[:, 1]) < y_max)
        log(f"view from ({x:.0f}, {y:.0f}, {z:.0f}): {len(pts)} points, {int(keep.sum())} past x={x_min:.0f} and above z={z_min:.0f}")
        found.append(pts[keep])
    P = np.vstack(found) if found else np.zeros((0, 3))
    fit = fit_wall(P)
    if fit is None:
        raise SystemExit(f"no wall found in {len(P)} points: is the backboard in view?")
    n, d_fit, inl = fit
    s = P @ n
    near = np.abs(s - d_fit) < 80.0
    offset = float(np.percentile(s[near], 2))
    rms = float(np.sqrt(np.mean((s[inl] - d_fit) ** 2)))
    wall = {"name": "backboard", "normal": [round(float(v), 4) for v in n], "offset_mm": round(offset, 1),
            "fit_offset_mm": round(d_fit, 1), "plane_rms_mm": round(rms, 1), "points": int(len(P)),
            "z_range_mm": [round(float(P[:, 2].min())), round(float(P[:, 2].max()))], "measured": time.time()}
    for y in (-300.0, 0.0, 300.0):
        log(f"  at y={y:+.0f} the backboard is at x={(offset - n[1] * y) / n[0]:.0f} mm")
    log(f"backboard: normal ({n[0]:.3f}, {n[1]:.3f}), plane fit {d_fit:.0f} mm (rms {rms:.1f}), "
        f"nearest part {offset:.0f} mm, from {len(P)} points")
    if save:
        A.WORKSPACE_PATH.parent.mkdir(parents=True, exist_ok=True)
        A.WORKSPACE_PATH.write_text(json.dumps({"walls": [wall]}, indent=1))
        np.save(A.WORKSPACE_PATH.with_name("wall_points.npy"), P.astype(np.float32))
        log(f"saved to {A.WORKSPACE_PATH}")
    return wall


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
    sub.add_parser("wall", help="measure the backboard behind the table and save it as the arm's wall; the arm moves")
    a = ap.parse_args()
    arm, dep = A.Arm(speed=20, floor_z_mm=60.0), WristDepth()
    try:
        if a.cmd == "calibrate":
            calibrate(arm, dep, a.x, a.y, a.top_z)
        elif a.cmd == "table":
            calibrate_on_table(arm, dep)
        elif a.cmd == "wall":
            measure_wall(arm, dep)
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
