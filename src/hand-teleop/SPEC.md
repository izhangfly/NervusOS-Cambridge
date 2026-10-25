# Hand Teleop: expected behaviour

This is the definition of what Hand Teleop does. It covers both ways the app runs: as a NervusOS app
(camera through `nervus.camerad`, arm through Manipulator@1 under a control lease) and standalone
(`--standalone`: a V4L2 camera through ffmpeg, the arm through the PiPER driver's HTTP API on
loopback, as client `handteleop`). Where the two differ, it says so.

Units: metres and radians inside the app, base_link frame (x forward, y left, z up, origin at the
base); the PiPER driver speaks millimetres and degrees. Camera axes: x right, y down, z away from the
camera.

There are two control modes (CONTROL MODE in the side panel, chosen while not in control):

- **Mirror my arm** (the default, section 4): absolute and shoulder-anchored. The arm puts its TCP where
  your hand is relative to your shoulder, scaled to the robot, with the robot's shoulder (J2) as the
  origin.
- **Relative (clutch)** (section 5): the arm follows your hand's movement since you engaged, scaled by
  the gains, from wherever the arm was.

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
- In mirror mode the overlay shows the shoulder-to-hand vector and where it would send the arm even
  before engaging, so the mapping can be checked with the arm still.

## 2. Start pose

Clutch mode starts from wherever the arm is, so the arm must already be inside its workspace (the box
x 0.08..0.46, y -0.30..0.30, z 0.10..0.55 m, `Teleop.WORKSPACE`). At rest the PiPER is folded near joint
zero (TCP about x 0.05 to 0.06, z 0.17 to 0.21 m), which is outside it. Mirror mode doesn't need this:
it sends the arm to the hand from anywhere (section 4).

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
- **Engage while outside the workspace** (clutch mode) never fails silently: the app shows a prominent
  prompt, "The arm is outside the workspace: move it to the start pose?", with a button that moves
  there and then engages. When joint moves aren't available on this path, the button is disabled and
  says why.

## 3. Engage

- With the ENGAGE button (or Space), or hands-free: a **fist** held for 1.5 s engages, and a fist held
  for 1.5 s while engaged disengages. A ring around the hand counts down while the fist is held.
- A fist means all four fingertips are closer to the wrist than their MCP knuckles (the rule in
  AgileX-College's handpose_det), measured on the 3-D world landmarks so tilting the hand doesn't
  change it. A pinch (thumb to index with the other fingers out) or a thumb-to-four-fingers pinch is
  not a fist. Up to 150 ms of the hand not reading as a fist doesn't reset the count (landmark flicker).
- After a gesture fires, the fist has to open before another one can fire, so the fist that engaged
  doesn't go on to disengage.
- While engaged, a shorter fist holds the arm and gripper (as in the AgileX demo, where the arm moves
  only while the hand is open). In clutch mode the anchor is set afresh when the hand opens, so the
  hand can be repositioned; in mirror mode the arm travels to the hand again (APPROACH).
- The gesture is a setting (FIST GESTURE), on by default. With it off, fists are ordinary hand shapes.

## 4. Mirror my arm

### The measurement

From the YOLO26 pose model: the operating side's shoulder (COCO keypoint 5 for the left, 6 for the
right) and both shoulders' pixel distance `w_px`. From the hand model: the palm centre `(u_h, v_h)`
(the mean of the wrist and the four knuckles) and the palm's depth `Z_h` (the palm bones' metric
length from the world landmarks over their pixel length, times f; see HandMath). With the focal length
`f` (a setting: 0.7 x the frame width, about a 71 deg horizontal field of view; the CAMERA LENS slider
shows the FOV) and the image centre `(cx, cy)`:

    torso depth     Z_t = f * W_sh / w_px                 (W_sh: your shoulder width, 0.37 m by default)
    shoulder        S = ((u_s - cx) Z_t / f, (v_s - cy) Z_t / f, Z_t)
    hand            H = ((u_h - cx) Z_h / f, (v_h - cy) Z_h / f, Z_h)
    vector          V = H - S                             (camera axes; V.z < 0: hand nearer the camera)

Each point is back-projected at its own depth, so reaching toward the camera (V.z falling) is real
forward motion, not apparent size. The shoulder pixel, the shoulder width and the hand are smoothed
with One Euro filters (the width hardest: the torso hardly moves toward or away while working).

### The mapping

