"""Where the overhead camera is: a projection from the arm's base frame (mm) to its pixels.

There is no checkerboard and nothing is measured by hand. The arm is its own target: at each calibration
pose it opens, closes and opens the gripper, and the pixels that go dark and come back are the closed
fingers: a point on the tool axis a fixed distance past the flange, which the driver knows in millimetres.
Twenty or so such pairs give a pinhole camera (focal length, position, direction, one radial term); the
distance along the tool axis is the one unknown of the marker and is found by a one-dimensional search.

The poses go from high to low. The first ones are above anything that can stand on the table; each fit then
tells where the next, lower ones would appear in the image, and a pose is skipped when something dark and
unmoving (the basket, the bottle's cap) is near that place. Nothing on the table has to be cleared away.

The camera is on a stand and gets nudged: `python -m gptarm.calib` redoes all of this in a few minutes.
"""
from __future__ import annotations

import json
import math
import os
import time
from pathlib import Path

import cv2
import numpy as np

from . import arm as A

HOME = Path(os.environ.get("GPTARM_HOME", "~/.config/gptarm")).expanduser()
CAL_PATH = HOME / "calibration.json"

DARKER = 0.5             # the fingers are under half the table's brightness; their shadow is not
MARKER_MM = 105.0        # first guess: flange to the middle of the fingers' visible length
MARKER_RANGE = (80.0, 130.0)


class CalibrationError(Exception):
    pass


# ---- the camera model --------------------------------------------------------------------------------------
class Camera:
    """A pinhole camera: one focal length, the principal point at the image centre, one radial term."""

    def __init__(self, K, dist, rvec, tvec, size, marker_mm=MARKER_MM, points=None, residual_px=None, when=None):
        self.K = np.asarray(K, float).reshape(3, 3)
        self.dist = np.asarray(dist, float).ravel()
        self.rvec = np.asarray(rvec, float).reshape(3, 1)
        self.tvec = np.asarray(tvec, float).reshape(3, 1)
        self.R = cv2.Rodrigues(self.rvec)[0]
        self.size = tuple(size)                    # (width, height)
        self.marker_mm = float(marker_mm)
        self.points = points or []
        self.residual_px = residual_px
        self.when = when or time.time()

    def project(self, xyz):
        """Base-frame mm -> pixels. One point (3,) -> (2,), or (N, 3) -> (N, 2)."""
        X = np.atleast_2d(np.asarray(xyz, float)).reshape(-1, 1, 3)
        uv = cv2.projectPoints(X, self.rvec, self.tvec, self.K, self.dist)[0].reshape(-1, 2)
        return uv[0] if np.ndim(xyz) == 1 else uv

    def ground(self, u, v, z=0.0):
        """The point at height z (mm) that appears at pixel (u, v): (x, y)."""
        n = cv2.undistortPoints(np.array([[[float(u), float(v)]]]), self.K, self.dist).reshape(2)
        d = self.R.T @ np.array([n[0], n[1], 1.0])
        c = self.centre()
        k = (z - c[2]) / d[2]
        return float(c[0] + k * d[0]), float(c[1] + k * d[1])

    def centre(self):
        """The camera's own position (mm)."""
        return (-self.R.T @ self.tvec).ravel()

    def mm_per_px(self, x, y, z=0.0):
        u, v = self.project([x, y, z])
        x2, y2 = self.ground(u + 1.0, v, z)
        return math.hypot(x2 - x, y2 - y)

    def to_dict(self):
        return {"K": self.K.tolist(), "dist": self.dist.tolist(), "rvec": self.rvec.ravel().tolist(),
                "tvec": self.tvec.ravel().tolist(), "size": list(self.size), "marker_mm": self.marker_mm,
                "points": self.points, "residual_px": self.residual_px, "when": self.when,
                "camera_mm": [round(float(v), 1) for v in self.centre()], "focal_px": round(float(self.K[0, 0]), 1)}

    def save(self, path=None):
        path = Path(path or CAL_PATH)
        path.parent.mkdir(parents=True, exist_ok=True)
        tmp = path.with_suffix(".tmp")
        tmp.write_text(json.dumps(self.to_dict(), indent=1))
        tmp.replace(path)

    @classmethod
    def load(cls, path=None):
        try:
            d = json.loads(Path(path or CAL_PATH).read_text())
            return cls(d["K"], d["dist"], d["rvec"], d["tvec"], d["size"], d.get("marker_mm", MARKER_MM),
                       d.get("points"), d.get("residual_px"), d.get("when"))
        except (OSError, ValueError, KeyError):
            return None


