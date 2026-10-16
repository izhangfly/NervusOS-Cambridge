package safety

// SafetyPath is the boundary across which the kernel Stop Lane delivers high-priority stop requests to the OEM Provider:
// the seam for the fixed stop signal.
//
// Implementation constraints (to follow when the real eventfd/UDS/hardware FD lands):
//   - Preallocated: the connection, buffers and the fixed signal are ready before the Provider is READY, and SendHalt must not allocate again.
//   - Non-blocking, zero heap allocations: return once written, without waiting for an ACK; when blocked by a slow consumer give up at once rather than queue.
//   - Only deliver the preconstructed fixed signal; device-specific stop actions are the job of the Provider's local high-priority path.
//
// Whether or not this path succeeds, the MCU motion watchdog, the physical emergency stop and independent cut-offs remain the Level-0 backstop
//
//	().
type SafetyPath interface {
	SendHalt(epoch uint64, reason ReasonCode) error
}

// NopPath is the preallocated no-op implementation for dev / v1 without a real Provider:
// it delivers nothing, allocates nothing and always succeeds. Real platform delivery lands separately in build-tagged files.
func NopPath() SafetyPath { return nopPath{} }

type nopPath struct{}

func (nopPath) SendHalt(uint64, ReasonCode) error { return nil }
