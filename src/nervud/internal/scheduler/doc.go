// Package scheduler provides the priority execution Lanes for Safety/Control.
//
// Go goroutines have no settable real-time priority, so a real-time Lane must be bound to an OS thread.
//
// Go has no public notion of goroutine priority, and the Go runtime moves goroutines freely
// between OS threads. The only way to give paths like the emergency stop a Linux real-time scheduling priority
// is:
//  1. runtime.LockOSThread pins the current goroutine to a dedicated OS thread;
//     from then on the Go scheduler puts no other goroutine on that thread and doesn't move it.
//  2. sched_setattr(2) sets a real Linux real-time scheduling policy on that OS thread
//     (SCHED_FIFO / SCHED_RR) and a priority (1..99).
//
// Priority allocation: Emergency Stop 99 / Safety Supervisor 97-98 / Control 40
//
// File layout: the cross-platform parts (types, constants, Lane wiring) are in scheduler.go and spawn.go;
// the part that really calls syscalls is sched.go, with //go:build linux. The package has no non-Linux
// implementation, so the whole package fails to compile on other platforms. That's deliberate: a silent no-op implementation would turn
// "the Safety Lane has real-time priority" into false confidence.
//
// Apart from authority, only this package may import syscall or golang.org/x/sys/unix directly,
// so other modules can't bypass the central scheduling policy and change thread attributes themselves.
package scheduler
