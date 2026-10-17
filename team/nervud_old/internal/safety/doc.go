// Package safety is the Safety Gate: atomic latching, motion epoch increments, revoking actuator leases,
// StopProgress tracking, escalation and re-arm. Top-level states: NORMAL/SAFETY_LATCHED/OEM_RECOVERY/REARM_REQUIRED.
//
// The Stop Lane hot path must allocate nothing on the heap after warm-up: no new, no growing append, no closure tasks.
// No log formatting, Protobuf encoding or ordinary RPC queues may enter it. It only sends the fixed stop signal built at startup.
package safety
