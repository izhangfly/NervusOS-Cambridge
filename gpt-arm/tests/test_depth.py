"""The wrist depth geometry on a synthetic scene: the mount fit and what the model is told."""
import math
import sys
import unittest
from pathlib import Path

import numpy as np

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from gptarm import arm as A, depth as D  # noqa: E402


def rot(axis, deg):
    a = math.radians(deg)
    c, s = math.cos(a), math.sin(a)
    if axis == "x":
        return np.array([[1, 0, 0], [0, c, -s], [0, s, c]])
    if axis == "y":
        return np.array([[c, 0, s], [0, 1, 0], [-s, 0, c]])
    return np.array([[c, -s, 0], [s, c, 0], [0, 0, 1]])


MOUNT = D.Mount(rot("z", 90) @ rot("x", 8), [0.0, -60.0, 40.0])     # a camera beside the gripper, tilted a little


def render(flange_pose, cap_xy, cap_top=125.0, cap_r=20.0, mount=MOUNT):
    """Depth image (mm along the camera axis) of a table at z=0 with a cap disc at cap_top."""
    fx, fy, cx, cy = D.intrinsics()
    p, R = A.K.pose_to_rt(flange_pose)
    Rc = np.asarray(R) @ mount.R
    oc = np.asarray(R) @ mount.t + np.asarray(p, float)
    vs, us = np.mgrid[0:D.H, 0:D.W]                           # the depth mode in use
    rays = np.stack([(us - cx) / fx, (vs - cy) / fy, np.ones_like(us, float)], axis=-1)      # z = 1 in camera
    dirs = rays @ Rc.T
    out = np.zeros((D.H, D.W), np.uint16)
    for zplane, check in ((cap_top, True), (0.0, False)):
        k = (zplane - oc[2]) / dirs[..., 2]
        hit = oc + dirs * k[..., None]
        inside = (np.hypot(hit[..., 0] - cap_xy[0], hit[..., 1] - cap_xy[1]) < cap_r) if check else np.ones_like(k, bool)
        sel = inside & (k > 0) & (out == 0)
        out[sel] = np.round(k[sel]).astype(np.uint16)          # k is the camera-z distance because ray z = 1
    return out


def render_boxes(flange_pose, boxes, floor=0.0, mount=MOUNT):
    """Depth image of a floor at `floor` with flat-topped boxes on it (each: xy, top, length, width, axis_deg);
    only their tops are drawn, which is what the map uses."""
    fx, fy, cx, cy = D.intrinsics()
    p, R = A.K.pose_to_rt(flange_pose)
    Rc = np.asarray(R) @ mount.R
    oc = np.asarray(R) @ mount.t + np.asarray(p, float)
    vs, us = np.mgrid[0:D.H, 0:D.W]
    dirs = np.stack([(us - cx) / fx, (vs - cy) / fy, np.ones_like(us, float)], axis=-1) @ Rc.T
    out = np.zeros((D.H, D.W), np.uint16)
    for b in sorted(boxes, key=lambda b: -b["top"]):                # the nearest surface wins
        k = (b["top"] - oc[2]) / dirs[..., 2]
        hit = oc + dirs * k[..., None]
        a = math.radians(b["axis_deg"])
        dx, dy = hit[..., 0] - b["xy"][0], hit[..., 1] - b["xy"][1]
        along, across = dx * math.cos(a) + dy * math.sin(a), -dx * math.sin(a) + dy * math.cos(a)
        sel = (np.abs(along) < b["length"] / 2) & (np.abs(across) < b["width"] / 2) & (k > 0) & (out == 0)
        out[sel] = np.round(k[sel]).astype(np.uint16)
    k = (floor - oc[2]) / dirs[..., 2]
    sel = (k > 0) & (out == 0)
    out[sel] = np.round(k[sel]).astype(np.uint16)
    return out


