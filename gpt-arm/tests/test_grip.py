"""Turning the fingers without leaning sideways: `grip` is the table direction the fingers close along.

The arm can only lean roughly away from its base, so a bottle lying across the bearing could not be gripped
across its body by choosing a yaw (2026-11-16: every such move was out of reach). The gripper's own turn (J6)
can do it."""
import math
import unittest

import numpy as np

from gptarm import arm as A
from gptarm import direct as D
from gptarm import sim, task


def closing_dir(R):
    c = np.asarray(R)[:, 1]
    return math.degrees(math.atan2(c[1], c[0]))


def same_line(a, b, tol=0.5):
    d = (a - b) % 180.0
    return min(d, 180.0 - d) < tol


class GripTest(unittest.TestCase):
    def test_the_fingers_close_along_the_grip(self):
        for yaw in (-40.0, -20.0, 0.0, 30.0):
            for lean in (0.0, 15.0, 30.0, 50.0):
                for grip in (-80.0, -30.0, 0.0, 45.0, 90.0, 170.0):
                    R = A._rotation(300.0, -100.0, lean, yaw, grip)
                    self.assertTrue(same_line(closing_dir(R), grip), (yaw, lean, grip, closing_dir(R)))
                    self.assertLessEqual(abs(A.twist_for_grip(yaw, lean, grip)), 90.0)
                    # the tool still points where the lean and yaw say
                    axis = R[:, 2]
                    self.assertAlmostEqual(math.degrees(math.acos(-axis[2])), lean, places=6)

    def test_no_grip_is_across_the_lean(self):
        R = A._rotation(300.0, 0.0, 20.0, 10.0, None)
        self.assertTrue(same_line(closing_dir(R), 100.0))

    def test_a_bottle_lying_across_the_bearing_can_be_gripped(self):
        x, y, z = 342.0, -123.0, 150.0                   # the case that was out of reach with yaw 80
        self.assertIsNone(A.min_lean(x, y, z, 80.0))
        lean = A.min_lean(x, y, z, None, grip_deg=170.0)
        self.assertIsNotNone(lean)
        self.assertTrue(same_line(A.grip_of(A.flange_pose(x, y, z, lean, None, 170.0)), 170.0))

    def test_move_by_keeps_the_grip(self):
        t = sim.Table()
        cam = sim.camera()
        w = task.World(sim.SimArm(t, move_s=0.0), sim.SimOverhead(t, cam), sim.SimWrist(), cam, task.Config(),
                       perceive=sim.perceive(t))
        D.execute(w, {"action": "move_tool", "x": 300, "y": -100, "z": 200, "lean": None, "yaw": None, "grip": 60})
        self.assertTrue(same_line(w.arm.state()["grip"], 60.0, tol=1.0))
        D.execute(w, {"action": "move_by", "x": 0, "y": 0, "z": -60, "lean": None, "yaw": None, "grip": None})
        st = w.arm.state()
        self.assertAlmostEqual(st["tool"][2], 140.0, delta=1.0)
        self.assertTrue(same_line(st["grip"], 60.0, tol=1.0), st)
        self.assertEqual(D.describe({"action": "move_tool", "x": 1, "y": 2, "z": 3, "grip": 60}), "move_tool (1, 2, 3) grip 60")


if __name__ == "__main__":
    unittest.main()