def fit_pinhole(world, pix, size, radial=False):
    """The camera through N >= 6 points: (K, dist, rvec, tvec, each point's error in pixels).

    A free 3x4 projection (the direct linear transform) fits the points it is given and nothing else: from
    points in a slab 5 cm thick, 25 cm above the table, it put the table a third too large. A real camera
    has far fewer freedoms. With square pixels and the principal point at the centre there are seven (one
    focal length, where it is, which way it looks), and the table comes out where it is."""
    W = np.asarray(world, np.float32).reshape(-1, 3)
    I = np.asarray(pix, np.float32).reshape(-1, 2)
    if len(W) < 6:
        raise CalibrationError("a fit needs at least 6 points")
    w, h = size
    K0 = np.array([[0.78 * w, 0.0, w / 2.0], [0.0, 0.78 * w, h / 2.0], [0.0, 0.0, 1.0]])
    flags = (cv2.CALIB_USE_INTRINSIC_GUESS | cv2.CALIB_FIX_PRINCIPAL_POINT | cv2.CALIB_FIX_ASPECT_RATIO
             | cv2.CALIB_ZERO_TANGENT_DIST | cv2.CALIB_FIX_K2 | cv2.CALIB_FIX_K3)
    if not radial:
        flags |= cv2.CALIB_FIX_K1
    try:
        _, K, dist, rv, tv = cv2.calibrateCamera([W], [I], (int(w), int(h)), K0, None, flags=flags)
    except cv2.error as e:
        raise CalibrationError(f"the camera fit failed: {str(e).splitlines()[-1][:120]}") from None
    proj = cv2.projectPoints(W.reshape(-1, 1, 3).astype(np.float64), rv[0], tv[0], K, dist)[0].reshape(-1, 2)
    return K, dist.ravel()[:5], rv[0], tv[0], np.linalg.norm(proj - I, axis=1)


def marker_point(flange_pose, marker_mm):
    p, R = A.K.pose_to_rt(flange_pose)
    return np.asarray(p, float) + R @ np.array([0.0, 0.0, marker_mm])


def fit(points, size, search=True):
    """points: [{"flange": [6], "px": [u, v]}, ...] -> Camera. Drops gross outliers once. With enough points
    it also finds the marker's distance along the tool axis and one radial distortion term."""
    pts = list(points)

    def solve(pp, L):
        return fit_pinhole([marker_point(q["flange"], L) for q in pp], [q["px"] for q in pp], size, radial=len(pp) >= 18)

    def best(pp):
        if not search or len(pp) < 14:
            return (MARKER_MM, *solve(pp, MARKER_MM))
        out = None
        for L in np.arange(MARKER_RANGE[0], MARKER_RANGE[1] + 0.1, 2.5):
            r = solve(pp, L)
            if out is None or np.sqrt((r[4] ** 2).mean()) < np.sqrt((out[5] ** 2).mean()):
                out = (float(L), *r)
        return out

    L, K, dist, rv, tv, err = best(pts)
    keep = err <= max(3.0 * np.median(err), 8.0)
    if len(pts) >= 10 and not keep.all() and keep.sum() >= 8:
        pts = [q for q, k in zip(pts, keep) if k]
        L, K, dist, rv, tv, err = best(pts)
    for q, e in zip(pts, err):
        q["err_px"] = round(float(e), 1)
    return Camera(K, dist, rv, tv, size, L, pts, round(float(np.sqrt((err ** 2).mean())), 2))


