// This file wraps Safety's observation and recovery capabilities as the handler of a built-in endpoint,
// which nervud registers during wiring (endpoint.RegisterBuiltin).
//
// # Why it's in the safety package and not main.go
//
// It has to translate motiongate.State -> wire TopState and StopPhase -> wire StopPhase,
// and the semantics of both enums belong to safety. Putting it in main.go would make wiring code carry
// semantic mapping, which is exactly where a new state is most easily missed.
//
// # It doesn't provide Trip
//
// Software emergency stops are triggered by other paths (a Vital component's circuit breaking, a deadman timeout, a Provider reporting a fault).
// Making "stop the machine" an RPC any App can call would turn one mistaken call into
// a production incident, and the emergency stop should be backed by physical buttons and the kernel's supervision chain, not depend on a network round trip.
package safety

import (
	"context"

	"google.golang.org/protobuf/proto"

	safetyv1 "github.com/nervus-os/nervus-ipc/protocol/interface/safetyv1"
	ipcv1 "github.com/nervus-os/nervus-ipc/protocol/ipcv1"

	"github.com/nervus-os/nervud/internal/audit"
	"github.com/nervus-os/nervud/internal/endpoint"
	"github.com/nervus-os/nervud/internal/identity"
	"github.com/nervus-os/nervud/internal/motiongate"
)

// BuiltinInterfaceID is this built-in interface's ID, which must match
// nervus-ipc's safety_control.proto.
const BuiltinInterfaceID = "nervus.interface.safety.control"

// method_ids come from the proto-generated enum, not constants copied here: a copy would quietly go stale,
// and the consequence of staleness is calls routed to the wrong method.
var (
	methodGetState        = uint32(safetyv1.SafetyControlMethod_SAFETY_CONTROL_METHOD_GET_STATE)
	methodRearm           = uint32(safetyv1.SafetyControlMethod_SAFETY_CONTROL_METHOD_REARM)
	methodRequestRecovery = uint32(safetyv1.SafetyControlMethod_SAFETY_CONTROL_METHOD_REQUEST_RECOVERY)
)

// BuiltinHandler returns a handler that can be passed straight to endpoint.RegisterBuiltin.
//
// The structured call/result shape keeps builtins on the same method gate as
// external Providers and leaves room for connection-scoped kernel services.
func (m *Module) BuiltinHandler() endpoint.BuiltinHandler {
	return func(call endpoint.BuiltinCall) endpoint.BuiltinResult {
		var payload []byte
		var code ipcv1.StatusCode
		switch call.MethodID {
		case methodGetState:
			payload, code = m.handleGetState()
		case methodRearm:
			payload, code = m.handleRearm(call.Caller)
		case methodRequestRecovery:
			payload, code = m.handleRequestRecovery(call.Caller)
		default:
			// Fail closed: an unimplemented method doesn't exist, and never succeeds silently;
			// a silent success would make the caller think the re-arm took effect.
			code = ipcv1.StatusCode_STATUS_CODE_NOT_FOUND
		}
		return endpoint.BuiltinResult{Payload: payload, Code: code}
	}
}

func (m *Module) handleGetState() ([]byte, ipcv1.StatusCode) {
	snap := m.SafetySnapshot()
	out, err := proto.Marshal(&safetyv1.SafetyState{
		State:       topStateToWire(snap.State),
		MotionEpoch: snap.Epoch,
		StopPhase:   stopPhaseToWire(snap.StopPhase),
	})
	if err != nil {
		return nil, ipcv1.StatusCode_STATUS_CODE_INTERNAL
	}
	return out, ipcv1.StatusCode_STATUS_CODE_OK
}

func (m *Module) handleRearm(caller identity.Caller) ([]byte, ipcv1.StatusCode) {
	// Internally Rearm only re-checks the hard precondition that can't be bypassed (has the stop progress settled); policy lives on the service side.
	if m.Rearm() {
		m.recordBuiltin(caller, "safety.Rearm", true)
		return nil, ipcv1.StatusCode_STATUS_CODE_OK
	}
	m.recordBuiltin(caller, "safety.Rearm", false)

	// Failure reasons must be distinguishable: a caller seeing STOP_NOT_SETTLED should wait and retry,
	// and one seeing WRONG_STATE should stop trying. Collapsing them into one code would make the recovery UI either give up
	// for nothing or loop forever on a state that can never succeed.
	return rearmFailureDetail(m.SafetySnapshot())
}

