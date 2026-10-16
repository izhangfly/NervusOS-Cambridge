package protocheck

import (
	"errors"
	"fmt"

	"github.com/nervus-os/nervus-ipc/protocol/ipcv1"
)

var (
	// ErrNilMethodMeta reports that no authoritative method policy was supplied.
	ErrNilMethodMeta = errors.New("protocheck: nil method metadata")

	// ErrOperationUnsupported means this setup has no Operation Manager.
	//
	// It no longer means "protocol not implemented": the wire contract and the Provider report path are both in place
	//  (nervus.interface.operation.control). The only remaining case is a minimal setup
	// or a test that didn't inject a Manager, and then it must be refused rather than downgraded to an ordinary call:
	// a downgrade would give the caller an OK while the machine is still moving, and it would think the work was done.
	ErrOperationUnsupported = errors.New("protocheck: operation manager is not wired")

	// ErrConfirmationUnauthorized means the caller may not trigger a method that needs user confirmation.
	//
	// This used to be an unconditional refusal (ErrConfirmationUnsupported): no UI could ask the user,
	// so needs_user_confirmation methods were unreachable for every caller, including
	// pkgmanagerd's INSTALL/UNINSTALL/SET_COMPONENT_ENABLED, so installing over IPC
	// was dead altogether.
	//
	// The criterion now is "the caller is the system confirmation UI itself". See GateUserConfirmation
	ErrConfirmationUnauthorized = errors.New("protocheck: caller may not invoke user-confirmed methods")

	// ErrProviderStatus reports an outcome/code combination that a Provider is
	// not authorized to produce.
	ErrProviderStatus = errors.New("protocheck: invalid provider status")
)

// ProviderOutcome identifies the DispatchResult oneof branch being checked.
type ProviderOutcome uint8

const (
	ProviderOutcomeUnspecified ProviderOutcome = iota
	ProviderOutcomeSuccess
	ProviderOutcomeFailure
)

// GateSupport rejects MethodMeta features for which the current IPC contract
// cannot provide end-to-end enforcement. It must run before dispatch.
//
// operationsWired says whether this setup has an Operation Manager. It's a fact about the setup, not
// caller input; making it a parameter rather than a package variable lets tests construct "not wired"
// without leaving production a switch that could be flipped at run time.
//
// This function [only answers what's wired up], not caller eligibility: it runs both before dispatching requests and on
// the Provider report re-check path (ValidateProviderStatus), and the latter has no caller. The eligibility question
// belongs to GateUserConfirmation, checked once at dispatch
func GateSupport(meta *ipcv1.MethodMeta, operationsWired bool) error {
	if meta == nil {
		return ErrNilMethodMeta
	}
	if meta.GetReturnsOperation() && !operationsWired {
		return ErrOperationUnsupported
	}
	return nil
}

// GateUserConfirmation decides a call to a needs_user_confirmation method. It's called once at
// dispatch and not re-checked on the Provider report path.
//
// callerIsConfirmationUI is decided by the caller holding perm.permission.admin. That permission is
// SYSTEM_ONLY + PLATFORM trust + the platform-release signing role, so the holder can only be
// a confirmation UI shipped with the system image (nervus.permissionui, or the App Store in NervusOS Cambridge).
//
// # Why "is the confirmation UI" equals "confirmed"
//
// Just before making this call, the confirmation UI showed the user what's being confirmed and got a nod: it
// is the one asking. Demanding a "user confirmed" credential from it would just have it sign a note
// to itself, adding no guarantee.
//
// Every other caller is refused, and the security boundary is exactly the same as the old unconditional refusal: an ordinary app
// still can't trigger an install directly; it can only ask the confirmation UI to do it. That also makes the confirmation UI
// the mandatory funnel for every confirmation-required operation.
//
// If third-party callers ever need to trigger confirmation-required operations directly, add a one-time credential issued by the confirmation UI
// that the kernel can verify; that builds on this criterion rather than overturning it
func GateUserConfirmation(meta *ipcv1.MethodMeta, callerIsConfirmationUI bool) error {
	if meta == nil {
		return ErrNilMethodMeta
	}
	if meta.GetNeedsUserConfirmation() && !callerIsConfirmationUI {
		return ErrConfirmationUnauthorized
	}
	return nil
}

// ValidateProviderStatus enforces the Provider-owned subset of StatusCode.
//
// Ordinary methods can only complete with OK. ACCEPTED used to belong entirely to the kernel, on the grounds that "the Operation must
// exist before that reply makes sense"; ordering now guarantees that premise: nervud builds the Operation before
// Dispatch and puts the operation_id in the ExecutionContext, so the Provider
// receives the Dispatch with it already in existence.
//
// So ACCEPTED is valid for returns_operation methods, and only for them:
// an ordinary method replying ACCEPTED would be claiming to have created a long task nobody owns.
//
// Identity and permission decisions still belong entirely to the kernel.
func ValidateProviderStatus(
	meta *ipcv1.MethodMeta, outcome ProviderOutcome, code ipcv1.StatusCode,
	operationsWired bool,
) error {
	if err := GateSupport(meta, operationsWired); err != nil {
		return err
	}

	switch outcome {
	case ProviderOutcomeSuccess:
		if code == ipcv1.StatusCode_STATUS_CODE_OK {
			return nil
		}
		if code == ipcv1.StatusCode_STATUS_CODE_ACCEPTED && meta.GetReturnsOperation() {
			return nil
		}
	case ProviderOutcomeFailure:
		switch code {
		case ipcv1.StatusCode_STATUS_CODE_INVALID_ARGUMENT,
			ipcv1.StatusCode_STATUS_CODE_NOT_FOUND,
			ipcv1.StatusCode_STATUS_CODE_FAILED_PRECONDITION,
			ipcv1.StatusCode_STATUS_CODE_RESOURCE_EXHAUSTED,
			ipcv1.StatusCode_STATUS_CODE_DEADLINE_EXCEEDED,
			ipcv1.StatusCode_STATUS_CODE_CANCELLED,
			ipcv1.StatusCode_STATUS_CODE_UNAVAILABLE,
			ipcv1.StatusCode_STATUS_CODE_INTERNAL:
			return nil
		}
	case ProviderOutcomeUnspecified:
	}

	return fmt.Errorf("%w: outcome=%d code=%s", ErrProviderStatus, outcome, code)
}
