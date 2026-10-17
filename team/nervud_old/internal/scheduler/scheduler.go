// This file is the cross-platform part of scheduler: Policy/priority constants and the Lane type.
// Package documentation is in doc.go.

package scheduler

// Policy is a platform-independent scheduling policy. On Linux it maps to SCHED_FIFO / SCHED_RR /
// SCHED_OTHER; on other platforms these values are only for logging and have no real effect.
type Policy int

const (
	// PolicyNormal: ordinary time-sharing (Linux SCHED_OTHER), for System/Background work
	PolicyNormal Policy = iota
	// PolicyFIFO: real-time first-in first-out (Linux SCHED_FIFO): no rotation at the same priority; runs until it yields.
	// For paths like Emergency Stop and the Safety Supervisor that must run to completion as soon as they start
	PolicyFIFO
	// PolicyRR: real-time round robin (Linux SCHED_RR): equal priorities take turns by time slice.
	// For paths like the Control Lane that need real-time behaviour but can share fairly with their peers
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

// Real-time priority values (the valid Linux SCHED_FIFO/RR range is 1..99, 99 highest).
// The numbers mean "who runs first", not a share of CPU, and there is no priority 100.
//
// Why the ceiling is 95 and not 99:
//
//	On real hardware the kernel's own real-time threads (migration/N, watchdog/N) run at FIFO 99.
//	SCHED_FIFO does not rotate equal priorities (whoever gets the CPU first runs until it yields), so a user-space thread
//	sitting on 99 can block migration behind it, with kernel-level consequences (stop-machine hangs,
//	the lockup detector stops working). The whole of 96..99 is left to the kernel; user space must not use it.
//
// Position relative to threaded IRQs (on PREEMPT_RT, irq/N-* default to FIFO 50):
//
//	a lane above 50 must never wait synchronously for I/O, or it starves the interrupt thread that feeds it
//	(priority inversion). That is why the Stop Lane only sends the fixed stop signal built at startup:
//	write and go, never wait for an acknowledgement. Only lanes below 50 may wait on I/O normally.
//
// Whatever the priority, no lane may spin: priority decides "who runs first", blocking decides
// "for how long", and the latter is what actually starves others. The kernel's default RT throttling allows at most
// 950 ms per 1 s period (/proc/sys/kernel/sched_rt_runtime_us); a runaway lane is
// forcibly stopped for 50 ms, an unacceptable delay on the emergency-stop path.
//
// The values leave gaps, so a new lane can be inserted without renumbering (renumbering priorities is where one place always gets missed).
const (
	// 96..99 are reserved for the kernel's real-time threads; user space must not use them

	PrioSafetyLatch    = 95 // Emergency Stop Lane: the shortest latch / revoke / brake path
	PrioSafety         = 90 // Safety Supervisor: state machine, timeouts and recovery permission
	PrioSafetySupervLo = 85 // reserved: the Supervisor's slow path (no consumer yet)
	PrioWatchdogFeed   = 80 // reserved: feeding the MCU heartbeat watchdog (starving it causes a false brake)
	PrioMotionTick     = 75 // reserved: periodic motion/state updates

	// 74..51 reserved: new lanes that must pre-empt threaded IRQs go in this band
	// 50 = the default priority of PREEMPT_RT threaded IRQs; crossing this line changes the meaning (see above)

	PrioControl   = 40 // Control Lane: dispatching HUMAN/AI control commands
	PrioTelemetry = 30 // reserved: collection/reporting with looser real-time needs

	// 29..1 reserved: other lanes with even looser real-time needs
	// Priority 0 = SCHED_OTHER, used by control-plane modules such as ipc / audit / pkgregistry / service
)

// setSched applies the scheduling policy to the current, already LockOSThread-ed thread. Each platform file defines it:
// sched_linux.go makes the real syscall; sched_other.go is a no-op.
// The build tags make exactly one implementation compile on any platform: its existence is guaranteed at compile time, with no run-time injection.
//
// (This is only an anchor for documentation; the function body is in the platform files.)

// Lane is an execution channel bound to a dedicated OS thread and given a fixed scheduling policy.
// For now only a skeleton: its fields and assembly will be filled in when Safety/Control land.
//
// TODO(rewrite): add a preallocated fixed-length command channel, the fixed stop signal, a writer FD and so on.
// Per items 3 and 6 of sec 6, the Stop Lane hot path must allocate nothing on the heap, checked by a benchmark.
type Lane struct {
	name     string
	policy   Policy
	priority int
}

// applySelf applies this Lane's scheduling policy to the current OS thread, on the condition that the current goroutine
// has already called LockOSThread. The caller must call runtime.LockOSThread() first and must not Unlock,
// or the setting lands on a thread that may later be reused, which is wrong.
func (l *Lane) applySelf() error {
	return setSched(l.policy, l.priority)
}
