// Package safety is layer 1 of NSOS's three-layer safety trust boundary: the kernel's stop decision, which can't be bypassed.
//
// # Three trust boundaries (decreasing trust, independent of each other)
//
//  1. The nervud kernel (soft real-time, always present, can't be bypassed)
//     atomically latches SAFETY_LATCHED, increments the motion epoch to revoke in-flight or queued commands, and revokes leases;
//     state machine / timeout escalation / re-arm; the Stop Lane (FIFO 95) + Supervisor (FIFO 90) are both in-process
//  2. The OEM Provider (a system service, replaceable, best effort)
//     how exactly this machine stops: brake / release torque / lie down / cut power / shut off. Only the OEM knows
//     declares safety_contract in its manifest; receives SafetyHalt -> executes -> replies HaltAccepted/StopProgress/...
//     its high priority is granted by nervud, subordinate to the kernel Stop Lane, never the arbiter; OEM code never enters this TCB
//  3. MCU/ESP32 (hard real-time, independent, the Level-0 backstop)
//     a heartbeat watchdog stops locally when the Host or the link fails, and refuses old sessions, sequences and epochs
//
// The authority to decide and revoke stays in layer 1; device actions are carried out by layer 2 on a best-effort basis; layer 3 is the final backstop.
// Whether services are fast, slow or exit doesn't affect the latch and revocation layer 1 has already done; after Linux exits, layer 3 brakes on its own.
//
// # Top-level state machine
//
//	NORMAL -> SAFETY_LATCHED -> OEM_RECOVERY (optional) -> REARM_REQUIRED -> NORMAL
//
// SAFETY_LATCHED takes effect atomically at the trigger point, without waiting for Provider/serial/MCU ACKs or mechanical standstill. Actual stop
// progress is tracked with an orthogonal StopProgress phase (REQUESTED -> ... -> OUTPUT_DISABLED -> [STANDSTILL_CONFIRMED],
// with side branches DELIVERY_FAULT/STANDSTILL_TIMEOUT); devices without trustworthy standstill evidence can only cap at OUTPUT_DISABLED.
//
// # Two RT Lanes
//
//	Stop Lane          FIFO 95 (scheduler.PrioSafetyLatch): the shortest delivery path
//	Safety Supervisor  FIFO 90 (scheduler.PrioSafety): state machine, timeout budgets, escalation, re-arm observation
//
// The 96..99 range is reserved for kernel real-time threads, so user-space Lanes can't starve critical kernel threads.
//
// # The Stop Lane zero-heap-allocation hard rule (a CI gate)
//
// The trigger hot path (Trip -> wake -> deliverHalt) must allocate nothing on the heap after warm-up: channels, fixed commands,
// the audit ring and SafetyPath buffers are all preallocated at startup; no new, no growing append, no closure tasks,
// no log formatting, Protobuf encoding, ordinary RPC/writer queues or slow lock entry. Audit is completed by the normal-priority
// auditDrain reading fixed event codes. trigger_alloc_test.go and bench_test.go
// keep verifying the path allocates nothing, so the emergency stop isn't affected by GC or allocator locks.
//
// The atomic core of the motion epoch and latch is in the leaf package internal/motiongate, shared by safety and control.
package safety