func (m *Module) handleRequestRecovery(caller identity.Caller) ([]byte, ipcv1.StatusCode) {
	if m.RequestRecovery() {
		m.recordBuiltin(caller, "safety.RequestRecovery", true)
		return nil, ipcv1.StatusCode_STATUS_CODE_OK
	}
	m.recordBuiltin(caller, "safety.RequestRecovery", false)
	return withDetail(ipcv1.StatusCode_STATUS_CODE_FAILED_PRECONDITION,
		safetyv1.SafetyControlReason_SAFETY_CONTROL_REASON_WRONG_STATE)
}

// rearmFailureDetail works out the distinguishable reason for a re-arm failure from the current snapshot.
func rearmFailureDetail(snap Snapshot) ([]byte, ipcv1.StatusCode) {
	if snap.State != motiongate.StateRearmRequired {
		// Not in REARM_REQUIRED at all: retrying any number of times changes nothing
		return withDetail(ipcv1.StatusCode_STATUS_CODE_FAILED_PRECONDITION,
			safetyv1.SafetyControlReason_SAFETY_CONTROL_REASON_WRONG_STATE)
	}
	// Right state but refused = the stop progress hasn't settled yet; waiting a bit may work
	return withDetail(ipcv1.StatusCode_STATUS_CODE_FAILED_PRECONDITION,
		safetyv1.SafetyControlReason_SAFETY_CONTROL_REASON_STOP_NOT_SETTLED)
}

func withDetail(code ipcv1.StatusCode, reason safetyv1.SafetyControlReason) ([]byte, ipcv1.StatusCode) {
	// The detail goes in error_detail rather than the payload: a failed Response uses the Failure
	// branch, and the payload path is never read. This returns a nil payload, and the detail is
	// built by ipc from the code. v1's built-in path doesn't pass typed details through yet; see the TODO below.
	//
	// TODO(builtin-detail): BuiltinHandler's signature can only return (payload, code),
	// with nowhere to put a typed error_detail. For the distinguishable reasons above to really reach the caller,
	// the signature needs to be able to carry a detail. Until then callers only see FAILED_PRECONDITION
	// and can't tell "wait a while" from "stop trying". That's the whole point of this file working out the reason,
	// so don't delete this logic when extending the signature.
	_ = reason
	return nil, code
}

// topStateToWire translates the kernel's motiongate.State into the wire enum.
//
// Unknown values map to UNSPECIFIED rather than panicking: when the kernel adds a new state and this hasn't caught up,
// an observer getting "can't say" is far safer than getting a wrong concrete state.
func topStateToWire(s motiongate.State) safetyv1.TopState {
	switch s {
	case motiongate.StateNormal:
		return safetyv1.TopState_TOP_STATE_NORMAL
	case motiongate.StateSafetyLatched:
		return safetyv1.TopState_TOP_STATE_SAFETY_LATCHED
	case motiongate.StateOEMRecovery:
		return safetyv1.TopState_TOP_STATE_OEM_RECOVERY
	case motiongate.StateRearmRequired:
		return safetyv1.TopState_TOP_STATE_REARM_REQUIRED
	default:
		return safetyv1.TopState_TOP_STATE_UNSPECIFIED
	}
}

// stopPhaseToWire translates the kernel's StopPhase into the wire enum.
func stopPhaseToWire(p StopPhase) safetyv1.StopPhase {
	switch p {
	case PhaseRequested:
		return safetyv1.StopPhase_STOP_PHASE_REQUESTED
	case PhaseSent:
		return safetyv1.StopPhase_STOP_PHASE_SENT
	case PhaseProviderAccepted:
		return safetyv1.StopPhase_STOP_PHASE_PROVIDER_ACCEPTED
	case PhaseMCUAcked:
		return safetyv1.StopPhase_STOP_PHASE_MCU_ACKED
	case PhaseOutputDisabled:
		return safetyv1.StopPhase_STOP_PHASE_OUTPUT_DISABLED
	case PhaseStandstillConfirmed:
		return safetyv1.StopPhase_STOP_PHASE_STANDSTILL_CONFIRMED
	case PhaseDeliveryFault:
		return safetyv1.StopPhase_STOP_PHASE_DELIVERY_FAULT
	case PhaseStandstillTimeout:
		return safetyv1.StopPhase_STOP_PHASE_STANDSTILL_TIMEOUT
	default:
		return safetyv1.StopPhase_STOP_PHASE_UNSPECIFIED
	}
}

// recordBuiltin records one built-in endpoint call in the audit.
//
// It must be recorded: Rearm is the only entry point in the whole system that can let a machine stopped for safety reasons move
// again. Who tried to release the stop, when, and whether it worked will certainly be asked afterwards.
func (m *Module) recordBuiltin(caller identity.Caller, action string, ok bool) {
	if m.aud == nil {
		return
	}
	m.aud.Record(context.Background(), audit.Event{
		Action:  action,
		Subject: caller.String(),
		Denied:  !ok,
	})
}