# ---- finding the fingers -----------------------------------------------------------------------------------
def gripper_marker(open_img, closed_img, open_again, near=None, radius=260.0):
    """Where the closed fingers are, from three photographs (gripper open, closed, open again): (u, v), pixels.
    None if they are not seen.

    The closed finger pair sits on the tool axis, and it is the one thing that is dark in the closed
    photograph and table in both open ones. The open fingers are not used: from above, the wrist camera's
    body hides one of them at many poses, and a centre taken from the other alone is 2 cm off to its side.
    Everything is relative to the table's own brightness there, so the test holds in a dim room as in a
    bright one, and a pixel that differs between the two open photographs (a person moving at the table's
    edge, a screen lighting up) is not counted. `near`: where the gripper is expected, once a fit exists.
    """
    ga, gb, gc = (cv2.GaussianBlur(cv2.cvtColor(i, cv2.COLOR_BGR2GRAY), (5, 5), 0).astype(np.float32)
                  for i in (open_img, closed_img, open_again))
    ref = np.minimum(ga, gc)
    mask = ((ref - gb) / (ref + 8.0) > DARKER) & (ref - gb > 14.0) & (np.abs(ga - gc) / (np.maximum(ga, gc) + 8.0) < 0.2)
    mask = cv2.morphologyEx(mask.astype(np.uint8), cv2.MORPH_OPEN, np.ones((3, 3), np.uint8))
    n, lab, stats, cent = cv2.connectedComponentsWithStats(mask)
    comps = [(int(stats[i, cv2.CC_STAT_AREA]), cent[i], i) for i in range(1, n) if stats[i, cv2.CC_STAT_AREA] >= 60]
    if near is not None:
        comps = [c for c in comps if np.linalg.norm(c[1] - np.asarray(near)) < radius]
    if not comps:
        return None
    anchor = max(comps, key=lambda c: c[0])[1]
    kept = [c for c in comps if np.linalg.norm(c[1] - anchor) < 70.0]
    ys, xs = np.nonzero(np.isin(lab, [c[2] for c in kept]))
    if not 150 <= len(xs) <= 20000 or max(np.ptp(xs), np.ptp(ys)) > 220:
        return None
    return (float(xs.mean()), float(ys.mean())), int(len(xs))


TABLE_X, TABLE_Y = (20.0, 400.0), (-320.0, 320.0)      # the table top in front of the arm (mm), inside its edges


def table_mask(cam, shape, x=TABLE_X, y=TABLE_Y):
    """The table top's pixels, from the fit."""
    corners = cam.project(np.array([[x[0], y[0], 0.0], [x[1], y[0], 0.0], [x[1], y[1], 0.0], [x[0], y[1], 0.0]]))
    mask = np.zeros(shape[:2], np.uint8)
    cv2.fillPoly(mask, [np.round(corners).astype(np.int32)], 255)
    return mask


def static_dark(frames, cam, level=0.42):
    """Things standing on the table: pixels of the table top that are well under the table's own brightness
    in every one of the frames (the basket, the bottle's cap). The arm was somewhere else in each frame, so
    it and its shadow drop out. Off the table nothing is marked: the gripper is never sent there."""
    table = table_mask(cam, frames[0].shape) > 0
    out = None
    for f in frames:
        g = cv2.cvtColor(f, cv2.COLOR_BGR2GRAY)
        dark = table & (g < level * np.median(g[table]))
        out = dark if out is None else (out & dark)
    return out


def gripper_clear(cam, dark, x, y, z, lean, margin_px):
    """Would the gripper at this pose stay clear of every static dark thing in the image? Checked on the
    way down too (the descent is vertical). The fit is rough at this point, hence the generous margin."""
    h, w = dark.shape
    for dz in (0.0, 45.0):
        pose = A.flange_pose(x, y, z + dz, lean)
        pts = A.tip_points(pose) + [np.array([x, y, z + dz]), np.asarray(pose[:3], float)]
        for u, v in cam.project(np.asarray(pts)):
            if not (0 <= u < w and 0 <= v < h):
                return False
            m = int(margin_px)
            if dark[max(0, int(v) - m):int(v) + m, max(0, int(u) - m):int(u) + m].any():
                return False
    return True


# ---- the routine -------------------------------------------------------------------------------------------
LEANS = (40.0, 45.0, 50.0, 55.0)
CARRY_Z = 250.0
STAGES = [   # name, tool heights, x values, y values, clearance margin in pixels (None: above everything)
    ("high", (250.0, 300.0), (170.0, 250.0, 330.0), (-40.0, 100.0, 240.0), None),
    ("middle", (185.0,), (200.0, 270.0, 340.0), (-100.0, 0.0, 100.0, 200.0, 300.0), 80),
    ("low", (115.0,), (190.0, 250.0, 310.0, 370.0), (-100.0, -20.0, 60.0, 140.0, 220.0, 300.0), 65),
]