In the operator's axes, as the robot copies them: `forward = -V.z`, `up = -V.y`, `side = V.x`, or
`-V.x` with FACE ME instead of MIRROR ME (the same switch as clutch mode). Then

    target = O + s * (forward, side, up) + (0, 0, lift)

with `O = (0, 0, 0.123)` m, the robot's shoulder (the J2 axis, d1 = 123 mm above the base), and `s` the
ARM SCALE: the robot's reach over your arm's length, shoulder to palm, 0.50 / 0.62 = 0.81 by default.
RAISE TARGETS (`lift`, 0.15 m by default) shifts every target up, for working with the hand below shoulder
height: the robot's shoulder is only 12 cm above the table, so with lift 0 a hand at chest height asks
for the table.

The target is then kept reachable: first its distance from `O` is held between 0.15 m (closer is inside
the robot's own links) and 0.58 m (the arm from J2 to the flange is about 0.63 m fully stretched, where
it is singular), along the same direction (`BodyMath.clampReach`, after the radial mapping in
so101-markerless-teleop). Then it is clamped to the driver's box, `Teleop.MIRROR_WORKSPACE`, the
driver's default `--workspace` x 0.06..0.48, y -0.32..0.32, z 0.08..0.60 m (the floor 8 cm above the
table). The driver clamps servo goals to that box itself, so a box of the app's own would mean nothing;
a driver started with another `--workspace` reports it in `/api/state`, and the app follows that.

Orientation: the tool keeps the orientation it had when you engaged, plus wrist roll (as in clutch
mode). Engaging from outside the clutch workspace (the folded arm), it takes the READY pose's
orientation instead (pointing forward and down), which works across the workspace. The gripper follows
the pinch.

### Engage, approach, follow

- Engaging (button or fist) takes control and starts **APPROACH** ("moving to your hand"): the target
  is sent at 30 % speed and the same goal is re-sent unchanged until the hand moves more than 3 cm, so
  the driver keeps one plan (its planner and Ruckig make the path) instead of re-planning every tick.
  APPROACH ends when the TCP is within 2 cm of the target, or when the TCP has stopped moving (less than
  5 mm in a second, after at least 2 s): then the target can't be reached as asked (the planner says
  `partial` or `unreachable`), and the status says how far short the arm stopped.
- Then the arm **follows**: the target every tick at the normal speed (ARM SPEED, 60 %). Small steps
  are what the driver streams straight to Ruckig.
- Hand or shoulders lost for 300 ms: the arm holds; when they are back, it approaches again.
- A shoulder-to-hand estimate that jumps more than 15 cm in one frame (or the palm 20 deg) is dropped;
  three in a row hold and approach again. (Measured on a recorded clip of fast signing at 10 fps, the
  90th percentile of real frame-to-frame motion was 12.6 cm, so at 20 Hz this passes real moves.)

### Calibration

CALIBRATE ARM: face the camera and hold the arm straight out to the side, level, for 2 s. With the arm
in the plane of the shoulders, the hand is at the torso's depth, and the palm gives that depth in metres
without assuming a body size. So:

    shoulder width  W_sh = w_px * Z_h / f
    arm length      A    = |palm - shoulder|_px * Z_h / f

Only frames with the elbow straight (above 155 deg) and the arm level (within 25 deg) count; after 2 s
of them the medians are taken (as in so101-markerless-teleop's calibration). A result outside 0.40 to
0.95 m (arm) or 0.25 to 0.55 m (shoulders) is rejected. The shoulder width and `A` go to
`~/.config/nervus/hand-teleop.json` (`shoulder_width_m`, `arm_length_m`), and the arm scale becomes
0.50 / A. They are loaded at start-up.

### Defaults

| Setting | Default | |
|---|---|---|
| Control mode | Mirror my arm | |
| Shoulder width | 0.37 m | calibrated |
| Arm scale | 0.81 (0.50 m robot reach / 0.62 m arm) | calibrated |
| Raise targets | 0 | |
| Camera lens | 0.70 x frame width (about 71 deg) | |
| Approach | 30 % speed, re-planned at 3 cm, done at 2 cm | |
| Jump gate | 15 cm or 20 deg per frame | |
| Reach band | 0.15 to 0.58 m from the robot's shoulder | |
| Box | the driver's: x 0.06..0.48, y -0.32..0.32, z 0.08..0.60 m | |

### Limits

- The torso depth assumes the shoulders face the camera: turned 30 deg away, they look 13 % narrower,
  and the torso reads 15 % farther, which adds to the forward reach. Calibrate and work facing the camera.
- The palm depth holds to about 2 % for a 40 to 45 deg tilt on the camera axis. Off the axis, a strongly
  pitched palm reads several per cent long if the model's world landmarks are camera-aligned (they may
  instead be aligned with the view of the hand, which would remove it); the overlay shows the forward
  component, which is where it would appear.
