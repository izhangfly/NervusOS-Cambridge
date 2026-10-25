# Hand Teleop

Drive the AgileX PiPER with your hand in front of a webcam: YOLO26 pose and MediaPipe hand landmarks
on the CPU, the arm through NervusOS (Manipulator@1) or, with `--standalone`, straight through the
PiPER driver on loopback. [SPEC.md](SPEC.md) defines what it does; this page is the orientation.

## Modes

- **Mirror my arm** (default): your hand relative to your shoulder, scaled, becomes the TCP relative to
  the robot's shoulder (J2). Reach toward the camera and the arm reaches forward; raise your hand and it
  rises; move it to the side and it swings. Calibrate once (CALIBRATE ARM, arm straight out sideways).
- **Relative (clutch)**: the arm follows your hand's movement from where you engaged.

Engage with the button, Space, or a fist held 1.5 s; the same fist disengages. Pinch works the gripper;
wrist roll turns the tool.

## Your arm and the PiPER

From the PiPER user manual. The robot's "shoulder" is the J2 axis, d1 = 123 mm above the base, which is
mirror mode's origin.

| Joint | Robot motion | Your body | Range (manual) | Max speed |
|---|---|---|---|---|
| J1 | base yaw | shoulder swing, left / right | -154..154 deg | 180 deg/s |
| J2 | shoulder lift | shoulder raise | 0..195 deg | 195 deg/s |
| J3 | elbow | elbow bend | -175..0 deg | 180 deg/s |
| J4 | forearm roll | forearm rotation | -102..102 deg | 225 deg/s |
| J5 | wrist bend | wrist flexion | -75..75 deg | 225 deg/s |
| J6 | wrist roll | wrist roll | -120..120 deg | 225 deg/s |

Modified DH (mm): d1 = 123, a3 = 285.03, d4 = 250.75, a4 = -21.98, d6 = 91; working radius 626.75 mm;
the TCP at joint zero is (56.1, 0, 213.3) mm. The driver enforces the stricter limits of the SDK (J4
+-106, J6 +-100), and its own workspace box for servo targets.

Mirror mode drives the TCP, not the joints: the driver's planner and IK choose the joints. Mapping your
joints onto the robot's one by one is future work (SPEC.md, section 7).

## Running

```sh
./gradlew test createDistributable
build/compose/binaries/main/app/hand-teleop/bin/hand-teleop --standalone            # Surface Camera, driver at 127.0.0.1:8770
build/compose/binaries/main/app/hand-teleop/bin/hand-teleop --standalone --camera /dev/video60
```

Every session's ticks are logged to `~/.cache/hand-teleop/session-*.csv`; the start pose and the arm
calibration are kept in `~/.config/nervus/hand-teleop.json`.

`HAND_TELEOP_REPLAY=clip.mp4 ./gradlew test --tests '*ReplayTest'` runs the real models over a recorded
clip (no arm) and writes the shoulder-to-hand vectors to `build/replay.csv`.
