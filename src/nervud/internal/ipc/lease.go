// This file is the wire wiring of ControlLease: it connects the Envelope's AcquireControl(70) /
// ReleaseControl(72) to internal/control's lease state machine.
//
// # Why this line used to be broken
//
// The control module itself was complete long ago (request/renew/release/pre-empt/deadman/epoch increments, 1200 lines
// of source + 1000 lines of tests), but nervud's conn state machine never handled these four bodies;
// the kernel was even pinned for a while to an old protocol version without these messages. The result: the lease logic was written
// but had no entry point, Apps couldn't get a motion lease, and operation.Manager's LeaseValidator
// could only be injected as nil, so every motion operation was refused up front.
//
// This file fills in that piece.
//
// # Leases are connection-scoped
//
// A lease_id is bound to this connection, can't be transferred, and lapses on disconnect (envelope.proto:
// AcquireControlSuccess.lease_id). So the conn must RevokeConn when it finishes, otherwise a
// disconnected App would still "hold" actuator control, which nobody could take away until the TTL expired naturally.
package ipc

import (
	"errors"
	"math"
	"time"

	"google.golang.org/protobuf/proto"

	ipcv1 "github.com/nervus-os/nervus-ipc/protocol/ipcv1"

	"github.com/nervus-os/nervud/internal/control"
)

// handleAcquireControl handles one lease request.
func (co *conn) handleAcquireControl(req *ipcv1.AcquireControl) bool {
	reqID := req.GetRequestId()
	if reqID == 0 {
		co.log.Warn("ipc: AcquireControl with reserved request_id 0, closing")
		co.s.auditViolation(co.caller, errZeroRequestID)
		return false
	}

	if co.s.leases == nil {
		// control isn't wired (tests / trimmed builds). Return UNAVAILABLE rather than closing the connection:
		// it's a capability gap, not a protocol violation, and retrying or degrading are both reasonable for the client.
		return co.enqueue(acquireFailure(reqID,
			ipcv1.StatusCode_STATUS_CODE_UNAVAILABLE,
			ipcv1.ControlLeaseErrorReason_CONTROL_LEASE_ERROR_REASON_RESOURCE_UNAVAILABLE))
	}

	class, ok := classFromWire(req.GetControllerClass())
	if !ok {
		// UNSPECIFIED or an unknown value. Fail closed: don't guess a category for the client;
		// guessing wrong would treat an AI session as a person operating, and the pre-emption matrix would give it the wrong priority.
		return co.enqueue(acquireFailure(reqID,
			ipcv1.StatusCode_STATUS_CODE_INVALID_ARGUMENT,
			ipcv1.ControlLeaseErrorReason_CONTROL_LEASE_ERROR_REASON_INVALID_CONTROLLER))
	}

	// Resource resolution: an empty selector is refused (v2 has no implicit default; see resolveLeaseResource).
	// It stays consistent with ResolveEndpoint's selector semantics; otherwise the same "left empty" would mean different things on the two
	// paths, the kind of inconsistency that most easily breeds bugs.
	resource, resourceGeneration, ok := co.s.resolveLeaseResource(req.GetResource())
	if !ok {
		return co.enqueue(acquireFailure(reqID,
			ipcv1.StatusCode_STATUS_CODE_FAILED_PRECONDITION,
			ipcv1.ControlLeaseErrorReason_CONTROL_LEASE_ERROR_REASON_RESOURCE_UNAVAILABLE))
	}

	// The current policy explicitly lets the caller choose controller_class and records the choice in the audit. The Package,
	// Component and connection identity have all been verified by the kernel, but the HUMAN/AI value itself is still self-reported;
	// it affects pre-emption priority, so this code can't be described as a trusted class decision. If session /
	// human-presence proofs are introduced later, the wire value should be cross-checked against a trusted policy result here.
	co.s.auditLeaseClass(co.caller, class, resource)

	lease, err := co.s.leases.Acquire(control.Request{
		Conn:               co.leaseConnID(),
		Class:              class,
		Resource:           resource,
		ResourceGeneration: resourceGeneration,
		Owner:              co.caller,
		RequestedTTL:       time.Duration(req.GetRequestedDeadlineNanos()),
	})
	if err != nil {
		code, reason := leaseErrToWire(err)
		return co.enqueue(acquireFailure(reqID, code, reason))
	}
	// Catalog publication and its RevokeResource callback are intentionally
	// separate operations. If publication happened after the first resolve but
	// its old-generation revoke completed before Acquire, that scan could not
	// see the lease we just created. Resolve again before exposing a wire handle:
	// either we still observe the exact authority used for the lease, or the
	// newly published generation will revoke it after this check.
	currentResource, currentGeneration, current := co.s.resolveLeaseResource(req.GetResource())
	if !current || currentResource != resource || currentGeneration != resourceGeneration {
		_ = co.s.leases.Release(lease.ID, co.leaseConnID())
		return co.enqueue(acquireFailure(reqID,
			ipcv1.StatusCode_STATUS_CODE_FAILED_PRECONDITION,
			ipcv1.ControlLeaseErrorReason_CONTROL_LEASE_ERROR_REASON_RESOURCE_UNAVAILABLE))
	}
	deadlineNanos, err := co.s.monotonicDeadlineNanos(lease.Deadline)
	if err != nil {
		// Do not leave a lease active when its required wire representation could
		// not be produced. The caller never received a handle for this lease.
		_ = co.s.leases.Release(lease.ID, co.leaseConnID())
		co.s.auditViolation(co.caller, err)
		return co.enqueue(acquireFailure(reqID,
			ipcv1.StatusCode_STATUS_CODE_INTERNAL,
			ipcv1.ControlLeaseErrorReason_CONTROL_LEASE_ERROR_REASON_UNSPECIFIED))
	}

	return co.enqueue(&ipcv1.Envelope{Body: &ipcv1.Envelope_AcquireControlResult{
		AcquireControlResult: &ipcv1.AcquireControlResult{
			RequestId: reqID,
			Outcome: &ipcv1.AcquireControlResult_Success{Success: &ipcv1.AcquireControlSuccess{
				// lease_id is a uint64 on the wire, while control.ID is a [16]byte.
				// A monotonic per-connection handle mapping is used so the internal ID isn't leaked,
				// the same principle as endpoint_id: the lookup key is (connection, handle).
				LeaseId:        co.registerLease(lease),
				MotionEpoch:    lease.Epoch,
				DeadlineNanos:  deadlineNanos,
				ResourceHandle: lease.Resource,
			}},
		},
	}})
}

