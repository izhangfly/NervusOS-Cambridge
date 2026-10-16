// Package motiongate is the shared atomic core of Safety/motion: one packed atomic.Uint64
// expresses both the top-level Safety state and the motion epoch (the revocation generation for actuator commands).
//
// # Packed layout
//
// A single 64-bit word, so one atomic read gives a consistent (state, epoch):
//
//	bit 63.......... 56 | bit 55................. 0
//	 8-bit State | 56-bit epoch
//
// Why one word: a Safety trigger must latch the state and increment the epoch atomically in one step.
// Two separate atomics could neither be read as a consistent snapshot nor
// latch and bump in one step. Packed, Trip is one CAS and Snapshot one Load.
//
// # Dependency direction
//
// This package is a leaf and must not import any sibling nervud module. safety and control share the same
// *Gate instance (New is called once by main.go during wiring and injected into both); it's part of the trusted computing base (TCB).
// So there's no ownership dispute: nobody owns the epoch. In Normal it's where control gets monotonic tokens
// for each Resource; tokens produced by a Safety Trip are the global revocation floor across Resources.
// Multi-resource callers can't use "the lease token must equal the current Gate token" as an ordinary validity check.
//
// # Lifecycle and fail-closed
//
// It must be created with New. New sets State=Normal, epoch=1:
//   - the epoch is never zero (initialised non-zero). epoch==0 is always an invalid generation,
//     and actuators/adapters refuse to send anything when they see epoch==0.
//   - the zero-value Gate (word==0) decodes to (StateInvalid, 0): the state isn't Normal and the epoch is invalid,
//     failing closed twice, so an uninitialised or zeroed Gate can't be mistaken for one that allows motion.
//
// # Concurrency
//
// Every method is lock-free with zero heap allocations (Trip/BumpEpoch/Snapshot are guarded by AllocsPerRun allocation tests).
// The hot path (a Safety trigger) only calls Trip, one non-blocking, non-allocating CAS.
package motiongate
