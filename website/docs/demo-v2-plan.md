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
| 3 | Conveyor beside the arm; one belt position moves every part and the belt surface | done (actuated belt and friction transport) |
| 4 | Robot contact: finger pads, palm, touch sensors, elliptic friction cones | done (both models; PiPER finger coupling stiffened) |
| 5 | IK with gripper twist (yaw), so the fingers close across two parallel faces | done (`inverseKinematics(p,target,seed,yaw)`, ~2 ms) |
| 6 | Physical pick and place: squeeze with limited force, check touch sensors, retry, no weld joints | done: rigid friction grasps; simplified adhesive cloth pinches |
| 7 | Physical conveyor (driven belt body, friction carries parts), carriers with a bearing pocket | done |
| 8 | Physical cloth pinch and washer lid opened by gripping its handle | done; laundry layout and garment states reworked (below) |
| 9 | Fleet view: PiPER and Panda workcells in one scene, install once, run on both, capability trace | done |
| 10 | Rebuild for ianzhyy.com and the NervusOS VPS site, verify in Chromium and WebKit | done, deployed 21 November 2026 |

## Decisions and measurements

- Physics cost per 4 ms step, laundry scene, before any change: 27.7 ms (969 DoF). Garment-garment contact
  was the cost: 7x7-grid garments 15.6 ms with it, 5.5 ms without; 6x6 grid without it 3.4 ms. Garments now
  use selective contact states, described below, to keep browser cost bounded.
- Originally, both robot models shipped visual-only geometry (all `contype="0"`). Finger axes: PiPER fingers close along
  the tool site's y axis (link6 frame), fingertips at the tool site; Panda follows MuJoCo Menagerie (pads
  `size .0085 .004 .0085` at `0 .0055 .0445` in each finger frame, closing along the hand's y axis).
- Collision groups: world and rigid parts `1/1`, finger pads and palm `8/1`, grip skin `0/16`;
  garment states select the contact masks described below.
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
  - sleep (2/3): nobody is handling it and it lies still; gravity compensated, damped, with world and garment contact;
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

## Release review (21 November 2026)

- Physics and planning live in `Cell` and `cell.worker.js`; `Simulator` draws worker snapshots. The page no
  longer executes MuJoCo steps. Independent workers let the browser schedule the two simulations; no
  particular CPU core is promised.
- Fleet status stays active until both robots finish. Pause/resume sends an explicit state to each cell.
  Stop remains latched across app/view changes. Cancellation clears cloth adhesion and holds joint targets.
- Scene revisions reject old worker updates. A current manifest is required before enabling controls.
  Changing between one arm and fleet cancels the task and rebuilds the requested workcells; rapid changes
  cannot recreate an unwanted cell. Disposal terminates workers and resolves pending development requests.
- Redraws are capped at 30 Hz while running, and idle cells sleep once motion settles. Hidden tabs do not
  step physics or render. Fine garment geometry updates only for a new snapshot; wrist rendering also
  requires a new frame. Cloth outlines now clip triangles at sleeve/collar/leg boundaries rather than
  keeping staircase edges. The surface has front/back faces and a connecting rim.
- Production builds omit window inspection hooks and the worker evaluation handler. Settings explains
  focus versus robot replacement. Storage failures do not crash the desktop.
- Fresh Node verification: 30 tests passed. Production WebKit at iPhone width completed basket laundry on
  both arms (3 garments each), with pause/resume, stop/re-arm, fleet/one-arm and page disposal checks.
  Factory and final deployment checks are recorded after completion below.

Simulation boundaries remain: selective garment contact, simplified adhesive cloth pinches, reduced
physics meshes, and incomplete arm collision geometry. The demo is not hardware task validation.

- Full laundry review found an avoidable failure: collecting the desk pile into the basket restacked six
  garments, which created tangles. The task now loads from each existing pile directly, one at a time.
  Starting the next job always clears the previous cloth grip state. A deterministic PiPER simulation
  loaded all six after this change; final production browser validation follows.