// monotonicDeadlineNanos converts a Go monotonic deadline into the absolute
// Linux CLOCK_MONOTONIC domain used by control and Dispatch execution contexts.
func (s *Server) monotonicDeadlineNanos(deadline time.Time) (int64, error) {
	if s == nil || s.monotonicNow == nil {
		return 0, errors.New("ipc: monotonic clock unavailable")
	}
	now, err := s.monotonicNow()
	if err != nil {
		return 0, err
	}
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return 0, errors.New("ipc: control lease expired before response")
	}
	delta := uint64(remaining)
	if now > math.MaxInt64 || delta > math.MaxInt64-now {
		return 0, errors.New("ipc: monotonic lease deadline overflow")
	}
	return int64(now + delta), nil
}

// handleReleaseControl handles one voluntary release.
func (co *conn) handleReleaseControl(req *ipcv1.ReleaseControl) bool {
	reqID := req.GetRequestId()
	if reqID == 0 {
		co.log.Warn("ipc: ReleaseControl with reserved request_id 0, closing")
		co.s.auditViolation(co.caller, errZeroRequestID)
		return false
	}

	if co.s.leases == nil {
		return co.enqueue(releaseFailure(reqID, ipcv1.StatusCode_STATUS_CODE_UNAVAILABLE))
	}

	id, ok := co.lookupLease(req.GetLeaseId())
	if !ok {
		// Not a lease_id this connection holds. The protocol says FAILED_PRECONDITION.
		// The connection isn't closed: releasing an expired / pre-empted lease_id is normal timing
		//  (the client hasn't received the revocation notice yet), and treating it as a protocol violation would kick normal clients off often.
		return co.enqueue(releaseFailure(reqID, ipcv1.StatusCode_STATUS_CODE_FAILED_PRECONDITION))
	}

	if err := co.s.leases.Release(id, co.leaseConnID()); err != nil {
		return co.enqueue(releaseFailure(reqID, ipcv1.StatusCode_STATUS_CODE_FAILED_PRECONDITION))
	}
	co.forgetLease(req.GetLeaseId())

	return co.enqueue(&ipcv1.Envelope{Body: &ipcv1.Envelope_ReleaseControlResult{
		ReleaseControlResult: &ipcv1.ReleaseControlResult{
			RequestId: reqID,
			Outcome: &ipcv1.ReleaseControlResult_Success{
				Success: &ipcv1.Success{Code: ipcv1.StatusCode_STATUS_CODE_OK},
			},
		},
	}})
}

// classFromWire translates the wire ControllerClass into control.Class.
//
// Unknown values always fail closed: the protocol says "unspecified or unknown values always fail closed",
// and the consequence of guessing wrong here is the pre-emption matrix using the wrong priority: treating AI as a person, or the reverse.
func classFromWire(c ipcv1.ControllerClass) (control.Class, bool) {
	switch c {
	case ipcv1.ControllerClass_CONTROLLER_CLASS_HUMAN:
		return control.ClassHuman, true
	case ipcv1.ControllerClass_CONTROLLER_CLASS_AI:
		return control.ClassAI, true
	default:
		return control.ClassUnspecified, false
	}
}

