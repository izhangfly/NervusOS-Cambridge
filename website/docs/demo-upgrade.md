# Browser demo upgrade — 15 November 2026

The demo adds household laundry and toy sorting, two example paid manufacturing apps and selectable PiPER/Panda manufacturer models. The website remains separate from the other VPS services and uses the existing static server and contact endpoint.

## Implementation

App packages describe task goals rather than joint trajectories. `planner.js` generates transfers, lid handling and conveyor operations; `kinematics.js` solves each arm's joint targets. `robots.js` holds manufacturer-specific joint limits, home configurations and SDK/ROS/model links. Switching profiles recompiles the MuJoCo scene and rebuilds the Three.js geometry while preserving the installed packages and local demo licences.

The laundry scene has six flexible garments, three in a basket and three in a table pile. The two routes load the basket or load all six directly from the basket and table. The washer lid is a MuJoCo hinge, opened by a touch-verified grip on its handle. The former anchor remains only for legacy replay and regression tests. Garments use a constrained surface mesh with bending elasticity. Cotton, denim and knit use distinct generated weave textures and bending stiffness. Animal toys have flexible volumetric bodies and procedural head/limb details. Rigid toys and factory parts use simplified collision proxies.

Toy tidy generates seven block shapes (cube, brick, sphere, cylinder, triangle, arch and star), three miniature cars and three soft animals. Scene-feature classification chooses one of three receiving baskets. LineSight QC indexes six housings for pass/rework classification; BearingFit locates bearings in carriers and outfeeds the completed modules. The conveyor adapts a downloaded frame model; its actuated belt transports parts through friction, and its visual rubber surface follows measured belt travel. The wrist camera is a second Three.js renderer attached to the selected robot's wrist; the displayed classification is simulated.

Factory checkout creates a local fake receipt for £49 or £79 and proceeds through the existing permission/install flow. No card information, payment provider or financial transaction is involved. Example publishers illustrate how a manufacturer's production workflow could be distributed.

## Models and licences

| Asset | Source | Licence and adaptation |
| --- | --- | --- |
| PiPER | [AgileX agx_arm_sim](https://github.com/agilexrobotics/agx_arm_sim) | Source README states MIT; original joint frames, inertias and limits, reduced STL visuals and browser servos. |
| Panda | [MuJoCo Menagerie](https://github.com/google-deepmind/mujoco_menagerie/tree/main/franka_emika_panda) | Apache-2.0; original frames, inertias and limits. Visual vertices welded before reduction and grouped by body/material. |
| Top-loader | Procedural geometry | Original cabinet, hollow drum, console and articulated lid/handle. |
| Laundry basket | Procedural geometry | Original slotted sides and rim sized to the collision model. |
| Conveyor | [Josh — Conveyor](https://poly.pizza/m/5f4he3uHyov) | CC BY 3.0; scaled cell, belt animation, sensors and fixtures added. |

The shipped model folders retain provenance and licences; `public/models/props/credits.html` provides attribution accessible from the demo. Toys, cloth meshes/textures, fixtures and camera housing are original procedural elements.

Settings links to [libfranka](https://github.com/frankarobotics/libfranka), [franka_ros](https://github.com/frankarobotics/franka_ros) and the simulation source. These are references, not SDK code included in the browser.

## Verification

`npm run build` builds the production bundle; `npm test` runs 30 tests covering app definitions, household/factory metadata, random scene composition, local receipts, both robot profiles, legacy attachment constraints and worker-cell control regressions, closed-lid clearance and handle opening, cloth draping, the server and mocked contact forwarding.

Browser verification checks cancellation and purchases of both factory apps, permissions, installations, pause/resume/stop/rearm, profile switching with retained licences, seven Panda sliders, persistence, English/Chinese and layouts at 390/768 pixels. Full task runs on both arms check finite physics and actual receiving positions: three or six laundry garments, seven blocks/three cars/three animals, a six-part QC batch and six assembly modules.

Current release checks and deployment status are recorded in [demo-v2-plan.md](demo-v2-plan.md).

## Limits

This demonstrates the common app/adapter contract on two simulated manufacturer models. It does not establish universal real-arm portability. Recognition uses generated scene features; no trained perception policy or physical robot is connected. Rigid grasps use force-limited pad friction and touch checks; cloth pinches use simplified adhesive skins. Flexible meshes are reduced for browser performance and use selective inter-garment contact, without full self-collision. Arm collision meshes are omitted; this is not collision avoidance, press-fit force validation or calibrated textile simulation. Existing evidence of the actual NervusOS prototype remains separate from this browser experience.

The November 2026 fleet/worker redesign and release review are documented in [demo-v2-plan.md](demo-v2-plan.md). The washer and laundry basket are now procedural; their former downloaded models are no longer shipped.
