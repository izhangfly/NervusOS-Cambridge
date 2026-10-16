// This file wires internal/operation's state machine to the wire, in the form of a built-in endpoint:
// nervus.interface.operation.control@1.
//
// # Why a built-in rather than a new Envelope body
//
// envelope.proto's rule: before adding a body, ask whether it's one of the eight things: establishing a connection / discovering an endpoint /
// making a call / returning a result / cancelling / subscribing / pushing an event / keeping the connection alive.
// "Query an operation's state" is none of them: it's just an ordinary method call.
//
// So it's a built-in, the same shape as Transfer Control: callers use completely standard Resolve + Request
// and neither know nor need to know the other side is the kernel.
//
// # Two groups of methods, two identities
//
//	Get / Cancel                           the caller (the one that created it)
//	Accept / ReportProgress / Complete     the Provider (the one executing it)
//
// Both groups are decided by connection identity. operation.Manager's Get/Cancel have built-in caller visibility
// checks; ownership on the reporting side is checked by this file: a Provider can only report on what nervud dispatched to it.
package ipc

import (
	operationv1 "github.com/nervus-os/nervus-ipc/protocol/interface/operationv1"
	ipcv1 "github.com/nervus-os/nervus-ipc/protocol/ipcv1"
	"google.golang.org/protobuf/proto"

	"github.com/nervus-os/nervud/internal/endpoint"
	"github.com/nervus-os/nervud/internal/operation"
	"github.com/nervus-os/nervud/internal/subscription"
)

// InterfaceOperationControl is this built-in interface's ID.
const InterfaceOperationControl = "nervus.interface.operation.control"

// operationBuiltinEndpointID is the endpoint number this built-in endpoint uses in the fan-out key.
//
// The event fan-out key is (ProviderConn, EndpointID, EventID, Scope). Built-ins have no conn,
// so ProviderConn is always nil; EndpointID is the one assigned at registration. RouteEvent already puts it
// in EventRoute.ProviderEndpointID, and this file records it at wiring for Publish.
//
// Both must be the same number: subscription registration uses the one RouteEvent gives, and fan-out uses the one recorded here;
// if they differ the subscriber never gets events, and neither side reports an error.
type operationWire struct {
	s *Server

	// endpointID is assigned by the endpoint module at RegisterBuiltin and read-only after wiring.
	endpointID uint64
}

// OperationBuiltinHandler returns a handler that can be passed to endpoint.RegisterBuiltin.
func (s *Server) OperationBuiltinHandler() endpoint.BuiltinHandler {
	w := &operationWire{s: s}
	s.operationWire = w
	return func(call endpoint.BuiltinCall) endpoint.BuiltinResult {
		return w.dispatch(call)
	}
}

// OperationSubscribeAdmitter returns the subscription admission.
//
// It answers "may this caller observe this operation", which is exactly
// operation.Manager.Get's visibility rule. It reuses that rather than writing another: once two sets of rules
// diverge, you get "can query but can't subscribe" or the reverse, and both look legitimate enough to hide the problem.
func (s *Server) OperationSubscribeAdmitter() endpoint.BuiltinSubscribeAdmitter {
	return func(call endpoint.BuiltinSubscribeCall) endpoint.BuiltinSubscribeResult {
		if s.operations == nil {
			return endpoint.BuiltinSubscribeResult{
				Code: ipcv1.StatusCode_STATUS_CODE_UNAVAILABLE,
			}
		}
		if call.EventID != uint32(
			operationv1.OperationControlEvent_OPERATION_CONTROL_EVENT_OPERATION_CHANGED) {
			return endpoint.BuiltinSubscribeResult{
				Code: ipcv1.StatusCode_STATUS_CODE_NOT_FOUND,
			}
		}

		// scope is the operation_id. The kernel understands it itself, with no need to decode a Provider's proto.
		//
		// Other callers always get NOT_FOUND, not PERMISSION_DENIED. The latter would tell
		// the caller "this operation exists, it's just not yours", which is information in itself.
		// Consistent with Manager.Get's indistinguishable projection.
		if _, ok := s.operations.Get(call.Caller, call.Scope); !ok {
			return endpoint.BuiltinSubscribeResult{
				Code: ipcv1.StatusCode_STATUS_CODE_NOT_FOUND,
			}
		}
		return endpoint.BuiltinSubscribeResult{Code: ipcv1.StatusCode_STATUS_CODE_OK}
	}
}

// SetOperationEndpointID records the built-in endpoint's handle, for event fan-out.
// Called once at wiring.
func (s *Server) SetOperationEndpointID(id uint64) {
	if s.operationWire != nil {
		s.operationWire.endpointID = id
	}
}