def _lean_for(x, y, zs, steep=False):
    """A lean that reaches every height in zs. The points alternate between gentle and steep leans: the
    marker's distance along the tool axis only shows when the tool's direction varies."""
    for lean in (LEANS[::-1] if steep else LEANS):
        if all(A.reachable(x, y, z, lean) and A.flange_pose(x, y, z, lean)[2] <= A.CEILING_FLANGE_MM for z in zs):
            return lean
    return None


def run(arm, overhead, log=print, save=True, out_dir=None, stages=STAGES, min_points=12, resume=False):
    """Calibrate with the arm. Returns the Camera. Raises CalibrationError if it can't. resume: carry on from
    the stages a previous run finished (their points and photographs are in out_dir)."""
    points, opens, cam, size = [], [], None, None
    out_dir = Path(out_dir) if out_dir else None
    if out_dir:
        out_dir.mkdir(parents=True, exist_ok=True)
    done = set()
    if resume and out_dir and (out_dir / "points.json").exists():
        points = json.loads((out_dir / "points.json").read_text())
        opens = [cv2.imread(str(f)) for f in sorted(out_dir.glob("p*-open.jpg"))]
        done = {tuple(q["at"]) for q in points if "at" in q}
        if opens and len(points) >= 6:
            size = (opens[0].shape[1], opens[0].shape[0])
            cam = fit(points, size, search=len(points) >= 14)
            log(f"resuming with {len(points)} points, {cam.residual_px} px rms")

    def measure(x, y, z, lean, stage):
        nonlocal size
        st = arm.raw_state()
        flange = [float(v) for v in st["pose_current"]]
        arm.grip(A.GRIPPER_OPEN_MM)
        a = overhead.settled(3)
        arm.grip(0.0)
        b = overhead.settled(3)
        arm.grip(A.GRIPPER_OPEN_MM)
        c = overhead.settled(3)
        size = (a.shape[1], a.shape[0])
        opens.append(a)
        near = cam.project(marker_point(flange, cam.marker_mm)) if cam is not None else None
        hit = gripper_marker(a, b, c, near)
        if out_dir:
            cv2.imwrite(str(out_dir / f"p{len(opens):02d}-open.jpg"), a)
            cv2.imwrite(str(out_dir / f"p{len(opens):02d}-closed.jpg"), b)
        if hit is None:
            log(f"  {stage} ({x:.0f}, {y:.0f}, {z:.0f}) lean {lean:.0f}: the gripper was not seen")
            return
        (u, v), n = hit
        points.append({"flange": [round(f, 2) for f in flange], "px": [round(u, 1), round(v, 1)], "n": n, "stage": stage,
                       "at": [x, y, z]})
        log(f"  {stage} ({x:.0f}, {y:.0f}, {z:.0f}) lean {lean:.0f}: seen at ({u:.0f}, {v:.0f}), {n} px")

    arm.enable()
    arm.grip(A.GRIPPER_OPEN_MM, wait_s=0.5)
    for name, zs, xs, ys, margin in stages:
        log(f"{name} points")
        if margin is not None and cam is None:
            raise CalibrationError("too few of the high points were seen to go lower safely")
        dark = static_dark(opens, cam) if margin is not None else None
        if dark is not None and out_dir:
            vis = opens[-1].copy()
            vis[dark] = (0, 0, 255)
            cv2.polylines(vis, [np.round(cam.project(np.array([[TABLE_X[0], TABLE_Y[0], 0.0], [TABLE_X[1], TABLE_Y[0], 0.0],
                          [TABLE_X[1], TABLE_Y[1], 0.0], [TABLE_X[0], TABLE_Y[1], 0.0]]))).astype(np.int32)], True, (0, 255, 0), 2)
            cv2.imwrite(str(out_dir / f"obstacles-{name}.jpg"), vis)
        todo = []
        for i, x in enumerate(xs):
            for j, y in enumerate(ys if i % 2 == 0 else ys[::-1]):       # boustrophedon: short moves
                z = zs[(i + j) % len(zs)]
                if (x, y, z) in done:
                    continue
                lean = _lean_for(x, y, (z, CARRY_Z) if margin is not None else (z,), steep=(i + j) % 2 == 1)
                if lean is None:
                    continue
                if margin is not None and not gripper_clear(cam, dark, x, y, z, lean, margin):
                    log(f"  {name} ({x:.0f}, {y:.0f}, {z:.0f}): something is there, skipped")
                    continue
                todo.append((x, y, z, lean))
        for x, y, z, lean in todo:
            try:
                if margin is not None:           # go over at carry height, straight down, measure, straight up
                    arm.move_tool(x, y, CARRY_Z, lean=lean, min_tip_z=CARRY_Z - 40.0)
                    arm.move_tool(x, y, z, lean=lean, min_tip_z=z - 25.0)
                else:
                    arm.move_tool(x, y, z, lean=lean, min_tip_z=min(zs) - 40.0)
                arm.hold()
                time.sleep(0.4)
                measure(x, y, z, lean, name)
                if margin is not None:
                    arm.move_tool(x, y, CARRY_Z, lean=lean, min_tip_z=z - 25.0)
            except A.ArmError as e:
                log(f"  {name} ({x:.0f}, {y:.0f}, {z:.0f}): {e}")
                st = arm.state()
                if st["tool"][2] < CARRY_Z - 10:          # never travel sideways from down low
                    arm.move_tool(st["tool"][0], st["tool"][1], CARRY_Z, lean=st["lean"])
        if out_dir:
            (out_dir / "points.json").write_text(json.dumps(points))
        if len(points) >= 6:
            cam = fit(points, size, search=len(points) >= 14)
            log(f"  fit on {len(cam.points)} points: {cam.residual_px} px rms, camera at "
                f"{[round(float(v)) for v in cam.centre()]} mm, marker {cam.marker_mm:.0f} mm past the flange")
    if cam is None or len(cam.points) < min_points:
        raise CalibrationError(f"only {len(points)} calibration points were seen; at least {min_points} are needed")
    arm.hold()
    if save:
        cam.save()
    if out_dir and opens:
        cv2.imwrite(str(out_dir / "calibration.jpg"), draw(cam, opens[-1]))
    return cam


