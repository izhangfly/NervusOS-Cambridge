package safety

// StopPhase is the actual stop progress phase, orthogonal to the top-level Safety Gate.
//
// SAFETY_LATCHED takes effect atomically at the trigger point; this phase only tracks how far the physical stop has got. Devices without
// trustworthy standstill evidence such as encoders / speed estimation can only cap at OUTPUT_DISABLED, and must not dress up
// "PWM/output at zero" as STANDSTILL_CONFIRMED.
//
// Values must match the StopPhase enum in nervus-ipc's safety.proto (the proto is the single source of truth).
type StopPhase uint8

const (
	PhaseUnspecified StopPhase = 0

	// The normal advancement chain (moves forward monotonically).
	PhaseRequested        StopPhase = 1
	PhaseSent             StopPhase = 2
	PhaseProviderAccepted StopPhase = 3
	PhaseMCUAcked         StopPhase = 4
	PhaseOutputDisabled   StopPhase = 5

	// Reachable only with trustworthy evidence.
	PhaseStandstillConfirmed StopPhase = 6

	// Terminal side branches: enterable from any non-terminal phase, irreversible.
	PhaseDeliveryFault     StopPhase = 7
	PhaseStandstillTimeout StopPhase = 8
)

func (p StopPhase) String() string {
	switch p {
	case PhaseRequested:
		return "REQUESTED"
	case PhaseSent:
		return "SENT"
	case PhaseProviderAccepted:
		return "PROVIDER_ACCEPTED"
	case PhaseMCUAcked:
		return "MCU_ACKED"
	case PhaseOutputDisabled:
		return "OUTPUT_DISABLED"
	case PhaseStandstillConfirmed:
		return "STANDSTILL_CONFIRMED"
	case PhaseDeliveryFault:
		return "DELIVERY_FAULT"
	case PhaseStandstillTimeout:
		return "STANDSTILL_TIMEOUT"
	default:
		return "UNSPECIFIED"
	}
}

// rank gives normal advancement phases a monotonic number; terminal side branches and Unspecified return 0 (handled separately).
func (p StopPhase) rank() int {
	switch p {
	case PhaseRequested:
		return 1
	case PhaseSent:
		return 2
	case PhaseProviderAccepted:
		return 3
	case PhaseMCUAcked:
		return 4
	case PhaseOutputDisabled:
		return 5
	case PhaseStandstillConfirmed:
		return 6
	default:
		return 0
	}
}

func (p StopPhase) isTerminalFault() bool {
	return p == PhaseDeliveryFault || p == PhaseStandstillTimeout
}

// A phase must be published together with the latch round it belongs to, otherwise the previous round's progress could be mistaken for this round's evidence.
//
// Packed into one word (the same layout as internal/motiongate: high 8 bits phase | low 56 bits epoch),
// so one Store by the Supervisor publishes the phase and its ownership together, and readers get a self-consistent
// pair with one Load. Splitting it into two atomics would reopen a tearing window of "new phase with old epoch".
const (
	phaseEpochBits = 56
	phaseEpochMask = (uint64(1) << phaseEpochBits) - 1
)

func packPhase(p StopPhase, epoch uint64) uint64 {
	return uint64(p)<<phaseEpochBits | epoch&phaseEpochMask
}

func unpackPhase(word uint64) (StopPhase, uint64) {
	return StopPhase(word >> phaseEpochBits), word & phaseEpochMask
}

// stopTracker tracks the stop progress of a single actuator Resource (only base.main in v1).
// Only accessed inside the Supervisor goroutine; no synchronisation needed.
type stopTracker struct {
	phase               StopPhase
	standstillSupported bool // from the Safety Contract: without evidence capability, capped at OUTPUT_DISABLED
	terminal            bool
}

// begin resets the tracker to REQUESTED and records whether this device can provide trustworthy standstill evidence.
func (t *stopTracker) begin(standstillSupported bool) {
	t.phase = PhaseRequested
	t.standstillSupported = standstillSupported
	t.terminal = false
}

// advance tries to move the phase forward to to, returning whether a real advance happened. Rules:
//
//   - Already terminal: no more changes.
//   - Terminal side branches (DELIVERY_FAULT / STANDSTILL_TIMEOUT): enterable from any non-terminal phase, irreversible.
//   - STANDSTILL_CONFIRMED: needs standstillSupported; otherwise capped at OUTPUT_DISABLED,
//     refusing a fake standstill without trustworthy evidence.
//   - Other normal phases: may only move forward monotonically (strictly increasing rank).
func (t *stopTracker) advance(to StopPhase) bool {
	if t.terminal {
		return false
	}
	if to.isTerminalFault() {
		t.phase = to
		t.terminal = true
		return true
	}
	if to == PhaseStandstillConfirmed && !t.standstillSupported {
		// No standstill evidence capability: advance at most to OUTPUT_DISABLED.
		if t.phase.rank() < PhaseOutputDisabled.rank() {
			t.phase = PhaseOutputDisabled
			return true
		}
		return false
	}
	if to.rank() > t.phase.rank() {
		t.phase = to
		return true
	}
	return false
}