func classToWire(c control.Class) (ipcv1.ControllerClass, bool) {
	switch c {
	case control.ClassHuman:
		return ipcv1.ControllerClass_CONTROLLER_CLASS_HUMAN, true
	case control.ClassAI:
		return ipcv1.ControllerClass_CONTROLLER_CLASS_AI, true
	default:
		return ipcv1.ControllerClass_CONTROLLER_CLASS_UNSPECIFIED, false
	}
}

// leaseErrToWire translates control's errors into (StatusCode, typed reason).
//
// The outer code decides the SDK's recovery behaviour, and the typed reason decides whether the client should back off or pre-empt.
// envelope.proto says "callers need distinguishable reasons such as who holds it to know whether to back off or
// pre-empt; a generic BUSY isn't enough". The two must agree.
func leaseErrToWire(err error) (ipcv1.StatusCode, ipcv1.ControlLeaseErrorReason) {
	switch {
	case errors.Is(err, control.ErrHeldByHuman):
		return ipcv1.StatusCode_STATUS_CODE_FAILED_PRECONDITION,
			ipcv1.ControlLeaseErrorReason_CONTROL_LEASE_ERROR_REASON_HELD_BY_HUMAN
	case errors.Is(err, control.ErrHeldByAI):
		return ipcv1.StatusCode_STATUS_CODE_FAILED_PRECONDITION,
			ipcv1.ControlLeaseErrorReason_CONTROL_LEASE_ERROR_REASON_HELD_BY_AI
	case errors.Is(err, control.ErrSafetyLatched):
		return ipcv1.StatusCode_STATUS_CODE_FAILED_PRECONDITION,
			ipcv1.ControlLeaseErrorReason_CONTROL_LEASE_ERROR_REASON_SAFETY_LATCHED
	case errors.Is(err, control.ErrUnknownResource):
		return ipcv1.StatusCode_STATUS_CODE_FAILED_PRECONDITION,
			ipcv1.ControlLeaseErrorReason_CONTROL_LEASE_ERROR_REASON_RESOURCE_UNAVAILABLE
	case errors.Is(err, control.ErrInvalidRequest), errors.Is(err, control.ErrPolicyViolation):
		return ipcv1.StatusCode_STATUS_CODE_INVALID_ARGUMENT,
			ipcv1.ControlLeaseErrorReason_CONTROL_LEASE_ERROR_REASON_INVALID_CONTROLLER
	case errors.Is(err, control.ErrShuttingDown):
		return ipcv1.StatusCode_STATUS_CODE_UNAVAILABLE,
			ipcv1.ControlLeaseErrorReason_CONTROL_LEASE_ERROR_REASON_RESOURCE_UNAVAILABLE
	case errors.Is(err, control.ErrResourceCapacity):
		return ipcv1.StatusCode_STATUS_CODE_UNAVAILABLE,
			ipcv1.ControlLeaseErrorReason_CONTROL_LEASE_ERROR_REASON_RESOURCE_UNAVAILABLE
	default:
		// Errors that can't be recognised are normalised to INTERNAL + UNSPECIFIED reason.
		// Don't guess a specific reason: guessing wrong would make the client back off or pre-empt by the wrong strategy.
		return ipcv1.StatusCode_STATUS_CODE_INTERNAL,
			ipcv1.ControlLeaseErrorReason_CONTROL_LEASE_ERROR_REASON_UNSPECIFIED
	}
}

func acquireFailure(reqID uint64, code ipcv1.StatusCode, reason ipcv1.ControlLeaseErrorReason) *ipcv1.Envelope {
	detail, err := proto.Marshal(&ipcv1.ControlLeaseErrorDetail{Reason: reason})
	if err != nil {
		// Failing to encode the detail is a bug on this side; still return a failure, so the caller doesn't wait forever
		detail = nil
	}
	return &ipcv1.Envelope{Body: &ipcv1.Envelope_AcquireControlResult{
		AcquireControlResult: &ipcv1.AcquireControlResult{
			RequestId: reqID,
			Outcome: &ipcv1.AcquireControlResult_Failure{Failure: &ipcv1.Failure{
				Code:        code,
				ErrorDetail: detail,
			}},
		},
	}}
}

func releaseFailure(reqID uint64, code ipcv1.StatusCode) *ipcv1.Envelope {
	return &ipcv1.Envelope{Body: &ipcv1.Envelope_ReleaseControlResult{
		ReleaseControlResult: &ipcv1.ReleaseControlResult{
			RequestId: reqID,
			Outcome:   &ipcv1.ReleaseControlResult_Failure{Failure: &ipcv1.Failure{Code: code}},
		},
	}}
}