- Lifecycle checks passed: rapid fleet toggles, rapid app changes, stop persistence, pause with no time
  advance, cancellation with zero adhesion, zero idle redraws in a one-second sample, no time advance in
  the visibility-guard test, and disposal with no workers or outstanding inspection promises.

- The final drum audit recovers requested garments left by skipped touch checks. Failed picks defer to
  other garments, so a difficult fold cannot starve the table load. Panda pinches the selected fold
  higher to clear its wider palm from the basket; cloth approach uses a narrower opening. Recovery is
  bounded, and the final count is measured from the drum. Simulation checks with both profiles loaded
  six garments; production browser checks are repeated after these changes.

- Garment settling runs after each fixed physics step, independent of browser frame batching. An extra
  Panda motion slowdown was removed after touch diagnostics showed it destabilized the cloth grasp.
- The washer cabinet is hollow: four walls surround the drum rather than a solid render box hiding the
  loaded clothes. On phones, the wrist camera is compact and placed below the fleet controls.
- Production WebKit at 393 px completed full laundry on both arms: 6 garments each, no page errors or
  failed requests, no horizontal overflow. Final factory/layout and deployment checks follow.

- Sorting review: toy cylinders now fit within both grippers' release opening; plush toys use
  tetrahedral volume elasticity rather than collapsible surface shells. Scatter spacing and receiver
  positions keep the batch within both robots' workspace. Separate basket slots prevent all items
  being forced into one pile. Release opens the fingers fully.
- Toy tidy and LineSight results audit actual final receiving positions. Historical per-job tallies
  cannot count an item that is still hovering, in the wrong basket or outside the scene. QC also uses
  separate receiving slots, with a second layer for batches beyond four items in one channel.
- Deterministic Toy tidy checks: PiPER (seed 42) and Panda (seed 43) placed all 13 toys in their receiving baskets.
  Panda recovered one rim-caught block through a new physical pick. The close-to-base IK fallback
  tries inward and outward wrist lean after the normal level approach fails. Sorting recovery is bounded.

- Final local release verification: `npm test` passed all 30 tests and `npm run build` completed.
  The production Chromium desktop check sorted all 13 toys on both profiles. Production WebKit at
  iPhone width inspected six parts on each profile, with no page errors, failed requests, horizontal
  overflow or overlapping fleet controls/wrist camera. Both checks also exercised pause/resume,
  stop/re-arm, switching between fleet and one arm, and leaving the page to dispose workers.

- Live checks found and corrected two final regressions. Panda's 3 mm finger stop left a gap wider
  than the flat fabric contact thickness; the 2 mm stop retains the cloth through wrist turns.
  The live WebKit phone run then loaded three garments on each arm, with measured drum counts.
  Pause now treats a pending worker start as running, matching the interface while reset messages
  briefly report idle. A regression test covers that transition.
- Final verification: all 31 Node tests passed and both production builds completed. Published
  WebKit phone and Chromium desktop checks passed pause/resume, stop/re-arm, fleet switching,
  replay and page disposal, with no page errors or failed requests.
- Both sites were deployed on 21 November 2026. The standalone deployment verified 132 file hashes,
  retained old hashed assets and checked the contact service health. The portfolio release uses
  `assets-20261121182510`; its files match the local build and earlier immutable asset folders remain
  available to open tabs. Source and release notes are pushed to NervusOS-Cambridge.

## Layout and controls release (22 November 2026)

- Fleet view opens by default even when an older browser preference selected one arm. The fleet wrist
  camera starts hidden; its button toggles it without rebuilding the workcells.
- The capability trace is below the canvas in both views. The old tool-position/joints/state strip is
  removed, and the orbit hint stays inside the scene. The stop button reads E-STOP. Both model sources
  are linked.
- Fullscreen uses the element API when supported and an expanded viewport panel otherwise. Exit and
  Escape restore normal layout and body scrolling without replacing the simulator.
- The homepage uses the new founder-supplied photo. The separate Awards photo remains unchanged.
- Build and all 31 Node tests passed. Local production Chromium desktop and WebKit phone checks passed.
  Published checks passed replay on both arms, default fleet, hidden/toggleable wrist camera, trace
  placement, pause/resume, E-STOP/re-arm, view changes, page disposal and fullscreen entry/exit. The
  phone check exercised the fullscreen fallback; Chromium exercised native fullscreen. Neither run
  reported page errors, failed requests or horizontal overflow.
