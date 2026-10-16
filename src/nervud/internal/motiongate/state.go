package motiongate

// State is a state of the top-level Safety state machine:
//
//	NORMAL -> SAFETY_LATCHED -> OEM_RECOVERY (optional) -> REARM_REQUIRED -> NORMAL
//
// Values start at 1, with 0 reserved for StateInvalid, so a zero-value Gate never lands in any state that allows motion;
// epoch==0 is invalid too, so an uninitialised state fails closed twice.
type State uint8

const (
	// StateInvalid is the zero-value sentinel: a Gate not made with New decodes to it. Never a valid running state,
	// and never allowed to send motion.
	StateInvalid State = 0

	// StateNormal is normal running: motion commands may be sent when the epoch and lease are also satisfied.
	StateNormal State = 1

	// StateSafetyLatched: Safety is latched. The ordinary motion Gate is closed, and queued/in-flight commands are voided wholesale by the new
	// epoch. It takes effect atomically at the trigger point, without waiting for Provider/MCU ACKs or mechanical standstill.
	StateSafetyLatched State = 2

	// StateOEMRecovery is the optional OEM recovery phase: still under safety semantics, not Normal;
	// recovery actions can only request allowlisted actions through the dedicated Safety Gate and can't clear the latch.
	StateOEMRecovery State = 3

	// StateRearmRequired: recovery is over or a reset was demanded; waiting for an explicit re-arm before returning to Normal.
	StateRearmRequired State = 4
)

// String returns a stable top-level state name, shared by logs, audits and external state projections.
func (s State) String() string {
	switch s {
	case StateNormal:
		return "NORMAL"
	case StateSafetyLatched:
		return "SAFETY_LATCHED"
	case StateOEMRecovery:
		return "OEM_RECOVERY"
	case StateRearmRequired:
		return "REARM_REQUIRED"
	default:
		return "INVALID"
	}
}
