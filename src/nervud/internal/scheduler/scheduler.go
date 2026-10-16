// This file defines Policy, the priority constants and the Lane type. Actual scheduling calls only exist in the Linux build,
// because a silent downgrade on non-Linux would create the illusion of having real-time priority.

package scheduler

// Policy maps Linux's SCHED_FIFO, SCHED_RR and SCHED_OTHER
type Policy int

const (
	// PolicyNormal is ordinary time-sharing scheduling (Linux SCHED_OTHER). Used by System/Background.
	PolicyNormal Policy = iota
	// PolicyFIFO is real-time first-in first-out (Linux SCHED_FIFO): no rotation at equal priority, runs until it yields.
	// Used for paths like Emergency Stop and Safety Supervisor that must run to completion as soon as they need to run.
	PolicyFIFO
	// PolicyRR is real-time round robin (Linux SCHED_RR): equal priorities take turns by time slice.
	// Used for paths like the Control Lane that need real-time behaviour but allow fair rotation among peers.
	PolicyRR
)

func (p Policy) String() string {
	switch p {
	case PolicyFIFO:
		return "SCHED_FIFO"
	case PolicyRR:
		return "SCHED_RR"
	default:
		return "SCHED_OTHER"
	}
}

// Real-time priority values (Linux SCHED_FIFO/RR valid range 1..99, 99 highest).
// These numbers are scheduling order, not CPU share, and there is no priority 100.
//
// Why the ceiling is 95 and not 99:
//
// On real hardware the kernel's own real-time threads (migration/N, watchdog/N) run at FIFO 99.
// SCHED_FIFO doesn't rotate at equal priority (whoever gets the CPU first runs until it yields), so user space
// holding 99 can block migration, and the consequences are kernel-level (stop-machine hangs,
// the lockup detector stops working). The whole range 96..99 is left to the kernel; user space must not use it.
//
// Position relative to threaded IRQs (irq/N-* defaults to FIFO 50 on PREEMPT_RT):
//
// A lane above 50 must never wait synchronously for any I/O to complete, or it starves the interrupt
// thread that feeds it data (priority inversion). That's why the Stop Lane only posts a fixed stop signal built at startup,
// and leaves as soon as it's written, without waiting for confirmation. Only lanes below 50 may wait on I/O normally.
//
// Whatever the priority, a lane must never spin: priority decides who runs first, blocking decides
// how long it runs, and the latter is what actually starves others. The kernel's default RT throttling allows at most
// 950ms per 1s period (/proc/sys/kernel/sched_rt_runtime_us), and a runaway lane gets
// forcibly stopped for 50ms, which is an unacceptable delay for the emergency stop path.
//
// The numbers leave gaps, so a new lane can be inserted without renumbering existing values (renumbering priorities makes it easy to miss one).
const (
	// 96..99 are reserved for kernel real-time threads; user space must not use them

	PrioSafetyLatch    = 95 // Emergency Stop Lane: the shortest latch / revoke / brake path
	PrioSafety         = 90 // Safety Supervisor: state machine, timeouts and recovery permission
	PrioSafetySupervLo = 85 // reserved: Supervisor slow path (no consumer yet)
	PrioWatchdogFeed   = 80 // reserved: MCU heartbeat watchdog feed (starving it causes false brakes)
	PrioMotionTick     = 75 // reserved: periodic motion/state updates

	// 74..51 reserved: new lanes that must pre-empt threaded IRQs go in this range
	// 50 = the PREEMPT_RT threaded IRQ default priority; crossing it changes the semantics (see above)

	PrioControl   = 40 // Control Lane: dispatching HUMAN/AI control commands
	PrioTelemetry = 30 // reserved: collection/reporting with looser real-time needs

	// 29..1 reserved: other lanes with even looser real-time needs
	// Priority 0 = SCHED_OTHER, used by control-plane modules such as ipc / audit / pkgregistry / service
)

// setSched applies the scheduling policy to the current thread, which has already called LockOSThread. The implementation is in sched.go,
// which carries the Linux build tag; there is no non-Linux no-op, so the Safety Lane can't keep running without a real-time guarantee

// Lane is an execution channel bound to a dedicated OS thread and given a fixed scheduling policy.
// For now it's only a skeleton: fields and wiring will be completed when Safety/Control land.
//
// TODO(rewrite): add a preallocated fixed-length command channel, a fixed stop signal, writer FDs, etc.
// The Stop Lane hot path must allocate nothing on the heap, checked by a benchmark, so GC can't delay the emergency stop.
type Lane struct {
	name     string
	policy   Policy
	priority int
}

// applySelf applies this Lane's scheduling policy to the current OS thread, assuming the current goroutine has
// already called LockOSThread. The caller must call runtime.LockOSThread first and must not Unlock,
// otherwise the setting lands on a thread that may later be reused, which is wrong.
func (l *Lane) applySelf() error {
	return setSched(l.policy, l.priority)
}
