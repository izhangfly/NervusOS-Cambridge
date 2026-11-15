"""Where the overhead camera is: a projection from the arm's base frame (mm) to its pixels.

There is no checkerboard and nothing is measured by hand. The arm is its own target: at each calibration
pose it opens and closes the gripper, and the pixels that change between the two photographs are the
fingers. Their centre is a point on the tool axis a fixed distance past the flange, which the driver knows
in millimetres. Twenty or so such pairs give the 3x4 projection matrix by the direct linear transform; the
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
    def __init__(self, P, size, marker_mm=MARKER_MM, points=None, residual_px=None, when=None):
        self.P = np.asarray(P, float).reshape(3, 4)
        self.size = tuple(size)                    # (width, height)
        self.marker_mm = float(marker_mm)
        self.points = points or []
        self.residual_px = residual_px
        self.when = when or time.time()

    def project(self, xyz):
        """Base-frame mm -> pixels. One point (3,) -> (2,), or (N, 3) -> (N, 2)."""
        X = np.atleast_2d(np.asarray(xyz, float))
        h = np.hstack([X, np.ones((len(X), 1))]) @ self.P.T
        uv = h[:, :2] / h[:, 2:3]
        return uv[0] if np.ndim(xyz) == 1 else uv

    def ground(self, u, v, z=0.0):
        """The point at height z (mm) that appears at pixel (u, v): (x, y)."""
        P = self.P
        a = np.array([[P[0, 0] - u * P[2, 0], P[0, 1] - u * P[2, 1]],
                      [P[1, 0] - v * P[2, 0], P[1, 1] - v * P[2, 1]]])
        b = -np.array([(P[0, 2] - u * P[2, 2]) * z + P[0, 3] - u * P[2, 3],
                       (P[1, 2] - v * P[2, 2]) * z + P[1, 3] - v * P[2, 3]])
        x, y = np.linalg.solve(a, b)
        return float(x), float(y)

    def centre(self):
        """The camera's own position (mm)."""
        _, _, vt = np.linalg.svd(self.P)
        c = vt[-1]
        return c[:3] / c[3]

    def mm_per_px(self, x, y, z=0.0):
        u, v = self.project([x, y, z])
        x2, y2 = self.ground(u + 1.0, v, z)
        return math.hypot(x2 - x, y2 - y)

    def to_dict(self):
        return {"P": self.P.tolist(), "size": list(self.size), "marker_mm": self.marker_mm,
                "points": self.points, "residual_px": self.residual_px, "when": self.when,
                "camera_mm": [round(float(v), 1) for v in self.centre()]}

    def save(self, path=CAL_PATH):
        path = Path(path)
        path.parent.mkdir(parents=True, exist_ok=True)
        tmp = path.with_suffix(".tmp")
        tmp.write_text(json.dumps(self.to_dict(), indent=1))
        tmp.replace(path)

    @classmethod
    def load(cls, path=CAL_PATH):
        try:
            d = json.loads(Path(path).read_text())
        except (OSError, ValueError):
            return None
        return cls(d["P"], d["size"], d.get("marker_mm", MARKER_MM), d.get("points"), d.get("residual_px"), d.get("when"))


def fit_dlt(world, pix):
    """The projection matrix through N >= 6 points (not all in one plane), and each point's error in pixels."""
    W, I = np.asarray(world, float), np.asarray(pix, float)
    if len(W) < 6:
        raise CalibrationError("a fit needs at least 6 points")

    def norm(X):
        c = X.mean(axis=0)
        s = math.sqrt(X.shape[1]) / (np.linalg.norm(X - c, axis=1).mean() + 1e-9)
        T = np.eye(X.shape[1] + 1)
        T[:-1, :-1] *= s
        T[:-1, -1] = -s * c
        return T

    Tw, Ti = norm(W), norm(I)
    Wn = (np.hstack([W, np.ones((len(W), 1))]) @ Tw.T)
    In = (np.hstack([I, np.ones((len(I), 1))]) @ Ti.T)
    rows = []
    for X, (u, v, _) in zip(Wn, In):
        rows.append(np.concatenate([X, np.zeros(4), -u * X]))
        rows.append(np.concatenate([np.zeros(4), X, -v * X]))
    _, _, vt = np.linalg.svd(np.asarray(rows))
    P = np.linalg.inv(Ti) @ vt[-1].reshape(3, 4) @ Tw
    h = np.hstack([W, np.ones((len(W), 1))]) @ P.T
    err = np.linalg.norm(h[:, :2] / h[:, 2:3] - I, axis=1)
    return P / np.linalg.norm(P[2, :3]), err


def marker_point(flange_pose, marker_mm):
    p, R = A.K.pose_to_rt(flange_pose)
    return np.asarray(p, float) + R @ np.array([0.0, 0.0, marker_mm])


