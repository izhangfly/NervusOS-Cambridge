# Live demo v2: physical grasping and the fleet view

Owner's brief (20 November 2026): the demo must show NervusOS's idea, *one app runs on different robot
bodies*, and the robots must really interact with things: fingers that squeeze, objects that can't pass
through each other, garments and bearings gripped and carried by friction, sensing that decides what happens
next. Concept chosen: **fleet view** (two different arms run the same app side by side, with a trace of the
capability calls each robot's provider turns into its own joint commands).

## Status

| Step | What | State |
|---|---|---|
| 1 | Garments: coarse MuJoCo flex, fine smooth render surface, painted garment textures | done (`cloth.js`) |
| 2 | Laundry: pick the top garment at run time, skip loaded ones; no garment-garment contact | done |
| 3 | Conveyor beside the arm; one belt position moves every part and the belt surface | done (kinematic), being made physical (step 7) |
| 4 | Robot contact: finger pads, palm, touch sensors, elliptic friction cones | done (both models; PiPER finger coupling stiffened) |
| 5 | IK with gripper twist (yaw), so the fingers close across two parallel faces | done (`inverseKinematics(p,target,seed,yaw)`, ~2 ms) |
| 6 | Physical pick and place: squeeze with limited force, check touch sensors, retry, no weld joints | rigid parts done (`rigidTransfer` in planner.js); soft items next |
| 7 | Physical conveyor (driven belt body, friction carries parts), carriers with a bearing pocket | todo |
| 8 | Physical cloth pinch and washer lid opened by gripping its handle | todo |
| 9 | Fleet view: PiPER and Panda workcells in one scene, install once, run on both, capability trace | todo |
| 10 | Rebuild for ianzhyy.com and the NervusOS VPS site, verify in Chromium and WebKit | todo |

## Decisions and measurements

- Physics cost per 4 ms step, laundry scene, before any change: 27.7 ms (969 DoF). Garment-garment contact
  was the cost: 7x7-grid garments 15.6 ms with it, 5.5 ms without; 6x6 grid without it 3.4 ms. Garments now
  collide with the world, the gripper and the washer, not with each other (`contype="2" conaffinity="1"`).
- Both robot models shipped visual-only geometry (all `contype="0"`). Finger axes: PiPER fingers close along
  the tool site's y axis (link6 frame), fingertips at the tool site; Panda follows MuJoCo Menagerie (pads
  `size .0085 .004 .0085` at `0 .0055 .0445` in each finger frame, closing along the hand's y axis).
- Collision groups: world and rigid parts `contype 1 / conaffinity 1`; garments `2 / 1` (world yes, each other
  no); finger pads and palm `1 / 1`.
- A grasp is valid only when both finger-pad touch sensors read above a threshold after closing; otherwise the
  job re-plans from the object's current pose (up to two retries) and the trace says why.

- Per 4 ms step after contact was added: Toy tidy 1.6 ms, Laundry 3.2 ms, BearingFit 0.2 ms, LineSight 0.1 ms; IK ~2 ms per target.
- Headless/background tabs throttle requestAnimationFrame: judge speed only in a visible tab.
