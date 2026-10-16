// Package control manages HUMAN/AI ControlLeases and the deadman, and on lease lifecycle events
// reads or increments the shared motion epoch held by internal/motiongate.Gate. safety and control
// must share the same Gate, or the latch state and the revocation generation could fork.
//
// # Control model
//
// A ControlLease is Resource-scoped: every non-empty stable handle already resolved and verified by the catalog
// has its own exclusive_control slot. control doesn't know type names of the base, the arm or future actuators.
//
//	SAFETY > HUMAN > AI > NONE
//
// This is only the product-level ordering of effective control sources, and its meaning is limited to actuator and motion control;
// it isn't a system-wide permission level, nor a CPU scheduling priority. The four don't share one
// owner enum or Lease implementation:
//
//	SAFETY   an independent Gate/latch (internal/safety), not an ordinary lease
//	HUMAN    an ordinary lease, Class = ClassHuman
//	AI       an ordinary lease, Class = ClassAI
//	NONE     a nil lease; clients can't request it, it's the conclusion that there's no valid lease right now
//
// Concurrency isn't resolved along the priority dimension but along the Resource dimension: while a person drives the base, AI can still control other Resources.
// One connection can hold several Resources at once; pre-emption, renewal and the deadman all act only on the target slot.
//
// # Class policy boundary
//
// control doesn't infer whether the caller is HUMAN or AI; it only consumes the Class passed in from above. Today IPC explicitly lets
// callers choose it and audits the choice, so HUMAN is no extra identity privilege. If this is tightened later, the policy should still live in the IPC/permission layer,
// and this package's Resource slots and pre-emption state machine needn't change.
//
// # Dependency direction
//
// It depends only on motiongate / audit / identity / scheduler and doesn't import safety: safety's
// LeaseRevoker is a consumer-defined narrow interface that *control.Module satisfies implicitly, so wiring is one-way and acyclic
// (main.go builds control first, then injects it into safety.New).
//
// The data plane injects an equally narrow LeaseObserver through SetLeaseObserver, so control doesn't import
// transfer. The end boundary of any lease that took effect is notified exactly once; Safety's RevokeAll only writes each slot's preallocated
// pending position, and the normal-priority drainRevoked notifies later, never calling external code
// on the Safety Supervisor Lane.
//
// Because it imports scheduler (whose sched.go has //go:build linux), the whole package is Linux-only,
// like internal/safety.
//
// # Concurrency model (three hard rules)
//
//	slots      atomic.Pointer[slotIndex]  an immutable slot directory; lock-free reads, copy-on-write when growing
//	cur/fresh  one set of atomic slots per Resource; Check/Refresh allocate nothing and never keep each other alive
//	mu         sync.Mutex                 only serialises control's own ordinary change paths (issue / renew / release / revoke / expire)
//
// 1. RevokeAll never takes mu. It's called by safety's Supervisor Lane (FIFO 90) in beginHalt;
// Go's mutex has no priority inheritance, so waiting for a normal-priority lock holder is priority inversion on FIFO 90.
// It scans one immutable slot snapshot, does an atomic Swap on each cur, and leaves the revoked lease in its slot
// for the Control Lane to account for afterwards.
//
// 2. RevokeAll doesn't increment the epoch. Each boundary may increment only once, and when safety trips
// gate.Trip has already incremented it; incrementing again here would be churn.
//
// 3. The motion epoch is a global monotonic token allocator, but ordinary lease boundaries only revoke the target Resource; other slots keep
// their own tokens. A Safety Trip's epoch is recorded as a global floor, and every lease at or before the floor is invalid.
// Issuing still uses a re-check after publishing (Gate + RevokeAll seqlock), so a concurrent Safety trip can't slip through.
//
// # Control Lane
//
// The module owns one Lane (SCHED_RR / scheduler.PrioControl = 40) that detects deadline and deadman
// expiry. It's below PREEMPT_RT's threaded IRQ at 50, so by internal/scheduler's rules it may
// wait for I/O, and can write audit directly on the Lane (audit's backend is a non-blocking AsyncWriter).
//
// Note this differs from the Safety Stop Lane's (FIFO 95) zero-heap-allocation hard rule; don't apply one to the other. That
// rule targets the emergency stop delivery path, and this Lane isn't on it. What really needs zero allocations is Check/Refresh,
// which are called on every motion command and guarded by AllocsPerRun tests.
//
// # Known v1 trade-offs
//
//   - The kernel has no gate for "may the robot act autonomously". Today AcquireControl itself is only constrained by Safety,
//     the Resource and the lease state machine; Permission is decided again at the dispatch gate of each method.
//     So a caller without method permission can't send commands, but could still occupy a lease slot. Tightening class/session
//     eligibility or a permission for requesting leases is a future IPC/permission policy point and shouldn't sink into Providers.
//   - There's no on-behalf-of attribution: for actions an App triggers through an Agent, the lease holder is the Agent Service.
//     Kernel audit only guarantees the lease lifecycle (the moments and epochs of issue/revocation) can be lined up with the Agent Service's own
//     logs; if the Agent crashes and loses logs, the trail breaks.
package control