// OperationEventObserver returns the bypass that can be passed to operation.Manager.SetEventObserver.
//
// It runs under operation.mu: it only fans out without blocking and never calls back into operation.
// The lock order is always operation.mu -> subscription.mu.
func (s *Server) OperationEventObserver() func(operation.Event) {
	return func(ev operation.Event) {
		s.publishOperationEvent(ev)
	}
}

func (s *Server) publishOperationEvent(ev operation.Event) {
	if s == nil || s.subscriptions == nil || s.operationWire == nil {
		return
	}
	payload, err := proto.Marshal(operationEventToWire(ev))
	if err != nil {
		// Failing to encode is a bug on this side. Drop this one and log it: turning one encoding failure into
		// a panic would take down the whole of nervud.
		s.log.Error("ipc: marshal OperationEvent", "operation_id", ev.OperationID, "err", err)
		return
	}

	key := subscription.Key{
		ProviderConn: nil, // built-ins have no conn
		EndpointID:   s.operationWire.endpointID,
		EventID: uint32(
			operationv1.OperationControlEvent_OPERATION_CONTROL_EVENT_OPERATION_CHANGED),
		// Split by operation: otherwise subscribers would get everyone's progress and detailed failure causes on the machine.
		Scope: ev.OperationID,
	}
	closed := s.subscriptions.Publish(key, payload, 0)
	s.closeSubscriptions(closed)

	// There are no events after a terminal state. Keeping the subscription would only keep the caller waiting; close it explicitly,
	// so its range ends naturally.
	if ev.Kind == operation.EventState && ev.State.Terminal() {
		s.closeSubscriptions(s.subscriptions.CloseScope(
			nil, s.operationWire.endpointID, ev.OperationID,
			ipcv1.SubscriptionClosedReason_SUBSCRIPTION_CLOSED_REASON_ENDPOINT_DIED))
	}
}

// dispatch dispatches by method_id.
func (w *operationWire) dispatch(call endpoint.BuiltinCall) endpoint.BuiltinResult {
	if w == nil || w.s == nil || w.s.operations == nil {
		return endpoint.BuiltinResult{Code: ipcv1.StatusCode_STATUS_CODE_UNAVAILABLE}
	}
	switch operationv1.OperationControlMethod(call.MethodID) {
	case operationv1.OperationControlMethod_OPERATION_CONTROL_METHOD_GET_OPERATION:
		return w.get(call)
	case operationv1.OperationControlMethod_OPERATION_CONTROL_METHOD_CANCEL_OPERATION:
		return w.cancel(call)
	case operationv1.OperationControlMethod_OPERATION_CONTROL_METHOD_ACCEPT_OPERATION:
		return w.accept(call)
	case operationv1.OperationControlMethod_OPERATION_CONTROL_METHOD_REPORT_PROGRESS:
		return w.reportProgress(call)
	case operationv1.OperationControlMethod_OPERATION_CONTROL_METHOD_COMPLETE_OPERATION:
		return w.complete(call)
	default:
		// Fail closed: an unimplemented method doesn't exist.
		return endpoint.BuiltinResult{Code: ipcv1.StatusCode_STATUS_CODE_NOT_FOUND}
	}
}

func (w *operationWire) get(call endpoint.BuiltinCall) endpoint.BuiltinResult {
	var req operationv1.GetOperationRequest
	if err := proto.Unmarshal(call.Payload, &req); err != nil {
		return operationFailure(ipcv1.StatusCode_STATUS_CODE_INVALID_ARGUMENT,
			operationv1.OperationReason_OPERATION_REASON_UNSPECIFIED)
	}
	op, ok := w.s.operations.Get(call.Caller, req.GetOperationId())
	if !ok {
		return operationFailure(ipcv1.StatusCode_STATUS_CODE_NOT_FOUND,
			operationv1.OperationReason_OPERATION_REASON_NOT_FOUND)
	}
	wire, err := proto.Marshal(operationStatusToWire(op, w.s))
	if err != nil {
		return endpoint.BuiltinResult{Code: ipcv1.StatusCode_STATUS_CODE_INTERNAL}
	}
	return endpoint.BuiltinResult{Payload: wire, Code: ipcv1.StatusCode_STATUS_CODE_OK}
}

func (w *operationWire) cancel(call endpoint.BuiltinCall) endpoint.BuiltinResult {
	var req operationv1.CancelOperationRequest
	if err := proto.Unmarshal(call.Payload, &req); err != nil {
		return operationFailure(ipcv1.StatusCode_STATUS_CODE_INVALID_ARGUMENT,
			operationv1.OperationReason_OPERATION_REASON_UNSPECIFIED)
	}
	code := w.s.operations.Cancel(call.Caller, req.GetOperationId())
	if code == ipcv1.StatusCode_STATUS_CODE_NOT_FOUND {
		return operationFailure(code, operationv1.OperationReason_OPERATION_REASON_NOT_FOUND)
	}
	// ACCEPTED rather than OK: a cancel is a request, not a command. The real terminal state comes in later events.
	return endpoint.BuiltinResult{Code: code}
}

