# Contact physics and cooperative applications

The owner reports garments intersecting and tangling during laundry transfer, Panda self-intersection
in sorting, and wants four distinct arm profiles plus two genuine two-arm applications.

## Selected approach

- Keep MuJoCo in workers. Garment-to-garment contact remains enabled in every garment state; sleeping
  reduces motion without removing collision geometry. Enable cloth self-contact, use smaller fixed
  physics steps and non-overlapping starting layers. Bound worker step batches for responsive controls.
- Add Panda link collision proxies and reject self-intersecting IK branches and interpolated paths.
  Use smooth acceleration/deceleration and conservative recovery rather than moving through the arm.
- Generate six LineSight parts with two to four pass parts and two to four rework parts. Shuffle once
  per seed, derive their appearance from classification, and share the batch across the fleet.
- Add Universal Robots UR5e and Kinova Gen3, using attributed MuJoCo Menagerie geometry and a simulated
  parallel gripper. Four independent workcells compare the existing single-arm apps.
- CarryTogether: two arms grip a tray at opposite handles, confirm contact, lift and carry together,
  and release onto another support. This demonstrates synchronized capability calls.
- FixtureMate: one arm holds a housing while the other picks and places an insert, then both release
  and verify the assembly. This demonstrates role assignment, shared-workspace control and barriers.
- Cooperative apps run two arm instances in one MuJoCo world, with shared objects and contact forces.
  The pair selector supports identical and different profiles. They do not use two independent copies
  of the object, or claim production hardware compatibility.

Alternatives considered: cloth folding would compound the reported textile failure; a simple
handover would exercise less synchronized motion than tray carrying. The selected tasks cover both
simultaneous manipulation and separate roles while reusing the existing capability contracts.

## Verification

Seeded batch checks, model compilation and joint/actuator address checks for all profiles, cloth
layer separation/contact checks, full laundry and sorting runs, collision checks along Panda paths,
and cooperative completion/contact/state checks for matching and mixed pairs. Build and browser
checks in Chromium and mobile WebKit precede deployment. Keep old immutable assets available.
