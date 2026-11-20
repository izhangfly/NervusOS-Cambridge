# Demo upgrade

Goal: deploy four cross-arm household/factory apps, physical fabric, wrist rendering, marketplace payment mock and correct model switching to the existing standalone preview.

Architecture: React/Vite + Three.js + MuJoCo 4 WASM. App definitions describe hardware-independent goals; a shared planner maps poses through the selected robot's FK/IK and actuators. Deformable garments attach at a simulated pinch constraint. Rigid toys use a grasp constraint. Render original mesh frames; household props adapt licensed downloadable assets. Payment, permission, install, uninstall states persist locally.

Tasks and files:
1. `public/models/panda/`: retain manufacturer kinematics/inertias, reduced mesh visuals; add SDK/model provenance. Compile and step using MuJoCo WASM.
2. `src/demo/robots.js`, `model.js`, `physics.js`: profile-dependent joint counts, model assets, dynamic scene bodies/flexcomp, named addresses and IK. Test both profiles, every reachable task and constraints.
3. `src/demo/scene.js`, `planner.js`, `apps/*.js`: real prop models, cloth topology/textures, randomized toys, two conveyors, object classification and sequential task generation.
4. `src/demo/Simulator.js`: multi-object rendering/physics, hinge opening, grasp events, wrist camera, counters, pause/stop/reset, robot-switch lifecycle.
5. `src/pages/Demo.jsx`, `src/demo.css`, `public/demo-packages/`: four apps, free household apps, factory purchase/receipt/cancel, same installed packages across arms, bilingual metadata.
6. `tests/`, browser verification script: npm build and tests; browser installs/purchases/denials, full task completions on both arms, finite physics, pause/stop/reset, responsive English/Chinese.
7. Deploy only `dist/` after backup of `/opt/nervus-website-preview/dist`, retain service/contact setup; verify port 8910 health and live task outcomes. Deliver source archive and concise notes.

Commands: `npm run build` (exit 0), `npm test` (0 failures), browser task scripts (all flows + correct physical destinations; captured results in `docs/verification/`). No email test sends real messages.
