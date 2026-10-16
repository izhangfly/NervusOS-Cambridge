// Package operation is the Operation Manager owned by nervud: it gives system-coordinated long tasks that "take time and need cancel /
// progress / terminal confirmation" (arm trajectories, homing, moving to a pose, navigating to a point) a
// state machine and handle held by the kernel.
//
// # Scope (v1 minimal surface)
//
// This package implements the v1 minimal usable surface: an in-memory state machine + the outward operations (Create/Get/
// Cancel/Subscribe) + the Provider reporting seam (ProviderReporter) + non-blocking Safety
// takeover (SafetySupersede). It doesn't do v2's persistence across restarts, complex multi-resource atomic operations,
// or operation_type Registry normalisation. A restart = a new host session, and old operations
// are all dropped, not recovered (consistent with motion epoch semantics; left for v2).
//
// # Four hard rules
//
//  1. nervud owns operations: operation_id, the state machine, caller, resource set,
//     deadline, cancellation, terminal state and audit all belong to this package. Providers don't issue ids or change the origin type,
//     and must not keep controlling resources after the system cancels.
//  2. The terminal state is written once: after SUCCEEDED/FAILED/CANCELLED it can't change (canTransition
//     has no outgoing edges from terminal states + compare-and-set under one lock). PENDING/RUNNING/
//     CANCEL_REQUESTED must not carry a terminal outcome.
//  3. Every enum's *_UNSPECIFIED=0 is an invalid default, and unknown values fail closed.
//  4. Never block Safety: SafetySupersede does one atomic write + one non-blocking delivery and returns;
//     the real state convergence and subscription fan-out happen on this package's own background goroutine.
//
// # Extensible (data-driven, not hard-coded)
//
// Which methods produce an operation is decided by the method metadata in the Method Registry (A3)
// (returns_operation + whether a lease is needed), not by a list of method names in this package. Whether it's
// motion (needs lease checks) is driven by Create's leaseID parameter: leaseID != 0 means motion.
// Today an operation has only one lease binding, so motion operations accept only one Resource; for future
// multi-actuator atomic actions, the wire must first provide repeated (resource, lease, epoch) bindings.
// Self-check: adding a new single-Resource, operation-returning OEM method needs no Go change in this package.
//
// # Dependency direction
//
// This package imports only identity (the Caller type), audit (Recorder/Event) and ipcv1
// (StatusCode). Dependencies on resource/control go through consumer-defined narrow interfaces
// (ResourceValidator/LeaseValidator), with concrete implementations injected by main.go at wiring,
// so the package builds and unit-tests on its own, without waiting on the dispatch wire.
//
// # Wire boundary
//
// Operation state remains local to this package while internal/ipc performs wire translation.
// Keeping that adapter outside the manager prevents protocol representation changes from
// bypassing the authoritative transition and caller-visibility checks here.
package operation
