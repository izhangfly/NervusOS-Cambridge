"""The camera model: a known camera is recovered from points, and pixels go back to the table."""
import math
import sys
import unittest
from pathlib import Path

import numpy as np

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from gptarm import arm as A  # noqa: E402
from gptarm import calib as C  # noqa: E402


def a_camera():
    """Looking down from (300, -150, 650) mm, tilted, 1600x1200."""
    f, c = 1100.0, np.array([300.0, -150.0, 650.0])
    Kc = np.array([[f, 0, 800.0], [0, f, 600.0], [0, 0, 1.0]])
    rx, rz = math.radians(165.0), math.radians(100.0)
    Rx = np.array([[1, 0, 0], [0, math.cos(rx), -math.sin(rx)], [0, math.sin(rx), math.cos(rx)]])
    Rz = np.array([[math.cos(rz), -math.sin(rz), 0], [math.sin(rz), math.cos(rz), 0], [0, 0, 1]])
    R = Rx @ Rz
    return Kc @ np.hstack([R, (-R @ c).reshape(3, 1)]), c


class CalibTest(unittest.TestCase):
    def test_fit_recovers_a_camera(self):
        P, c = a_camera()
        rng = np.random.default_rng(1)
        W = np.column_stack([rng.uniform(150, 450, 20), rng.uniform(-300, 300, 20), rng.uniform(100, 320, 20)])
        h = np.hstack([W, np.ones((20, 1))]) @ P.T
        pix = h[:, :2] / h[:, 2:3]
        K, dist, rv, tv, err = C.fit_pinhole(W, pix, (1600, 1200))
        self.assertLess(err.max(), 0.05)
        cam = C.Camera(K, dist, rv, tv, (1600, 1200))
        np.testing.assert_allclose(cam.centre(), c, atol=1.0)
        self.assertLess(abs(cam.K[0, 0] - 1100.0), 2.0)
        u, v = cam.project([400.0, 120.0, 0.0])
        x, y = cam.ground(u, v, 0.0)
        self.assertAlmostEqual(x, 400.0, places=4)
        self.assertAlmostEqual(y, 120.0, places=4)
        self.assertTrue(0.3 < cam.mm_per_px(300, 0) < 1.5)

    def test_fit_finds_the_marker_offset_and_drops_an_outlier(self):
        P, _ = a_camera()
        rng = np.random.default_rng(2)
        pts = []
        for _ in range(22):
            x, y, z = rng.uniform(170, 420), rng.uniform(-80, 300), rng.uniform(115, 300)
            lean = rng.choice([40.0, 45.0, 50.0])
            if not A.reachable(x, y, z, lean):
                continue
            flange = A.flange_pose(x, y, z, lean)
            m = C.marker_point(flange, 97.5)
            h = P @ np.append(m, 1.0)
            pts.append({"flange": flange, "px": list(h[:2] / h[2] + rng.normal(0, 0.5, 2))})
        self.assertGreaterEqual(len(pts), 14)
        pts[3]["px"][0] += 80.0
        cam = C.fit(pts, (1600, 1200))
        self.assertEqual(len(cam.points), len(pts) - 1)
        self.assertLess(cam.residual_px, 1.5)
        self.assertLess(abs(cam.marker_mm - 97.5), 8.0)
        u, v = cam.project([300.0, 100.0, 0.0])
        h = P @ np.array([300.0, 100.0, 0.0, 1.0])
        self.assertLess(math.hypot(u - h[0] / h[2], v - h[1] / h[2]), 6.0)

    def test_marker_from_two_photographs(self):
        a = np.full((600, 800, 3), 180, np.uint8)
        b = a.copy()
        a[300:340, 380:392] = 20
        a[300:340, 440:452] = 20          # open fingers
        b[290:340, 405:427] = 20          # closed
        b[100:160, 600:660] = 20          # something else that moved and did not move back
        c = a.copy()
        c[100:160, 600:660] = 20
        (u, v), n = C.gripper_marker(a, b, c)
        self.assertLess(abs(u - 415.5), 3)
        self.assertLess(abs(v - 314.5), 3)
        self.assertIsNone(C.gripper_marker(a, a, a))
        dim = lambda i: (i * 0.3).astype(np.uint8)      # the same in a dim room
        (u, v), n = C.gripper_marker(dim(a), dim(b), dim(c))
        self.assertLess(abs(u - 415.5), 3)

    def test_stage_points_are_reachable(self):
        for name, zs, xs, ys, margin in C.STAGES:
            n = sum(C._lean_for(x, y, (z, C.CARRY_Z) if margin else (z,)) is not None for x in xs for y in ys for z in zs[:1])
            self.assertGreaterEqual(n, 8, name)


if __name__ == "__main__":
    unittest.main()
