# NervusOS website preview

A bilingual React/Vite website with separate pages and an independent browser operating-system mockup. The current project evidence is explained separately from simulated apps.

## Run

```sh
npm ci
npm run build
npm start
```

The production server defaults to `127.0.0.1:8910`. Set `HOST=0.0.0.0` for an IP preview. `npm run dev` starts Vite; it proxies `/api` to the production server on port 8910. The production server serves every page route, the static assets and the contact endpoint.

## Pages

`/`, `/platform`, `/demo`, `/progress`, `/awards`, `/investors`, `/developers`, `/contact`.

Use the header language button to switch English and Chinese. The choice persists locally; `?lang=zh` or `?lang=en` selects a language on a shared link.

## Browser apps and robot switching

The App Store contains five apps. Install a free app or complete the clearly labelled demo checkout for a factory app, approve its simulated permissions, open it and choose a task.

- **Laundry transfer**: a downloaded top-loader and laundry basket, three garments in each of the basket and table pile. The arm opens the hinged lid by its handle, then loads individual garments. “Clear table & load all” first collects the desk garments into the basket. Cotton T-shirts, knit jackets and denim trousers have different procedural weave textures and bending stiffness.
- **Toy tidy**: household sorting of a fresh randomized scene with seven block shapes, three cars and three flexible animal toys into category baskets. “Shuffle & sort” creates another arrangement.
- **LineSight QC**: example publisher Northline Automation; £49 demo licence. An indexed conveyor presents six housings to a wrist inspection station; the arm separates pass and rework parts.
- **BearingFit**: example publisher BenchFlow Robotics; £79 demo licence. A different factory workflow takes bearings from a feeder, seats them in carriers and sends the completed modules out on the conveyor.
- **Teach & repeat**: the original two example routes remain available.

In the OS mockup, open **Settings** and choose **AgileX PiPER** (six joints) or **Franka Emika Panda** (seven joints). The manufacturer geometry, joint limits, actuators and IK profile change; installed apps and demo licences remain. Manual control adapts to the selected joint count. SDK, ROS and model links appear in Settings.

The renderer uses Three.js. Official `@mujoco/mujoco` 4.0.1 WebAssembly executes joint dynamics, gravity/contact, the washer hinge and flexible cloth/plush meshes in the browser. A second renderer provides a view from a camera mounted to the wrist. The app definitions contain common task goals; a shared planner maps them through the selected profile's FK/IK and actuators.

**Demo boundaries:** camera classification uses generated scene features, not a trained image model. Pinches and grasps use planned MuJoCo constraints, and conveyor indexing/outfeed are scripted. Cloth uses reduced meshes, bending elasticity and edge constraints, with simplified self-contact; this is not a calibrated textile or washing simulation. Arm collision meshes are omitted, so the preview does not demonstrate collision avoidance or physical hardware task validation. No SDK controls a real robot. Portability is demonstrated on these two simulated profiles; support for every real arm still requires hardware providers and validation. Factory publishers are illustrative; checkout stores a fake receipt locally and never charges or collects card data.

`src/demo/apps/*.js` define the app packages. `scene.js`, `model.js`, `planner.js`, `physics.js`, `view.js` and `Simulator.js` implement scenes, dynamics, planning and rendering. The JSON copies in `public/demo-packages/` match the definitions and are checked by the tests. Models and WASM are served locally, with no third-party runtime asset requests. Installations, licences and robot choice persist only in browser local storage.

See [the upgrade notes](docs/demo-upgrade.md) for architecture, model sources and verification.

## Contact

`POST /api/contact` validates all requested contact fields, requires consent, applies a honeypot and per-address rate limit, and forwards to the fixed recipient set in the service's `CONTACT_TO` environment variable through FormSubmit’s free forwarding endpoint. No mail credentials are stored. FormSubmit requires one inbox activation before forwarding works. Provider acceptance is not a delivery receipt; the UI reports submission, activation and failures without claiming confirmed delivery. The direct `mailto:` link remains available.

Set `CONTACT_TRANSPORT=disabled` to disable forwarding. The tests mock the service so they do not send email. Use real contact details only after the preview has a trusted HTTPS domain.

## Validation

```sh
npm test
npm run build
```

The automated tests check app packaging, purchases, random scenes, both manufacturer models, physical grasp attachment, cloth draping, the static server and mocked contact forwarding. The upgrade notes record browser and full-task verification. Original review files record the initial website review. The Perit styling reference was captured with the requested Perfect Web Clone tool. Its automatic integrity gate failed because a rotating headline changed during stabilization. The adapted site therefore uses manually inspected layout/style evidence and is not a measured pixel-perfect clone. Original Perit business claims, images and certification logos are not used.

## Dependencies and assets

MuJoCo: Apache-2.0. Three.js: MIT. React/Vite/router/Lucide: their distributed package licences. DM Sans: SIL Open Font License. PiPER model: source README states MIT. Panda model: MuJoCo Menagerie, Apache-2.0. Washer, basket and conveyor: CC BY 3.0, with author attribution and adaptations recorded in `public/models/props/credits.html` and `credits.json`. Cloth textures, toy models and fixtures are procedural assets created for this demo. Photos supplied by the founder. NervusOS repository component terms are discussed on the developer page; they are not incorporated into this website’s runtime.

The website and service are independent of all existing VPS applications. Domain and HTTPS setup are intentionally deferred until the owner approves the preview.
