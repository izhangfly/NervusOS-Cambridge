//go:build linux

package scheduler

import "golang.org/x/sys/unix"

// SCHED_OTHER is the value 0 in the kernel; x/sys/unix has no named constant for it, so it is spelled out here
const schedOther = 0

// setSched sets the scheduling policy and priority of the current OS thread.
//
// It uses sched_setattr(2) (x/sys/unix.SchedSetAttr), the modern superset of sched_setscheduler,
// which expresses policy and priority together in one SchedAttr struct.
//
// Notes:
//
//		pid = 0 means the current thread, i.e. the caller's OS thread.
//		pid = 0 may only be passed after runtime.LockOSThread();
//	    otherwise the current thread is undefined and may be moved by the Go runtime after the call.
//		The real-time priority of SCHED_FIFO / SCHED_RR goes in SchedAttr.Priority (valid range 1..99).
//		SCHED_OTHER uses Priority=0 and needs no real-time priority.
//		Setting a real-time policy needs the CAP_SYS_NICE capability;
//		without it, EPERM is returned to the caller unchanged.
func setSched(policy Policy, priority int) error {
	var attr unix.SchedAttr
	switch policy {
	case PolicyFIFO:
		attr.Policy = unix.SCHED_FIFO
		attr.Priority = uint32(priority)
	case PolicyRR:
		attr.Policy = unix.SCHED_RR
		attr.Priority = uint32(priority)
	default:
		attr.Policy = schedOther
		attr.Priority = 0 // SCHED_OTHER's static priority must be 0
	}
	// flags = 0: applies to the current thread, with no modifier such as RESET_ON_FORK
	return unix.SchedSetAttr(0, &attr, 0)
}