- The robot's reach is about 0.6 m; a fully stretched operator arm maps to 0.50 m by default, so the
  last bit of the robot's reach is only used with a larger arm scale.

## 5. Relative (clutch)

- The arm follows the hand's movement since the anchor, in metres, scaled by the gains: sideways,
  up/down, and toward/away from the camera (reach). Engaging anchors the hand to the arm's current
  pose: nothing moves at the moment of engaging.
- Wrist roll turns the tool about its own axis (off / on / inverted); pinch works the gripper.
- Hand lost for more than 300 ms: the arm holds, and re-anchors when the hand is back. Tracking jumps
  (5 cm or 20 deg in a frame) are dropped; three in a row count as a lost hand.
- At the workspace edge the target is clamped, and the anchor slips so the arm comes back as soon as
  the hand turns back.

## 6. Status and diagnostics

- The status line always says why the arm isn't moving: hand or shoulders lost, fist, palm edge-on,
  tracking jump, at the edge of the workspace (clamped), stopped short of the target (and the driver's
  plan, e.g. `partial`), waiting for the arm's pose, refused by the driver or the kernel (with its
  words: busy, IK guard, stale feedback), outside the workspace.
- Why control was given up (disengaged, move stopped or timed out, lease or driver refused) stays on
  screen for 5 s, in red when it was a failure, instead of being replaced by the next tick's message.
- On the video, in both modes: the hand's vector in cm (mirror: shoulder to hand, FWD / SIDE / UP;
  clutch: the hand's movement since the anchor), the target before and after the clamp (mm, marked
  CLAMPED), the arm's actual TCP and the distance between them, ANCHOR SLIPPING in clutch mode, and the
  driver's note when its planner can't do the target. In mirror mode the shoulder and the line from it
  to the palm are drawn.
- A CSV line per tick (while a hand is in view or the app has control) goes to
  `~/.cache/hand-teleop/session-<date>-<time>.csv`: time, phase, mode, the hand vector, the target
  before and after the clamp, the TCP from the arm's state, and a note (clamped, slip, the driver's
  note, the status). The newest 20 sessions are kept.

## How this is checked

Unit tests, run on the Surface (`./gradlew test`), with no camera and never the real arm or driver:

- the shoulder-to-hand math on a synthetic operator (arm forward, up, sideways, down; 1.2 to 2.5 m from
  the camera; tilted palms; a wrong shoulder width), the operator axes, the target, the reach band and
  the box, the calibration (an arm out sideways; bent or raised arms refused) and the 15 cm gate;
- the gesture classifier on landmark sets (fist, open hand, pinch, four-finger pinch, tilted) and its
  hold timer; the forward kinematics against the arm's measured poses and the settings file;
- the control loop end to end against a fake PiPER driver on an ephemeral loopback port whose joints
  and servo TCP move (`FakePiper`): mirror mode's preview, approach on one goal and following, a jump,
  a lost hand, an unreachable target, a smaller driver box, calibration saved and reloaded, the CSV log;
  clutch mode's outside-workspace prompt, the start-pose move (speed, joint targets, heartbeats,
  convergence, STOP, the 25 s timeout), fist engage / clutch / disengage, and the arm state in words;
- `TeleopUiTest` renders the real screen off-screen and clicks through prompt, move and STOP;
- `ReplayTest` (run with `HAND_TELEOP_REPLAY=<video>`) runs the real pose and hand models over a
  recorded clip and writes the shoulder-to-hand vectors to `build/replay.csv`.

## 7. Future work: full-arm retargeting

Not built. A further mode would map the operator's whole arm onto the PiPER's joints instead of one hand
point: shoulder, elbow and wrist from the body pose model (YOLO pose or a 3-D body model) give the
upper-arm and forearm directions, which map to J1 (yaw of the upper arm), J2/J3 (shoulder and elbow
flexion), and the hand's palm frame to J4 to J6 (the joint-to-body table is in README.md). It needs a
calibration pose (operator's arm along the PiPER's READY pose), joint-limit clamping and rate limits in
joint space, and joint streaming (`/api/stream`, or Manipulator@1 `MoveJointTrajectory` in short chunks)
instead of Cartesian servo. Monocular depth makes the elbow angle the weakest link; a depth camera or a
3-D lifting model would help.
