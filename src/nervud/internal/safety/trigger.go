package safety

// Trip is the Safety trigger entry point (zero heap allocations, lock-free). It can be called from any goroutine: the future
// IPC Safety method handler, Controller.RequestStop (HUMAN/UI emergency stop), Supervisor escalation,
// the OEM Safety Gate, and tests.
//
// Three steps:
//  1. gate.Trip: atomically latch SAFETY_LATCHED and increment the motion epoch. The latch takes effect as soon as this call
//     returns, without depending on any Lane scheduling; queued/in-flight commands are voided by the new epoch at once.
//  2. Record the latest trigger reason (coalescable: several concurrent triggers merge into the same latch).
//  3. Wake the Stop Lane (for RT-priority delivery) and the Supervisor (to start tracking and timing) without blocking.
//
// Even if the Stop Lane/Supervisor don't wake for some reason, the latch has already taken effect, adapters refuse by epoch, and the MCU heartbeat
// remains the Level-0 backstop.
func (m *Module) Trip(reason ReasonCode) {
	m.gate.Trip()
	m.pendingReason.Store(uint32(reason))
	wake(m.wake)
	wake(m.superWake)
}

// RequestStop implements Controller: an authorised software emergency stop is equivalent to a Trip.
func (m *Module) RequestStop(reason ReasonCode) { m.Trip(reason) }

// wake posts one wake-up on the capacity-1 signal channel without blocking. When the channel is full (a wake-up is already pending)
// it takes default, so multiple wake-ups merge naturally into one. Zero heap allocations.
func wake(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}