class DepthTest(unittest.TestCase):
    def test_kabsch_recovers_a_mount(self):
        rng = np.random.default_rng(3)
        P = rng.uniform(-80, 80, (8, 3)) + [0, 0, 200]
        Q = P @ MOUNT.R.T + MOUNT.t
        R, t, err = D.kabsch(P, Q)
        np.testing.assert_allclose(R, MOUNT.R, atol=1e-9)
        np.testing.assert_allclose(t, MOUNT.t, atol=1e-6)
        self.assertLess(err.max(), 1e-6)

    def test_summary_finds_the_cap_under_the_gripper(self):
        pose = A.flange_pose(310.0, 110.0, 230.0, A.min_lean(310.0, 110.0, 230.0))
        d = render(pose, (300.0, 100.0))
        s = D.summary(d, pose, MOUNT)
        o = s["objects_below"][0]
        self.assertEqual(o["label"], "D1")
        self.assertLess(math.dist(o["centre_xy_mm"], (300, 100)), 6, s)
        self.assertLess(abs(o["top_z_mm"] - 125), 4, s)
        self.assertLess(abs(o["length_mm"] - 40), 8, s)                   # the 40 mm disc
        self.assertEqual(o["offset_from_tool_xy_mm"], [o["centre_xy_mm"][0] - 310, o["centre_xy_mm"][1] - 110])

    def test_a_lying_bottle_beside_a_wall_is_measured_apart_from_it(self):
        """The basket case: a bottle lying on a floor, a tall thin wall nearby. The map tells them apart, gives
        the bottle's long axis, and puts the tool point where it is."""
        bottle = dict(xy=(290.0, -160.0), top=55.0, length=125.0, width=45.0, axis_deg=35.0)
        wall = dict(xy=(290.0, -240.0), top=105.0, length=260.0, width=12.0, axis_deg=0.0)
        pose = A.flange_pose(300.0, -150.0, 230.0, A.min_lean(300.0, -150.0, 230.0))
        d = render_boxes(pose, [bottle, wall], floor=8.0)
        s = D.summary(d, pose, MOUNT)
        objs = s["objects_below"]
        b = min(objs, key=lambda o: math.dist(o["centre_xy_mm"], bottle["xy"]))
        self.assertLess(math.dist(b["centre_xy_mm"], bottle["xy"]), 10, objs)
        self.assertLess(abs(b["top_z_mm"] - 55), 4, objs)
        self.assertLess(abs(b["length_mm"] - 125), 15, objs)
        self.assertLess(abs(b["width_mm"] - 45), 12, objs)
        self.assertLess(abs(b["long_axis_deg"] - 35), 6, objs)
        w = min(objs, key=lambda o: math.dist(o["centre_xy_mm"], wall["xy"]))
        self.assertIsNot(w, b)
        self.assertGreater(w["top_z_mm"], 95)
        img = D.top_view(d, pose, MOUNT, objects=objs)
        self.assertEqual(img.shape[1], 640)
        self.assertGreater(img.shape[0], 640)

    def test_the_gripper_is_not_an_object(self):
        """Points on the fingers themselves (just under the camera) are dropped."""
        pose = A.flange_pose(300.0, 0.0, 200.0, A.min_lean(300.0, 0.0, 200.0))
        p, R = A.K.pose_to_rt(pose)
        finger = np.asarray(p, float) + np.asarray(R) @ np.array([0.0, 30.0, 110.0])
        pts = np.vstack([np.column_stack([np.random.default_rng(0).uniform(150, 450, 4000),
                                          np.random.default_rng(1).uniform(-150, 150, 4000), np.zeros(4000)]),
                         np.repeat(finger[None, :], 50, axis=0)])
        cam = (pts - (np.asarray(R) @ MOUNT.t + np.asarray(p, float))) @ (np.asarray(R) @ MOUNT.R)
        fx, fy, cx, cy = D.intrinsics()
        u, v = np.round(cam[:, 0] / cam[:, 2] * fx + cx).astype(int), np.round(cam[:, 1] / cam[:, 2] * fy + cy).astype(int)
        d = np.zeros((D.H, D.W), np.uint16)
        ok = (cam[:, 2] > 0) & (u >= 0) & (u < D.W) & (v >= 0) & (v < D.H)
        d[v[ok], u[ok]] = np.round(cam[ok, 2]).astype(np.uint16)
        kept = D.base_points(d, pose, MOUNT, step=1)
        self.assertFalse((kept[:, 2] > 50).any())

    def test_nearest_top_and_calibration_math(self):
        """Views of a known cap from several poses give back the mount."""
        P, Q = [], []
        target = np.array([300.0, 100.0, 125.0])
        for dx, dy, z in ((0, 0, 230), (40, 0, 230), (-40, 0, 280), (0, 40, 280), (0, -40, 230), (30, 30, 280)):
            pose = A.flange_pose(300.0 + dx, 100.0 + dy, z, A.min_lean(300.0 + dx, 100.0 + dy, z))
            hit = D.nearest_top(render(pose, target[:2]))
            if hit is None:                          # the cap is cut by the picture's edge from this pose
                continue
            u, v, zc, _ = hit
            pf, Rf = A.K.pose_to_rt(pose)
            P.append(D.backproject(u, v, zc))
            Q.append(np.asarray(Rf).T @ (target - np.asarray(pf, float)))
        self.assertGreaterEqual(len(P), 4)
        R, t, err = D.kabsch(np.array(P), np.array(Q))
        self.assertLess(float(np.sqrt((err ** 2).mean())), 4.0)
        self.assertLess(np.linalg.norm(t - MOUNT.t), 8.0)


class TablePlaneTest(unittest.TestCase):
    def test_the_bare_table_gives_back_the_mount(self):
        views = []
        for x, y, z, lean, twist in D.table_poses():
            p, R = D._pose_with_twist(x, y, z, lean, twist)
            if A.KIN.nearest_ik(A.K.rt_to_pose(p, R), [0, 45, -60, 0, 45, 0]) is None:
                continue
            pose = A.K.rt_to_pose(p, R)
            d = render(pose, (900.0, 900.0))                   # no cap in view: bare table
            fit = D.fit_plane(D.cloud(d))
            self.assertIsNotNone(fit)
            n, dist, _ = fit
            pf, Rf = A.K.pose_to_rt(pose)
            views.append((np.asarray(Rf), np.asarray(pf, float), n, dist))
        self.assertGreaterEqual(len(views), 6)
        m, q = D.plane_fit_mount(views)
        self.assertGreater(q["conditioning"], 0.05, q)
        np.testing.assert_allclose(m.R, MOUNT.R, atol=0.02)
        self.assertLess(np.linalg.norm(m.t - MOUNT.t), 4.0, (m.t, MOUNT.t))


if __name__ == "__main__":
    unittest.main()
