// This file is a thin adaptation layer between operation's local types <-> wire types.
//
// It only translates and never decides. Any if that affects "whether it may" rather than "how to write it as
// bytes" belongs in the operation package or operation_builtin.go, not here.
// A decision mixed into the conversion layer would bypass audit, because audit records transitions in the state machine.
package ipc

import (
	"errors"

	operationv1 "github.com/nervus-os/nervus-ipc/protocol/interface/operationv1"
	ipcv1 "github.com/nervus-os/nervus-ipc/protocol/ipcv1"
	"google.golang.org/protobuf/proto"

	"github.com/nervus-os/nervud/internal/endpoint"
	"github.com/nervus-os/nervud/internal/operation"
)

// stateToWire translates the internal state into the wire enum.
//
// Unknown values map to UNSPECIFIED rather than a guess: UNSPECIFIED on the wire is an explicit
// "fail closed" sentinel that clients never treat as any definite state. A guess would let the caller
// carry on from a wrong conclusion.
func stateToWire(s operation.State) operationv1.OperationState {
	switch s {
	case operation.StatePending:
		return operationv1.OperationState_OPERATION_STATE_PENDING
	case operation.StateRunning:
		return operationv1.OperationState_OPERATION_STATE_RUNNING
	case operation.StateCancelRequested:
		return operationv1.OperationState_OPERATION_STATE_CANCEL_REQUESTED
	case operation.StateSucceeded:
		return operationv1.OperationState_OPERATION_STATE_SUCCEEDED
	case operation.StateFailed:
		return operationv1.OperationState_OPERATION_STATE_FAILED
	case operation.StateCancelled:
		return operationv1.OperationState_OPERATION_STATE_CANCELLED
	default:
		return operationv1.OperationState_OPERATION_STATE_UNSPECIFIED
	}
}

func originToWire(o operation.OriginBinding) *operationv1.OperationOrigin {
	return &operationv1.OperationOrigin{
		InterfaceId:         o.InterfaceID,
		InterfaceMajor:      o.IfaceMajor,
		InterfaceMinor:      o.IfaceMinor,
		MethodId:            o.MethodID,
		InterfaceSchemaHash: append([]byte(nil), o.SchemaHash...),
	}
}

// operationStatusToWire projects a snapshot.
//
// The deadline must be translated into the CLOCK_MONOTONIC absolute-nanosecond domain, the same clock as ExecutionContext.
// When it can't be translated (no monotonic clock) it's left as 0 rather than failing: what the caller needs most is the state and terminal
// reason, and a missing deadline shouldn't fail the whole query.
func operationStatusToWire(op operation.Operation, s *Server) *operationv1.OperationStatus {
	status := &operationv1.OperationStatus{
		OperationId:     op.ID,
		State:           stateToWire(op.State),
		Origin:          originToWire(op.Origin),
		ResourceHandles: append([]string(nil), op.Resources...),
		MotionEpoch:     op.MotionEpoch,
		TerminalCode:    op.TerminalStatus,
		TerminalResult:  append([]byte(nil), op.TerminalResult...),
		TerminalError:   append([]byte(nil), op.TerminalError...),
	}
	if nanos, err := s.monotonicDeadlineNanos(op.Deadline); err == nil {
		status.DeadlineNanos = nanos
	}
	return status
}

func eventKindToWire(k operation.EventKind) operationv1.OperationEventKind {
	switch k {
	case operation.EventProgress:
		return operationv1.OperationEventKind_OPERATION_EVENT_KIND_PROGRESS
	case operation.EventState:
		return operationv1.OperationEventKind_OPERATION_EVENT_KIND_STATE
	default:
		return operationv1.OperationEventKind_OPERATION_EVENT_KIND_UNSPECIFIED
	}
}

func operationEventToWire(ev operation.Event) *operationv1.OperationEvent {
	return &operationv1.OperationEvent{
		OperationId:    ev.OperationID,
		Kind:           eventKindToWire(ev.Kind),
		State:          stateToWire(ev.State),
		Origin:         originToWire(ev.Origin),
		Progress:       append([]byte(nil), ev.Payload...),
		TerminalCode:   ev.TerminalStatus,
		TerminalResult: append([]byte(nil), ev.TerminalResult...),
		TerminalError:  append([]byte(nil), ev.TerminalError...),
	}
}

