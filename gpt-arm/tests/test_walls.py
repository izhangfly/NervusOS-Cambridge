"""The backboard behind the table: no part of the arm may come near it (2026-11-16: the wrist camera slid along it)."""
import json
import math
import tempfile
import unittest
from pathlib import Path

import numpy as np

from gptarm import arm as A
from gptarm import depth as Dp
from gptarm import sim

# joint angles from the run where the wrist camera touched the backboard (tool at x 454 to 465 mm, lean 15 to 25)
TOUCHED = [[4.2, 108.4, -99.1, 0.0, 60.7, 0.0], [4.2, 112.2, -100.8, 0.0, 63.7, 0.0],
           [4.8, 109.7, -96.9, 0.0, 62.1, 0.0], [4.8, 113.7, -101.2, 0.0, 67.4, 0.0]]


class WallTest(unittest.TestCase):
    walls = [dict(w) for w in A.DEFAULT_WALLS]

    def test_the_poses_that_touched_are_refused(self):
        for q in TOUCHED:
            pose = A.KIN.fk_pose(q)
            gap, part, wall = A.wall_clearance(pose, self.walls)
            self.assertLess(gap, A.WALL_MARGIN_MM, q)
            self.assertEqual(wall, "backboard")
            with self.assertRaises(A.ArmError):
                A.check_walls(pose, A.flange_pose(300, 0, 250), self.walls)

    def test_the_camera_is_what_touches(self):
        _, part, _ = A.wall_clearance(A.KIN.fk_pose(TOUCHED[1]), self.walls)
        self.assertEqual(part, "wrist camera")

    def test_ordinary_table_work_is_allowed(self):
        for x, y, z in [(160, 0, 330), (300, 0, 60), (330, -100, 120), (380, 60, 150), (250, 200, 80)]:
            lean = A.min_lean(x, y, z)
            self.assertIsNotNone(lean, (x, y, z))
            gap, _, _ = A.wall_clearance(A.flange_pose(x, y, z, lean), self.walls)
            self.assertGreaterEqual(gap, A.WALL_MARGIN_MM, (x, y, z))

    def test_moving_away_is_always_allowed(self):
        close = A.KIN.fk_pose(TOUCHED[3])
        A.check_walls(A.flange_pose(300, 0, 250), close, self.walls)        # away: fine
        A.check_walls(close, close, self.walls)                              # staying put: fine
        with self.assertRaises(A.ArmError):
            A.check_walls(A.KIN.fk_pose(TOUCHED[1]), A.flange_pose(300, 0, 250), self.walls)

    def test_more_lean_pulls_the_camera_back(self):
        # leaning the tips outward swings the wrist, and the camera on it, back towards the base
        gaps = [A.wall_clearance(A.flange_pose(400, 0, 150, lean, yaw_deg=0), self.walls)[0] for lean in (10, 30, 50)]
        self.assertLess(gaps[0], gaps[1])
        self.assertLess(gaps[1], gaps[2])

    def test_load_walls_from_file_and_fallback(self):
        with tempfile.TemporaryDirectory() as d:
            p = Path(d) / "workspace.json"
            self.assertEqual(A.load_walls(p)[0]["offset_mm"], A.DEFAULT_WALLS[0]["offset_mm"])
            p.write_text(json.dumps({"walls": [{"name": "backboard", "normal": [0.99, -0.1, 0.0], "offset_mm": 520.0}]}))
            self.assertEqual(A.load_walls(p)[0]["offset_mm"], 520.0)
            p.write_text("not json")
            self.assertEqual(A.load_walls(p)[0]["name"], "backboard")


class SimWallTest(unittest.TestCase):
    def test_sim_refuses_the_backboard(self):
        arm = sim.SimArm(sim.Table(), move_s=0.0)
        with self.assertRaises(A.ArmError) as e:
            arm.move_tool(465, 34, 190, lean=25, yaw_deg=4)
        self.assertIn("backboard", str(e.exception))
        arm.move_tool(330, 0, 200)                                           # the middle of the table is fine


class FitWallTest(unittest.TestCase):
    def test_recovers_a_tilted_wall_and_takes_its_nearest_part(self):
        rng = np.random.default_rng(1)
        n = np.array([math.cos(math.radians(6)), math.sin(math.radians(-6)), 0.0])
        t = np.array([-n[1], n[0], 0.0])
        pts = [510 * n + s * t + np.array([0, 0, z]) + rng.normal(0, 2, 3)
               for s, z in zip(rng.uniform(-300, 300, 1500), rng.uniform(80, 400, 1500))]
        foot = [500 * n + s * t + np.array([0, 0, z]) for s, z in zip(rng.uniform(-300, 300, 60), rng.uniform(80, 110, 60))]
        junk = rng.uniform([430, -350, 80], [600, 350, 400], (100, 3))
        fit = Dp.fit_wall(np.vstack([pts, foot, junk]))
        self.assertIsNotNone(fit)
        got, d, _ = fit
        self.assertLess(math.degrees(math.acos(min(1.0, float(got @ n)))), 1.0)
        self.assertAlmostEqual(d, 510, delta=3)
        s = np.vstack([pts, foot]) @ got
        self.assertLess(np.percentile(s, 2), 506)                          # the foot pulls the saved plane in

    def test_no_wall_in_a_floor(self):
        rng = np.random.default_rng(2)
        self.assertIsNone(Dp.fit_wall(rng.uniform([0, -300, 0], [400, 300, 2], (500, 3))))


if __name__ == "__main__":
    unittest.main()
