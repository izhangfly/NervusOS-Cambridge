# nervus.safety.recovery

The Safety recovery policy service. **Mechanism in the kernel, policy here.**

```text
safetyrecoveryd  ──IPC──▶  nervud's built-in endpoint
                            nervus.interface.safety.control
```

## Where it's installed

```text
/usr/lib/nervus/system-packages/nervus.safety.recovery/
├── manifest.json
├── manifest.sig
└── bin/safetyrecoveryd
```

## The opposite of pkgmanagerd: a Client, not a Provider

pkgmanagerd calls `RegisterEndpoint` and waits for nervud to forward Dispatches to it.
This service calls `ResolveEndpoint` and then `Request`: it's a **consumer**.

The interface it consumes is a **built-in endpoint**, implemented by nervud itself rather than by any
Package. It's called exactly like an interface any Provider offers, and this service doesn't know or
need to know that the other end is the kernel.

## The kernel keeps only one hard precondition

The one thing `safety.Rearm()` re-checks: **stop progress must have settled**, meaning either

- the normal chain reached `OUTPUT_DISABLED` / `STANDSTILL_CONFIRMED` (output confirmed off), or
- it entered a terminal side branch such as `DELIVERY_FAULT` / `STANDSTILL_TIMEOUT` that needs a human.

Releasing the latch while stopping is still under way (`REQUESTED` / `SENT` / `PROVIDER_ACCEPTED` /
`MCU_ACKED`) would let motion resume **without knowing whether output is off**.

"Should we re-arm now" isn't in the kernel: it depends on device state, the cause of the fault,
whether a user is present, and whether the condition that triggered the stop has been cleared. That
will change as the product evolves, and putting it in the kernel would make "change the recovery
policy" and "change the kernel" the same thing.

## What it does now: observe

It reads the safety state periodically and logs transitions. The moment it enters
`REARM_REQUIRED` and **the stop phase at that time** are logged at WARN: first-hand material for
reviewing afterwards "why did it stop, and how far did stopping get".

A change only counts if one of three fields differs. `motion_epoch` changing on its own
(pre-emption, a Provider restart) doesn't change the top-level state, but it means **every motion
command in flight has been voided**, an event that must be recorded.

### Why polling instead of subscribing

The subscription channel works, but the built-in `nervus.interface.safety.control` declares no
events, and subscribing to an event_id that isn't in the contract is refused.

The 2-second period is a trade-off: safety transitions are rare (they don't change at all in normal
running), so faster polling only burns CPU and audit entries; but after a stop, ops people watch the
UI for the update, and too slow looks unresponsive.

## What it does [not] do yet: automatic re-arm

**There's no logic to clear a stop automatically, deliberately.**

Automatic re-arm first has to answer "has the condition that triggered the stop gone away", which
depends on Providers reporting device state, and `safety.proto`'s five messages (`SafetyHalt` /
`HaltAccepted` / `StopProgress` / `StandstillConfirmed` / `ProviderFault`) currently **have no
channel to carry them**: the kernel's delivery side is `NopPath` and its report side `NopReports`.

Writing a "wait N seconds, then re-arm" policy before the real device state is available would let a
timer decide when the machine may move again. **That's more dangerous than doing nothing**: the
system would look like it can recover, while that ability rests on an assumption unrelated to reality.

### Order of work

1. [ ] The kernel settles how the Safety Path is carried (a dedicated high-priority channel or
       Dispatch; `safety.proto` marks it "to be frozen")
2. [ ] The kernel replaces `NopPath` / `NopReports` with real implementations
3. [ ] This service implements a recovery policy from the device state Providers report
4. [ ] `BuiltinHandler` gains a signature that passes through a typed error_detail, so
       `STOP_NOT_SETTLED` (wait a bit) and `WRONG_STATE` (stop trying) can be told apart

Item 4 affects this service already: when re-arm fails it only sees `FAILED_PRECONDITION` and can't
tell whether to retry or give up. The kernel already works out the reason; it just can't pass it on
(see `TODO(builtin-detail)` in `nervud/internal/safety/builtin.go`).

## Permissions

The manifest asks for two:

| Permission | Trust floor | Use |
|---|---|---|
| `perm.safety.observe` | OEM | Read the safety state |
| `perm.safety.rearm` | Platform + `platform-release` signature | Clear the stop latch |

The second is **the riskiest permission in the system**: misusing a motion permission makes the
machine move once, misusing this one switches off the whole safety system. That's why it sits at the
same level as `perm.authority.reboot`.

**Once enforcement is back** (the kernel's `permission.V1GrantAll = false`), this package must really
have a platform-release signature to resolve the interface. On a dev machine that fails with
`PERMISSION_DENIED`, which is expected, not a regression.