// operationAcceptedResponse turns a Provider's ACCEPTED into a final response carrying an OperationHandle.
//
// The handle is written by the kernel: operation_id is allocated by nervud, and the state machine belongs to nervud too. The Provider's
// Success.payload must be empty; non-empty means it tried to make its own handle, which would give the caller
// a number nervud doesn't know, and cancels and subscriptions would silently stop working.
func (s *Server) operationAcceptedResponse(
	entry *routeEntry, success *ipcv1.Success,
) (*ipcv1.Response, bool) {
	if len(success.GetPayload()) != 0 {
		return s.rejectProviderResult(entry,
			errors.New("ipc: Provider set a payload on an operation ACCEPTED"))
	}
	operationID := entry.execution.GetOperationId()
	if operationID == 0 {
		// The dispatch side should already have created the operation and put it in the ExecutionContext.
		// Getting here means the two disagree. Refusing it as a Provider violation would be wrong,
		// but an ACCEPTED without a handle can't be let through either: the caller would think it got
		// a long task and then never be able to look it up.
		return s.rejectProviderResult(entry,
			errors.New("ipc: operation ACCEPTED without an operation id"))
	}

	// Long tasks carry no Transfer handles: setting up the data plane goes through ordinary methods such as
	// OpenStream. This explicitly ends the route "with no handles", so a prepared pipe isn't left dangling.
	if err := s.transfer.FinishRoute(entry.routeID, true, nil); err != nil {
		return s.rejectProviderResult(entry, err)
	}

	payload, err := proto.Marshal(&operationv1.OperationHandle{OperationId: operationID})
	if err != nil {
		return s.rejectProviderResult(entry, err)
	}
	return &ipcv1.Response{
		RequestId: entry.sourceRequestID,
		Outcome: &ipcv1.Response_Success{Success: &ipcv1.Success{
			Code:    ipcv1.StatusCode_STATUS_CODE_ACCEPTED,
			Payload: payload,
		}},
	}, true
}

// operationFailure builds a built-in failure carrying a typed detail.
func operationFailure(
	code ipcv1.StatusCode, reason operationv1.OperationReason,
) endpoint.BuiltinResult {
	detail, err := proto.Marshal(&operationv1.OperationErrorDetail{Reason: reason})
	if err != nil {
		// Failing to encode the detail shouldn't turn the failure into a response with no code:
		// the code itself carries the most important information (whether to retry), so keep it.
		return endpoint.BuiltinResult{Code: code}
	}
	// ErrorDetail rather than Payload: the latter is the response payload on success, typed by
	// response_type. Putting it in the wrong field would make the caller decode bytes as the wrong type.
	return endpoint.BuiltinResult{ErrorDetail: detail, Code: code}
}

// operationErrToResult translates the operation package's sentinel errors into (code, typed reason).
//
// The outer code decides the SDK's recovery behaviour, and the reason decides whether a person can understand what happened.
// The two must agree: a code saying "precondition not met" with a reason saying "not found" would send troubleshooting
// the wrong way from the start.
func operationErrToResult(err error) endpoint.BuiltinResult {
	switch {
	case errors.Is(err, operation.ErrNotFound):
		return operationFailure(ipcv1.StatusCode_STATUS_CODE_NOT_FOUND,
			operationv1.OperationReason_OPERATION_REASON_NOT_FOUND)
	case errors.Is(err, operation.ErrAlreadyTerminal):
		return operationFailure(ipcv1.StatusCode_STATUS_CODE_FAILED_PRECONDITION,
			operationv1.OperationReason_OPERATION_REASON_ALREADY_TERMINAL)
	case errors.Is(err, operation.ErrStaleEpoch):
		return operationFailure(ipcv1.StatusCode_STATUS_CODE_FAILED_PRECONDITION,
			operationv1.OperationReason_OPERATION_REASON_STALE_EPOCH)
	case errors.Is(err, operation.ErrIllegalTransition):
		return operationFailure(ipcv1.StatusCode_STATUS_CODE_FAILED_PRECONDITION,
			operationv1.OperationReason_OPERATION_REASON_INVALID_TRANSITION)
	default:
		// Errors that can't be recognised are normalised to INTERNAL + UNSPECIFIED. Don't guess a specific reason:
		// guessing wrong would make the Provider retry or give up on a wrong conclusion.
		return operationFailure(ipcv1.StatusCode_STATUS_CODE_INTERNAL,
			operationv1.OperationReason_OPERATION_REASON_UNSPECIFIED)
	}
}

// convergeSynchronousOperation closes the operation of a long-task method whose
// Provider answered with a final result instead of ACCEPTED.
//
// pkgmanagerd does this for INSTALL: it finishes the work inside the call and
// returns OK with the typed result. Without this step the operation stays
// PENDING until its deadline and is then recorded as DEADLINE_EXCEEDED, so the
// audit log says an install failed when it succeeded, and anyone watching the
// operation sees a failure.
//
// A real long task answers ACCEPTED and reports through operation.control, so
// it is left alone here.
func (s *Server) convergeSynchronousOperation(entry *routeEntry, resp *ipcv1.Response) {
	if s.operations == nil || !entry.route.Method.Meta.GetReturnsOperation() {
		return
	}
	id := entry.execution.GetOperationId()
	if id == 0 {
		return
	}
	if success := resp.GetSuccess(); success != nil {
		if success.GetCode() == ipcv1.StatusCode_STATUS_CODE_ACCEPTED {
			return
		}
		// PENDING can't jump straight to SUCCEEDED; Accept carries the epoch
		// check, so a result that arrives after a revocation still fails closed.
		if err := s.operations.Accept(id, entry.execution.GetMotionEpoch()); err != nil {
			s.log.Warn("ipc: converge synchronous operation", "operation_id", id, "err", err)
			return
		}
		if err := s.operations.Succeed(id, nil); err != nil {
			s.log.Warn("ipc: converge synchronous operation", "operation_id", id, "err", err)
		}
		return
	}
	if failure := resp.GetFailure(); failure != nil {
		if err := s.operations.Fail(id, failure.GetCode(), nil); err != nil {
			s.log.Warn("ipc: converge synchronous operation", "operation_id", id, "err", err)
		}
	}
}
