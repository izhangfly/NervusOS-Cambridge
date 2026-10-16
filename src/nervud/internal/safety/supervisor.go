package safety

import (
	"sync/atomic"
	"time"

	"github.com/nervus-os/nervud/internal/motiongate"
)

// supervisor is the state of the Safety Supervisor Lane (FIFO 90): it manages the StopProgress state machine,
// Safety Contract timeouts and escalation, and observation of re-arm.
//
// Every field is read and written only inside the Supervisor goroutine (reconcile/onReport/onTick all run serially
// on that goroutine), so no synchronisation is needed; the only read-only value published outward is phase (atomic, for SafetySnapshot).
type supervisor struct {
	gate     *motiongate.Gate
	ring     *auditRing
	contract Contract
	revoker  LeaseRevoker   // may be nil: incrementing the epoch is already the main revocation mechanism
	phase    *atomic.Uint64 // publishes [current StopPhase | the halt epoch it belongs to]
	pending  *atomic.Uint32 // reads the latest trigger reason (written by Trip)

	active    bool      // whether a stop is currently being tracked
	haltAt    time.Time // when this latch happened (for timeout budgets)
	haltEpoch uint64
	tracker   stopTracker
	esc       escFlags
}

// escFlags records whether each escalation action has fired, so each kind of escalation runs only once per stop.
type escFlags struct {
	acceptTimeout     bool
	deviceAckTimeout  bool
	standstillTimeout bool
}

// reconcile is called after being woken by a Trip / re-arm / recovery request: it aligns the Supervisor's tracking state with the Gate's
// authoritative state. The Gate is the truth; the Supervisor only tracks progress and escalates around it.
func (s *supervisor) reconcile(now time.Time) {
	st, ep := s.gate.Snapshot()
	switch st {
	case motiongate.StateSafetyLatched:
		// A new latch, or the recovery phase re-latched by a new trigger (the epoch changed) -> start a new round of tracking.
		if !s.active || ep != s.haltEpoch {
			s.beginHalt(ep, now)
		}
	case motiongate.StateNormal:
		// Back to NORMAL through an explicit re-arm: stop tracking.
		if s.active {
			s.active = false
			s.setPhase(PhaseUnspecified)
			s.push(evRearm, ReasonUnspecified, PhaseUnspecified, ep)
		}
	case motiongate.StateOEMRecovery, motiongate.StateRearmRequired:
		// Still under safety semantics. Tracking normally starts in the SafetyLatched phase; but if the order of operations
		// completed latch -> (OEMRecovery/RearmRequired) before the Supervisor handled this round,
		// beginHalt never ran. Tracking is added here once, so RevokeAll and the stop audit aren't skipped for the whole round because of the race.
		// The phase starts from Requested (this round's physical progress is unknown, so be conservative), and its re-arm
		// is only allowed once a timeout or Provider evidence has advanced it to a settled phase.
		if !s.active || ep != s.haltEpoch {
			s.beginHalt(ep, now)
		}
	}
}

func (s *supervisor) beginHalt(epoch uint64, now time.Time) {
	s.active = true
	s.haltAt = now
	s.haltEpoch = epoch
	s.esc = escFlags{}
	s.tracker.begin(s.contract.StandstillConfirmationSupported)
	s.setPhase(PhaseRequested)
	s.push(evTripped, ReasonCode(s.pending.Load()), PhaseRequested, epoch)

	// Revoke every actuator lease. Incrementing the epoch (already done by gate.Trip) is the main revocation mechanism; revoker
	// is the increment that cleans up lease objects / sends EndpointRevoked once control lands, and may be nil in v1.
	if s.revoker != nil {
		s.revoker.RevokeAll(epoch)
	}
}

