// This file turns failures on the wire (StatusCode + typed error_detail) into Go errors.
//
// Design trade-off: StatusCodes aren't mapped into a pile of Go sentinel errors; a structured
// *StatusError is kept instead. For a caller to tell "back off and retry" from "Resolve again" from "no chance at all",
// it needs both the code and the typed detail, and collapsing into sentinels would lose the detail.
package sdk

import (
	"errors"
	"fmt"

	"google.golang.org/protobuf/proto"

	ipcv1 "github.com/nervus-os/nervus-ipc/protocol/ipcv1"
)

// Sentinel errors at the connection and protocol level.
var (
	// ErrClosed: the connection is closed (a normal close or this side's own Close).
	ErrClosed = errors.New("sdk: connection closed")
	// ErrHandshakeFailed: nervud refused the handshake; see the wrapped *StatusError for details.
	ErrHandshakeFailed = errors.New("sdk: handshake rejected")
	// ErrProtocol: the peer sent an Envelope that violates the protocol (wrong direction, unexpected body and so on).
	ErrProtocol = errors.New("sdk: protocol violation by peer")
	// ErrRequestIDExhausted: request_ids are used up. The protocol says they don't wrap, so the connection must be rebuilt.
	ErrRequestIDExhausted = errors.New("sdk: request id space exhausted; reconnect required")
)

// StatusError is the failure result of one call.
//
// Detail is the **undecoded** typed error_detail bytes: its exact type is decided by context
// (ResolveEndpoint failures are always ResolveEndpointErrorDetail, ControlLease always
// ControlLeaseErrorDetail, and method call failures the interface's own detail). This package doesn't guess the type;
// callers decode it with UnmarshalDetail from known context.
//
// It isn't decoded here on the caller's behalf because "decoding by a sender-reported type" is exactly the pattern status.proto explicitly
// forbids: the type must be decided by the receiver from its own context.
type StatusError struct {
	// Code is the only basis for program decisions.
	Code ipcv1.StatusCode

	// PublicMessage is **only for people to read, never used in program decisions** (status.proto says so explicitly).
	//
	// Don't switch on it, and don't branch localisation on it: nervud generates it from audited templates,
	// and the wording may change at any time. For distinguishable reasons use Code + Detail.
	//
	// **The provider side (ServiceHost) shouldn't set it**: the protocol forbids passing Service/Provider
	// free text through as is, and nervud won't forward strings you write. Handlers wanting to express causes use Detail.
	PublicMessage string

	// Detail is the undecoded typed error_detail bytes; see UnmarshalDetail.
	Detail []byte
}

func (e *StatusError) Error() string {
	if e.PublicMessage != "" {
		return fmt.Sprintf("sdk: %s: %s", e.Code, e.PublicMessage)
	}
	return fmt.Sprintf("sdk: %s", e.Code)
}

// UnmarshalDetail decodes the typed error_detail into m.
//
// The caller is responsible for choosing the right type: ResolveEndpoint failures use *ipcv1.ResolveEndpointErrorDetail,
// interface method failures use that interface's *XxxErrorDetail. Choosing the wrong type decodes garbage rather than erroring
// (protobuf's wire format carries no type identifier), so only call it when **the context is certain**.
//
// Returns false when Detail is empty; returns an error when decoding fails.
func (e *StatusError) UnmarshalDetail(m proto.Message) (bool, error) {
	if len(e.Detail) == 0 {
		return false, nil
	}
	if err := proto.Unmarshal(e.Detail, m); err != nil {
		return false, fmt.Errorf("sdk: decode error_detail: %w", err)
	}
	return true, nil
}

// statusErrorFrom turns a wire Failure into a *StatusError.
//
// When f is nil it gives INTERNAL rather than returning nil: a response whose "outcome is failure but the failure
// itself is empty" is the peer's bug, and treating it as success would hand the caller a zero value as the result.
func statusErrorFrom(f *ipcv1.Failure) *StatusError {
	if f == nil {
		return &StatusError{
			Code:          ipcv1.StatusCode_STATUS_CODE_INTERNAL,
			PublicMessage: "peer sent failure outcome with no Failure body",
		}
	}
	return &StatusError{
		Code:          f.GetCode(),
		PublicMessage: f.GetPublicMessage(),
		Detail:        f.GetErrorDetail(),
	}
}

// IsRetryable reports whether the error is worth retrying **the same** call after backing off.
//
// Deliberately conservative: only UNAVAILABLE and RESOURCE_EXHAUSTED count as retryable.
//
//   - DEADLINE_EXCEEDED does **not** count: the call may already have caused side effects on the other side (motors already moved),
//     and retrying blindly is dangerous. Whether to retry is the caller's decision by method semantics, not this package's.
//   - FAILED_PRECONDITION doesn't count: until the precondition changes, retrying any number of times gives the same result.
//   - NOT_FOUND doesn't count: an invalid endpoint should be Resolved again, not the original call retried.
func IsRetryable(err error) bool {
	var se *StatusError
	if !errors.As(err, &se) {
		return false
	}
	switch se.Code {
	case ipcv1.StatusCode_STATUS_CODE_UNAVAILABLE,
		ipcv1.StatusCode_STATUS_CODE_RESOURCE_EXHAUSTED:
		return true
	default:
		return false
	}
}

// NeedsReResolve reports whether the error means the endpoint binding is invalid and should be Resolved again.
//
// NOT_FOUND means the binding doesn't exist or the target is dead; PERMISSION_DENIED means the permission was revoked
// at run time. The latter will usually be refused on Resolve too, but letting callers take the same recovery path, with
// Resolve giving the exact reason, is better than guessing here.
func NeedsReResolve(err error) bool {
	var se *StatusError
	if !errors.As(err, &se) {
		return false
	}
	switch se.Code {
	case ipcv1.StatusCode_STATUS_CODE_NOT_FOUND,
		ipcv1.StatusCode_STATUS_CODE_PERMISSION_DENIED:
		return true
	default:
		return false
	}
}