// ---- Provider side ---------------------------------------------------------------

// providerOperation checks the ownership of one Provider report and returns the operation being reported on.
//
// A Provider can only report on what nervud dispatched to it. Without this check, any system service
// could report someone else's operation as failed, and the caller would see a "normal" failure,
// with even its detailed cause forged.
func (w *operationWire) providerOperation(
	call endpoint.BuiltinCall, id uint64,
) (operation.Operation, endpoint.BuiltinResult, bool) {
	op, ok := w.s.operations.ProviderOperation(call.Conn, id)
	if !ok {
		// Doesn't exist, or wasn't dispatched to this connection. Both get the same answer: telling them apart would tell
		// the caller "this id exists, it's just not yours", which is information in itself.
		return operation.Operation{}, operationFailure(
			ipcv1.StatusCode_STATUS_CODE_NOT_FOUND,
			operationv1.OperationReason_OPERATION_REASON_NOT_FOUND), false
	}
	return op, endpoint.BuiltinResult{}, true
}

func (w *operationWire) accept(call endpoint.BuiltinCall) endpoint.BuiltinResult {
	var req operationv1.AcceptOperationRequest
	if err := proto.Unmarshal(call.Payload, &req); err != nil {
		return operationFailure(ipcv1.StatusCode_STATUS_CODE_INVALID_ARGUMENT,
			operationv1.OperationReason_OPERATION_REASON_UNSPECIFIED)
	}
	if _, fail, ok := w.providerOperation(call, req.GetOperationId()); !ok {
		return fail
	}
	if err := w.s.operations.Accept(req.GetOperationId(), req.GetMotionEpoch()); err != nil {
		return operationErrToResult(err)
	}
	return endpoint.BuiltinResult{Code: ipcv1.StatusCode_STATUS_CODE_OK}
}

func (w *operationWire) reportProgress(call endpoint.BuiltinCall) endpoint.BuiltinResult {
	var req operationv1.ReportProgressRequest
	if err := proto.Unmarshal(call.Payload, &req); err != nil {
		return operationFailure(ipcv1.StatusCode_STATUS_CODE_INVALID_ARGUMENT,
			operationv1.OperationReason_OPERATION_REASON_UNSPECIFIED)
	}
	if _, fail, ok := w.providerOperation(call, req.GetOperationId()); !ok {
		return fail
	}
	if err := w.s.operations.Progress(req.GetOperationId(), req.GetProgress()); err != nil {
		return operationErrToResult(err)
	}
	return endpoint.BuiltinResult{Code: ipcv1.StatusCode_STATUS_CODE_OK}
}

func (w *operationWire) complete(call endpoint.BuiltinCall) endpoint.BuiltinResult {
	var req operationv1.CompleteOperationRequest
	if err := proto.Unmarshal(call.Payload, &req); err != nil {
		return operationFailure(ipcv1.StatusCode_STATUS_CODE_INVALID_ARGUMENT,
			operationv1.OperationReason_OPERATION_REASON_UNSPECIFIED)
	}
	if _, fail, ok := w.providerOperation(call, req.GetOperationId()); !ok {
		return fail
	}

	var err error
	switch req.GetCode() {
	case ipcv1.StatusCode_STATUS_CODE_OK:
		err = w.s.operations.Succeed(req.GetOperationId(), req.GetResult())
	case ipcv1.StatusCode_STATUS_CODE_CANCELLED:
		err = w.s.operations.Cancelled(req.GetOperationId())
	case ipcv1.StatusCode_STATUS_CODE_INVALID_ARGUMENT,
		ipcv1.StatusCode_STATUS_CODE_NOT_FOUND,
		ipcv1.StatusCode_STATUS_CODE_FAILED_PRECONDITION,
		ipcv1.StatusCode_STATUS_CODE_RESOURCE_EXHAUSTED,
		ipcv1.StatusCode_STATUS_CODE_DEADLINE_EXCEEDED,
		ipcv1.StatusCode_STATUS_CODE_UNAVAILABLE,
		ipcv1.StatusCode_STATUS_CODE_INTERNAL:
		err = w.s.operations.Fail(req.GetOperationId(), req.GetCode(), req.GetErrorDetail())
	default:
		// ACCEPTED / UNAUTHENTICATED / PERMISSION_DENIED and the like are decision results reserved for the kernel,
		// not execution results. A Provider writing them would be forging a kernel decision.
		return operationFailure(ipcv1.StatusCode_STATUS_CODE_INVALID_ARGUMENT,
			operationv1.OperationReason_OPERATION_REASON_INVALID_TERMINAL_CODE)
	}
	if err != nil {
		return operationErrToResult(err)
	}
	return endpoint.BuiltinResult{Code: ipcv1.StatusCode_STATUS_CODE_OK}
}
