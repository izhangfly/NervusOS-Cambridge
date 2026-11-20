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
| 2 | Laundry: pick the top garment at run time, skip loaded ones; garments sleep until handled (see below) | done |
| 3 | Conveyor beside the arm; one belt position moves every part and the belt surface | done (kinematic), being made physical (step 7) |
| 4 | Robot contact: finger pads, palm, touch sensors, elliptic friction cones | done (both models; PiPER finger coupling stiffened) |
| 5 | IK with gripper twist (yaw), so the fingers close across two parallel faces | done (`inverseKinematics(p,target,seed,yaw)`, ~2 ms) |
| 6 | Physical pick and place: squeeze with limited force, check touch sensors, retry, no weld joints | rigid parts done (`rigidTransfer` in planner.js); soft items next |
| 7 | Physical conveyor (driven belt body, friction carries parts), carriers with a bearing pocket | done |
| 8 | Physical cloth pinch and washer lid opened by gripping its handle | done; laundry layout and garment states reworked (below) |
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

## Laundry rework (20 November 2026, owner's report: arm hits the basket, clothes stuck on the basket and the gripper)

- Basket: the old downloaded basket was round and tall while its collision boxes were a smaller rectangle, so
  garments caught on walls nobody could see. It is now procedural (`laundryBasket` in view.js), drawn at the
  exact size of its collision boxes: 240 x 200 mm, 65 mm walls, slotted sides, rolled rim. The washer and
  basket GLBs are no longer loaded (credits updated).
- Layout: basket at (.17, -.27), table pile at (.45, -.07): about 10 cm of clear table between them.
- Pinch point: the highest fold of the chosen garment where both open fingers clear the basket wall by 15 mm.
- Garment states (`garmentState` in model.js), contact bits world 1/1, hand 8/1, grip skin 0/16:
  - sleep (2/1): nobody is handling it and it lies still; gravity compensated, damped, world contact only;
  - free (2/3): falling or settling; also collides with the other garments, so it lands on top of a pile;
  - held (18/11): the one garment the job picks; the only one the fingers and the grip skin touch.
  A free garment sleeps after 0.5 s still. Cost per 4 ms step: 1.7 ms idle, about 3 ms with one garment
  awake (12 ms when every garment collides with every other).
- Grip: on cloth the fingers stop 2 mm short of closed (closing fully squeezed the sheet out of the pinch);
  touch is checked after closing and again 5 cm into the lift, with a re-grasp on slip.
- Release: the garment is lowered until it rests on the drum floor or the pile, then the fingers open and the
  garment becomes free (the fingers no longer touch it), so it can't hang on a finger.
- A garment that misses its target is picked again (up to three extra jobs per run); the result line reports
  what is in the drum at the end.
- Over the washer the hand turns to lie along the hinge (Panda's hand is 20 cm wide and met the open lid), and
  the lid hinge has 0.12 N m of friction so a hanging garment can't swing it shut.
- A free garment sleeps once its mean vertex speed stays under 2 cm/s for 0.5 s (or after 4 s under 5 cm/s):
  two free garments resting on each other jitter slightly and otherwise never slept (8.7 ms per step).
- Verified 20 November 2026 in Chromium: PiPER "Clear the table & load all" six of six in the drum, lid 1.24 rad;
  PiPER and Panda "Load the laundry basket" three of three; idle step 1.6 ms afterwards.