def draw(cam, img):
    """The fit drawn on a photograph: a 100 mm grid on the table plane, the axes, and each point's error."""
    out = img.copy()
    for x in range(0, 501, 100):
        a, b = cam.project([x, -400.0, 0.0]), cam.project([x, 400.0, 0.0])
        cv2.line(out, tuple(map(int, a)), tuple(map(int, b)), (0, 200, 0) if x else (0, 0, 255), 1, cv2.LINE_AA)
    for y in range(-400, 401, 100):
        a, b = cam.project([0.0, y, 0.0]), cam.project([500.0, y, 0.0])
        cv2.line(out, tuple(map(int, a)), tuple(map(int, b)), (0, 200, 0) if y else (255, 0, 0), 1, cv2.LINE_AA)
    for q in cam.points:
        u, v = q["px"]
        pu, pv = cam.project(marker_point(q["flange"], cam.marker_mm))
        cv2.circle(out, (int(u), int(v)), 6, (0, 255, 255), 1, cv2.LINE_AA)
        cv2.drawMarker(out, (int(pu), int(pv)), (255, 0, 255), cv2.MARKER_CROSS, 10, 1)
    return out


def main():
    import argparse
    from .cameras import Overhead
    ap = argparse.ArgumentParser(description="Calibrate the overhead camera with the arm (the arm moves).")
    ap.add_argument("--speed", type=int, default=20)
    ap.add_argument("--out", default="/tmp/gptarm-calib")
    ap.add_argument("--only", default="", help="comma-separated stage names, for a partial run that is not saved")
    ap.add_argument("--resume", action="store_true", help="carry on from the stages already in --out")
    a = ap.parse_args()
    arm = A.Arm(speed=a.speed, floor_z_mm=60.0)
    stages = [s for s in STAGES if not a.only or s[0] in a.only.split(",")]
    try:
        cam = run(arm, Overhead(), log=lambda m: print(m, flush=True), save=not a.only, out_dir=a.out, stages=stages, min_points=6 if a.only else 12,
                  resume=a.resume)
    finally:
        arm.hold()
    print(json.dumps({k: v for k, v in cam.to_dict().items() if k != "points"}))
    for q in cam.points:
        print(q["stage"], q["px"], q.get("err_px"))


if __name__ == "__main__":
    main()