def fit(points, size, search=True):
    """points: [{"flange": [6], "px": [u, v]}, ...] -> Camera. Drops gross outliers once."""
    pts = list(points)

    def solve(pp, L):
        P, err = fit_dlt([marker_point(q["flange"], L) for q in pp], [q["px"] for q in pp])
        return P, err

    def best(pp):
        if not search or len(pp) < 10:
            return (MARKER_MM, *solve(pp, MARKER_MM))
        out = None
        for L in np.arange(MARKER_RANGE[0], MARKER_RANGE[1] + 0.1, 2.5):
            P, err = solve(pp, L)
            if out is None or np.sqrt((err ** 2).mean()) < np.sqrt((out[2] ** 2).mean()):
                out = (float(L), P, err)
        return out

    L, P, err = best(pts)
    keep = err <= max(3.0 * np.median(err), 6.0)
    if len(pts) >= 10 and not keep.all() and keep.sum() >= 8:
        pts = [q for q, k in zip(pts, keep) if k]
        L, P, err = best(pts)
    for q, e in zip(pts, err):
        q["err_px"] = round(float(e), 1)
    return Camera(P, size, L, pts, round(float(np.sqrt((err ** 2).mean())), 2))


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


def static_dark(frames, level=0.42):
    """Pixels that are dark in every one of the frames: things that stand still (the basket, the bottle's cap,
    the floor beyond the table) but not the arm, which was somewhere else in each."""
    out = None
    for f in frames:
        g = cv2.cvtColor(f, cv2.COLOR_BGR2GRAY)
        dark = g < level * np.percentile(g, 90)
        out = dark if out is None else (out & dark)
    return out


def gripper_clear(cam, dark, x, y, z, lean, margin_px):
    """Would the gripper at this pose stay clear of every static dark thing in the image? Checked on the
    way down too (the descent is vertical). The fit is rough at this point, hence the generous margin."""
    h, w = dark.shape
    for dz in (0.0, 50.0, 100.0):
        pose = A.flange_pose(x, y, z + dz, lean)
        pts = A.tip_points(pose) + [np.array([x, y, z + dz]), np.asarray(pose[:3], float)]
        for u, v in cam.project(np.asarray(pts)):
            if not (margin_px <= u < w - margin_px and margin_px <= v < h - margin_px):
                return False
            m = int(margin_px)
            if dark[int(v) - m:int(v) + m, int(u) - m:int(u) + m].any():
                return False
    return True


# ---- the routine -------------------------------------------------------------------------------------------
LEANS = (40.0, 45.0, 50.0, 55.0)
CARRY_Z = 250.0
STAGES = [   # name, tool heights, x values, y values, clearance margin in pixels (None: above everything)
    ("high", (250.0, 300.0), (170.0, 250.0, 330.0), (-40.0, 100.0, 240.0), None),
    ("middle", (185.0,), (220.0, 320.0, 420.0), (-60.0, 90.0, 240.0), 130),
    ("low", (115.0,), (220.0, 300.0, 380.0), (-80.0, 40.0, 160.0, 280.0), 110),
]


def _lean_for(x, y, zs, steep=False):
    """A lean that reaches every height in zs. The points alternate between gentle and steep leans: the
    marker's distance along the tool axis only shows when the tool's direction varies."""
    for lean in (LEANS[::-1] if steep else LEANS):
        if all(A.reachable(x, y, z, lean) and A.flange_pose(x, y, z, lean)[2] <= A.CEILING_FLANGE_MM for z in zs):
            return lean
    return None


def run(arm, overhead, log=print, save=True, out_dir=None, stages=STAGES, min_points=12):
    """Calibrate with the arm. Returns the Camera. Raises CalibrationError if it can't."""
    points, opens, cam, size = [], [], None, None
    out_dir = Path(out_dir) if out_dir else None
    if out_dir:
        out_dir.mkdir(parents=True, exist_ok=True)

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
        points.append({"flange": [round(f, 2) for f in flange], "px": [round(u, 1), round(v, 1)], "n": n, "stage": stage})
        log(f"  {stage} ({x:.0f}, {y:.0f}, {z:.0f}) lean {lean:.0f}: seen at ({u:.0f}, {v:.0f}), {n} px")

    arm.enable()
    arm.grip(A.GRIPPER_OPEN_MM, wait_s=0.5)
    for name, zs, xs, ys, margin in stages:
        log(f"{name} points")
        dark = static_dark(opens) if margin is not None else None
        if margin is not None and cam is None:
            raise CalibrationError("too few of the high points were seen to go lower safely")
        todo = []
        for i, x in enumerate(xs):
            for j, y in enumerate(ys if i % 2 == 0 else ys[::-1]):       # boustrophedon: short moves
                z = zs[(i + j) % len(zs)]
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
    for x in range(0, 601, 100):
        a, b = cam.project([x, -500.0, 0.0]), cam.project([x, 500.0, 0.0])
        cv2.line(out, tuple(map(int, a)), tuple(map(int, b)), (0, 200, 0) if x else (0, 0, 255), 1, cv2.LINE_AA)
    for y in range(-500, 501, 100):
        a, b = cam.project([0.0, y, 0.0]), cam.project([600.0, y, 0.0])
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
    a = ap.parse_args()
    arm = A.Arm(speed=a.speed, floor_z_mm=60.0)
    stages = [s for s in STAGES if not a.only or s[0] in a.only.split(",")]
    try:
        cam = run(arm, Overhead(), save=not a.only, out_dir=a.out, stages=stages, min_points=6 if a.only else 12)
    finally:
        arm.hold()
    print(json.dumps({k: v for k, v in cam.to_dict().items() if k != "points"}))
    for q in cam.points:
        print(q["stage"], q["px"], q.get("err_px"))


if __name__ == "__main__":
    main()