- Uploaded both sites: portfolio release `assets-20261122061713`; standalone deployment verified 133
  file checksums and contact-service health. Previous hashed assets are retained. Production HTML
  matches the local release. Desktop and phone photo checks confirmed the refreshed homepage image.

## Four-arm and cooperative upgrade (25 November 2026)

- Laundry uses 2 ms physics steps, 50 solver iterations, persistent contact between all cloth layers and `selfcollide=auto`. Starting layers are 12 mm apart. Fine rendering stays within the coarse triangles, removing spline overshoot. Contact isolation selects one soft item for gripping while other items retain contact with their surroundings.
- The arm still opens the washer lid through the handle. A simulated mechanical hinge detent engages only after that physical opening; reset releases it. Cloth placement reads the held garment centre before lowering.
- Panda and UR5e use self-contact proxies, IK clearance inequalities and sampled joint paths. New profiles use Cartesian carry paths and bounded carry speed. Bearing placement can re-index and retry a missed physical insertion. Final task status reports incomplete placement as an error.
- Soft toys have a padded rigid core and a deformable tetrahedral outer shell. Assisted contact pads and limited compression prevent inversion and unstable volume collapse. This remains a reduced soft-body model.
- LineSight creates six parts with a random count (2–4) and sequence for each finish. Classification and colours derive from the same generated finish.
- Added the six-joint UR5e and seven-joint Kinova Gen3 using their Menagerie model descriptions, meshes and licences. Model frames include calibrated fingertip offsets. Both implicit OBJ mesh names and explicit STL names render.
- Default fleet view compares four independent workcells. Cooperative apps use two namespaced providers in one MuJoCo world, with selectable matching or mixed geometry. CarryTogether confirms both contact grips before moving a shared tray. FixtureMate reserves the insertion area while one arm steadies a housing and the partner places an insert. The coordinator enforces ownership, support pose and grip barriers.
- Worker stepping is budgeted at 6 ms per cell in the four-arm view, 12 ms for single/pair views. Hidden tabs do not step or draw; idle scenes sleep.
- Physics replay verification: all 64 ordered cooperative pair/action replays passed, covering both actions of both apps on every matching/mixed pair. Factory and teach tasks passed on all four profiles. Final laundry audits counted six garments and Toy tidy counted all 13 toys on each arm; bounded recovery picks were needed in some runs. Panda sorting reported no sampled self-intersection.
- Fresh builds completed for standalone and portfolio hosting; all 41 Node tests passed, including full-model versus physics-only UR5e inertias/contact equivalence. UR5e visual source meshes were reduced from 31.0 MB to 2.2 MB of binary STL without changing inertias or contact proxies. Browser and deployment verification are recorded below.

- Production Chromium desktop completed six QC parts on all four arms with no page errors or failed requests. The check exercised camera visibility, native fullscreen, pause/resume, E-STOP/re-arm, fleet/one-arm switching and disposal. After seam-welding the reduced UR5e visual meshes, a fresh screenshot confirmed clean rendering and inertias/contact equivalence tests passed again.

- Production WebKit at iPhone width completed the six-part QC batch on all four arms, with no page errors, failed requests or horizontal overflow. It also verified expanded-viewport fullscreen, camera visibility, pause/resume, E-STOP/re-arm, fleet/one-arm switching and disposal.
- Deployed both sites on 25 November 2026. Standalone deployment verified 190 file hashes, retained old hashed assets and confirmed the existing contact service healthy and unchanged. Portfolio release `assets-20261125133425` verified 141 hashes; the public demo HTML matches the local entry point. Prior immutable asset directories were retained. The published cooperative task check passed, as recorded below.

- Published WebKit/iPhone verification completed FixtureMate with UR5e and Gen3, including pause/resume, E-STOP/re-arm and disposal. The physical insert reached its housing; the trace remained below the scene and the document width matched the 393 px viewport. No page errors or failed requests occurred after waiting for the default fleet load before switching worker configurations.
