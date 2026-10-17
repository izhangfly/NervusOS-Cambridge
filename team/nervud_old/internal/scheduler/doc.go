// Package scheduler provides Safety/Control priority execution Lanes.
//
// Background (architecture doc sec 6):
//
//	Go's goroutines have no public notion of priority, and the Go runtime moves goroutines freely between
//	OS threads. The only way to give paths like the emergency stop Linux real-time scheduling priority
//	is:
//	  1. runtime.LockOSThread()  pins the current goroutine to a dedicated OS thread;
//	     the Go scheduler then never puts other goroutines on it or moves this one away
//	  2. sched_setattr(2)        sets a real Linux real-time scheduling policy on that OS thread
//	     (SCHED_FIFO / SCHED_RR) and a priority (1..99)
//
// Priorities: Emergency Stop 99 / Safety Supervisor 97-98 / Control 40
//
// File layout: the cross-platform parts (types, constants, Lane assembly) are in scheduler.go and spawn.go;
// the part that really makes syscalls is sched.go, with //go:build linux. There is no non-Linux
// implementation: on other platforms the package does not compile, deliberately. A silent no-op implementation would turn
// "the Safety Lane has real-time priority" into false confidence.
//
// Dependency boundary: per sec 4, apart from authority, only this package may import syscall /
// golang.org/x/sys/unix
package scheduler
