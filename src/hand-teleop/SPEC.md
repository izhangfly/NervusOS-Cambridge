# Hand Teleop: expected behaviour

This is the definition of what Hand Teleop does. It covers both ways the app runs: as a NervusOS app
(camera through `nervus.camerad`, arm through Manipulator@1 under a control lease) and standalone
(`--standalone`: a V4L2 camera through ffmpeg, the arm through the PiPER driver's HTTP API on
loopback, as client `handteleop`). Where the two differ, it says so.

Units: metres and radians inside the app, base_link frame; the PiPER driver speaks millimetres and
degrees. The teleop workspace is the box x 0.08..0.46, y -0.30..0.30, z 0.10..0.55 m (`Teleop.WORKSPACE`).

## 1. Start-up

- Live video within about a second of the window opening, before the vision models have loaded.
  If there is no camera, the video area says why ("no camera: ...") and keeps retrying.
- The arm's state is shown in words, refreshed five times a second:
  - *driver not reachable*: the PiPER driver isn't running;
  - *not powered*: no feedback from the arm;
  - *e-stopped* / *fault*: needs Recover (Robot Arm app or the driver panel);
  - *teach mode*: someone is drag-teaching it;
  - *controlled by another app*: another driver client (Robot Arm, the ACT engine) holds it;
  - *motors off*: fine, engaging turns them on and holds the pose;
  - *ready*.
  On the NervusOS path these come from Manipulator@1's readiness, which is coarser.

## 2. Start pose

Teleop starts from wherever the arm is, so the arm must already be inside the workspace. At rest the
PiPER is folded near joint zero (TCP about x 0.05 to 0.06, z 0.17 to 0.21 m), which is outside it.

- **Go to start pose** moves the arm slowly in joint space (MOVE J: no IK, so it can't fault on a
  singularity) to the start pose:
  - standalone: `/api/speed` 12 % for the move (the previous speed is put back afterwards), the motors
    enabled if they are off, then `/api/joint` for each joint, and `/api/heartbeat` at least every
    0.5 s (the driver's panel deadman is 1.5 s);
  - NervusOS: Manipulator@1 `MoveJointTrajectory` to the start pose at about 10 deg/s. It needs the
    user's confirmation, so the move gets an extra minute to start.
- The move counts as done when every measured joint is within 1 deg of its target. Progress is shown
  while it runs (on the video and in the START POSE panel). It gives up after 25 s (stops the arm and
  says which joint was short), and STOP, Esc or Space aborts it at once; the arm then holds where it
  got to. Control is given back afterwards, unless the move was asked for in order to engage.
- The default start pose is the driver's READY pose, joints (0, 45, -60, 0, 45, 0) deg. Its TCP is
  (0.138, 0.000, 0.418) m by piper_sdk's forward kinematics (`dh_is_offset` 1), which matches the
  end pose measured on the arm at those joints ([138, 0, 418] mm, legacy/piper-act/docs/HARDWARE.md),
  so it is inside the workspace. `PiperKinematics` is a port of that FK and checks every start pose.
- **Set start pose here** saves the arm's current joints as the start pose, if its TCP is inside the
  workspace, in `~/.config/nervus/hand-teleop.json` (mode 0644). It is used from then on, also after a
  restart. **Reset start pose** goes back to READY. A saved pose that is malformed, outside the joint
  limits or outside the workspace is ignored, and the default is used.
- **Engage while outside the workspace** never fails silently: the app shows a prominent prompt, "The
  arm is outside the workspace: move it to the start pose?", with a button that moves there and then
  engages. When joint moves aren't available on this path, the button is disabled and says why.

## 3. Engage

- With the ENGAGE button (or Space), or hands-free: a **fist** held for 1.5 s engages, and a fist held
  for 1.5 s while engaged disengages. A ring around the hand counts down while the fist is held.
- A fist means all four fingertips are closer to the wrist than their MCP knuckles (the rule in
  AgileX-College's handpose_det), measured on the 3-D world landmarks so tilting the hand doesn't
  change it. A pinch (thumb to index with the other fingers out) or a thumb-to-four-fingers pinch is
  not a fist. Up to 150 ms of the hand not reading as a fist doesn't reset the count (landmark flicker).
- After a gesture fires, the fist has to open before another one can fire, so the fist that engaged
  doesn't go on to disengage.
- While engaged, a fist is a clutch (as in the AgileX demo, where the arm moves only while the hand is
  open): the arm and gripper hold while it is closed, and when the hand opens again the anchor is set
  afresh, so you can reposition your hand without moving the arm.
- The gesture is a setting (FIST GESTURE), on by default. With it off, fists are ordinary hand shapes.
- Engaging anchors the hand to the arm's current pose: nothing moves at the moment of engaging.

## 4. While engaged

- The arm follows the hand's movement since the anchor, in metres, scaled by the gains: sideways,
  up/down, and toward/away from the camera (reach).
- Wrist roll turns the tool about its own axis (off / on / inverted); pinch works the gripper.
- Hand lost for more than 300 ms: the arm holds, and re-anchors when the hand is back. Tracking jumps
  (5 cm or 20 deg in a frame) are dropped; three in a row count as a lost hand.
- At the workspace edge the target is clamped, and the anchor slips so the arm comes back as soon as
  the hand turns back.
- The status line always says why the arm isn't moving: hand lost, fist (clutch), palm edge-on,
  tracking jump, at the workspace edge, waiting for the arm's pose, refused by the driver or the
  kernel (with its words: busy, IK guard, stale feedback), outside the workspace.
- Why control was given up (disengaged, move stopped or timed out, lease or driver refused) stays on
  screen for 5 s, in red when it was a failure, instead of being replaced by the next tick's message.

## How this is checked

Unit tests, run on the Surface (`./gradlew test`), with no camera and never the real arm or driver:
the gesture classifier on landmark sets (fist, open hand, pinch, four-finger pinch, tilted) and its
hold timer; the forward kinematics against the arm's measured poses and the start-pose file; and the
control loop end to end against a fake PiPER driver on an ephemeral loopback port whose joints move
(`FakePiper`): the outside-workspace prompt, the start-pose move (speed, joint targets, heartbeats,
convergence, STOP, the 25 s timeout), fist engage / clutch / disengage, and the arm state in words.
`TeleopUiTest` renders the real screen off-screen and clicks through prompt, move and STOP.

## 5. Future work: full-arm retargeting

Not built. A second control mode would map the operator's whole arm onto the PiPER's joints instead
of following one hand point: shoulder, elbow and wrist from the body pose model (YOLO pose or a 3-D
body model) give the upper-arm and forearm directions, which map to J1 (yaw of the upper arm), J2/J3
(shoulder and elbow flexion), and the hand's palm frame to J4 to J6. It needs a calibration pose
(operator's arm along the PiPER's READY pose), joint-limit clamping and rate limits in joint space,
and joint streaming (`/api/stream`, or Manipulator@1 `MoveJointTrajectory` in short chunks) instead
of Cartesian servo. Monocular depth makes the elbow angle the weakest link; a depth camera or a
3-D lifting model would help.
