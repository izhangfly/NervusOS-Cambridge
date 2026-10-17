# apps/: NervusOS app projects from before the Dell

Apps written for the RDK S100 board (linux-arm64) during the AdventureX 2026 build, before NervusOS moved to the
Dell in hosted mode. They were local folders on the Mac and in no repository until now. Each builds against the
SDK in `src/` (the `includeBuild` paths point there), but none has been rebuilt or run on the Dell yet. The apps
that do run on the Dell are `src/hand-teleop` and the system apps in `src/nervus-system-software`.

| Folder | What it does |
|---|---|
| `alien-wandering-app/` | The AgileX Limo Pro "Alien Wandering" demo ported to NervusOS: it holds a HUMAN lease on the base and streams `BaseMotion.setVelocity` at 20 Hz from an on-screen drive pad (the wander loop is a placeholder). The first `.nspkg` built and signed on the Mac (2026-07-25). See `docs/onboarding/limo/`. |
| `piper-ring-app/` | Drive the PiPER with the Zilo smart ring: ring orientation and motion become `Manipulator@1` poses, a wave plays a preset. Talks BLE to the ring through BlueZ and uses the Kotlin ring SDK in `src/nring`. |
| `nervus-piper-ring/` | The first version of the ring app: a Python sidecar (`ring-sidecar/ring_bridge.py`, using the vendor's Python ring SDK) streams motion intents to a Kotlin app over loopback TCP. Superseded by `piper-ring-app/`. |

Signing keys are not included. Generate a developer key before building:

```bash
mkdir -p keys && openssl genpkey -algorithm ed25519 -out keys/signing-key.pem
```