// onReport consumes the Provider's boundary reports and advances the StopProgress state machine. Stale epochs are always ignored.
func (s *supervisor) onReport(r ProviderReport, _ time.Time) {
	if !s.active || r.Epoch != s.haltEpoch {
		return
	}
	switch r.Kind {
	case ReportHaltAccepted:
		if s.tracker.advance(PhaseProviderAccepted) {
			s.setPhase(s.tracker.phase)
			s.push(evStopProgress, ReasonUnspecified, s.tracker.phase, r.Epoch)
		}
	case ReportStopProgress:
		if s.tracker.advance(r.Phase) {
			s.setPhase(s.tracker.phase)
			s.push(evStopProgress, ReasonUnspecified, s.tracker.phase, r.Epoch)
		}
	case ReportStandstillConfirmed:
		if s.tracker.advance(PhaseStandstillConfirmed) {
			s.setPhase(s.tracker.phase)
			// If the device doesn't support standstill evidence, advance caps at OUTPUT_DISABLED; the event code is chosen accordingly.
			kind := evStandstillConfirmed
			if s.tracker.phase != PhaseStandstillConfirmed {
				kind = evStopProgress
			}
			s.push(kind, ReasonUnspecified, s.tracker.phase, r.Epoch)
		}
	case ReportProviderFault:
		if s.tracker.advance(PhaseDeliveryFault) {
			s.setPhase(s.tracker.phase)
			s.push(evDeliveryFault, ReasonProviderFault, s.tracker.phase, r.Epoch)
		}
	}
}

// onTick periodically checks Safety Contract timeouts and escalates. No escalation clears the
// Safety latch or restores old Leases; it only advances tracking and leaves audit, while the actual power cut / alarm is handled by device policy and the MCU.
func (s *supervisor) onTick(now time.Time) {
	if !s.active || s.tracker.terminal {
		// Once a terminal state (DELIVERY_FAULT / STANDSTILL_TIMEOUT) is reached, no more timeout escalation: the terminal phase's
		// rank is 0, and without this guard the accept/deviceAck `rank < X` checks would fire again by mistake after a fault.
		return
	}
	elapsed := now.Sub(s.haltAt)

	// provider_accept_timeout: no HaltAccepted yet.
	if !s.esc.acceptTimeout && s.tracker.phase.rank() < PhaseProviderAccepted.rank() &&
		elapsed > s.contract.ProviderAcceptTimeout {
		s.esc.acceptTimeout = true
		s.push(evProviderAcceptTimeout, ReasonSupervisorEscalation, s.tracker.phase, s.haltEpoch)
		// Mark the Provider/Resource FAULT and stop ordinary Dispatch; left for when resource/service are wired, v1 only audits.
	}

	// device_stop_ack_timeout: not OUTPUT_DISABLED yet.
	if !s.esc.deviceAckTimeout && s.tracker.phase.rank() < PhaseOutputDisabled.rank() &&
		elapsed > s.contract.DeviceStopAckTimeout {
		s.esc.deviceAckTimeout = true
		s.push(evDeviceAckTimeout, ReasonSupervisorEscalation, s.tracker.phase, s.haltEpoch)
	}

	// standstill_timeout: standstill not confirmed yet (or the device has no evidence capability).
	if !s.esc.standstillTimeout && s.tracker.phase.rank() < PhaseStandstillConfirmed.rank() &&
		elapsed > s.contract.StandstillTimeout {
		s.esc.standstillTimeout = true
		s.tracker.advance(PhaseStandstillTimeout) // the Latch stays; enter the timeout terminal state
		s.setPhase(s.tracker.phase)
		s.push(evStandstillTimeout, ReasonSupervisorEscalation, s.tracker.phase, s.haltEpoch)
	}
}

// setPhase publishes the phase and binds it to the latch round currently being tracked (s.haltEpoch).
// For why the binding matters, see Module.Rearm: without it, a standstill phase left by the previous round would become the credential for releasing a new latch.
func (s *supervisor) setPhase(p StopPhase) { s.phase.Store(packPhase(p, s.haltEpoch)) }

func (s *supervisor) push(kind eventKind, reason ReasonCode, phase StopPhase, epoch uint64) {
	s.ring.push(eventRecord{kind: kind, reason: reason, phase: phase, epoch: epoch})
}
