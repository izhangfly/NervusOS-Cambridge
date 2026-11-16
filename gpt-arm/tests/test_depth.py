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
        o = s["nearest_object"]
        self.assertLess(math.dist(o["top_centre_xy_mm"], (300, 100)), 6, s)
        self.assertLess(abs(o["top_z_mm"] - 125), 4, s)

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
