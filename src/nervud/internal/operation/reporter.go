// This file is the Provider reporting seam: Providers report progress and terminal states through nervud (the dispatch/provider loop),
// and nervud stores them after checking that the transition is legal and the origin/epoch binding holds. Providers
// don't own the final system Operation state and can't keep controlling resources after the system
// cancels; this file's transition checks and write-once terminal state (CAS) are where that hard rule is enforced.
package operation

import (
	ipcv1 "github.com/nervus-os/nervus-ipc/protocol/ipcv1"
)

// ProviderReporter is the narrow interface operation defines for the dispatch/provider loop to hold
// (*Manager satisfies it implicitly). Every method stores only after nervud checks: the Provider only reports
// facts, and the system decides the state.
type ProviderReporter interface {
	// Accept: the Provider accepted and started executing -> RUNNING. epoch must match the MotionEpoch bound at
	// creation, otherwise the revocation generation the operation was born in has expired, and it fails closed.
	Accept(id, epoch uint64) error
	// Progress: a typed progress event that doesn't change state (only during RUNNING/CANCEL_REQUESTED).
	Progress(id uint64, payload []byte) error
	// Succeed: a real success -> SUCCEEDED (terminal_status=OK, optional typed result).
	Succeed(id uint64, result []byte) error
	// Fail: failure/device fault -> FAILED (terminal_status not OK and not CANCELLED, optional detail).
	Fail(id uint64, code ipcv1.StatusCode, detail []byte) error
	// Cancelled: the Provider confirms it cancelled and stopped controlling the resource -> CANCELLED.
	Cancelled(id uint64) error
}

// Compile-time assertion: *Manager satisfies ProviderReporter.
var _ ProviderReporter = (*Manager)(nil)

// Accept moves an operation from PENDING to RUNNING.
//
// epoch check: the Provider reports the motion epoch it started executing in. If it doesn't match the
// MotionEpoch bound at creation, the world has crossed a revocation boundary (Safety trip / lease change),
// and this operation should no longer enter RUNNING: it fails directly with ErrStaleEpoch, enforcing
// "no controlling resources after the system cancels".
func (m *Manager) Accept(id, epoch uint64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	op, ok := m.ops[id]
	if !ok {
		return ErrNotFound
	}
	if op.State.Terminal() {
		return ErrAlreadyTerminal
	}
	if epoch != op.MotionEpoch {
		// Stale epoch: converge to failure, don't enter RUNNING.
		m.terminateLocked(op, StateFailed, ipcv1.StatusCode_STATUS_CODE_FAILED_PRECONDITION, nil,
			actionFailed, ErrStaleEpoch)
		return ErrStaleEpoch
	}
	// Pre-emption check: even if the epoch matches creation, the world may have crossed a later revocation boundary, or this
	// operation may be past its deadline. In either case never enter RUNNING; converge to FAILED on the spot.
	if code, pcause, ok := m.supersededOrExpiredLocked(op); ok {
		m.terminateLocked(op, StateFailed, code, nil, actionFor(code), pcause)
		return pcause
	}
	if !canTransition(op.State, StateRunning) {
		m.record(actionIllegal, op, true, ErrIllegalTransition)
		return ErrIllegalTransition
	}
	m.transitionLocked(op, StateRunning)
	m.record(actionAccepted, op, false, nil)
	return nil
}

// Progress delivers one typed progress event without changing state. Only legal during RUNNING/CANCEL_REQUESTED
// (PENDING hasn't started and terminal states have ended, so there's no progress to speak of).
func (m *Manager) Progress(id uint64, payload []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	op, ok := m.ops[id]
	if !ok {
		return ErrNotFound
	}
	if op.State != StateRunning && op.State != StateCancelRequested {
		return ErrNotRunning
	}
	m.emitLocked(op, Event{
		OperationID: op.ID,
		Kind:        EventProgress,
		State:       op.State,
		Origin:      op.Origin,
		Payload:     cloneBytes(payload),
	})
	return nil
}

// Succeed ends with a real success -> SUCCEEDED. It may finish early from RUNNING or CANCEL_REQUESTED,
// because the device may have completed before the cancel arrived.
func (m *Manager) Succeed(id uint64, result []byte) error {
	return m.reportTerminal(id, StateSucceeded, ipcv1.StatusCode_STATUS_CODE_OK, result, actionSucceeded, nil)
}

// Fail ends with failure/device fault -> FAILED. code must be a failure reason a Provider may legitimately produce;
// any out-of-bounds or illegal code fails closed and is normalised to INTERNAL, never silently
// treated as success or cancellation, and the Provider can't impersonate decisions only nervud may make. When normalisation happens,
// reportTerminal audits it as a Provider Contract violation.
func (m *Manager) Fail(id uint64, code ipcv1.StatusCode, detail []byte) error {
	norm := code
	var contractViolation error
	if !providerFailCode(norm) {
		norm = ipcv1.StatusCode_STATUS_CODE_INTERNAL
		contractViolation = ErrProviderContract
	}
	return m.reportTerminal(id, StateFailed, norm, detail, actionFailed, contractViolation)
}

// providerFailCode reports whether code is a failure reason a Provider may legitimately produce in Fail.
// An allowlist that fails closed: it only lets through generic states that clearly belong to "device/execution-side failure".
//   - OK/CANCELLED/UNSPECIFIED: not failure reasons (respectively success, a cancel decision, the zero sentinel).
//   - PERMISSION_DENIED/UNAUTHENTICATED: authorisation decisions only nervud may produce; a Provider
//     impersonating one is a contract violation.
//   - ACCEPTED: a delivery receipt, not a terminal reason.
//   - Unknown/future enums: never let through (anything outside the allowlist is refused).
func providerFailCode(code ipcv1.StatusCode) bool {
	switch code {
	case ipcv1.StatusCode_STATUS_CODE_INTERNAL,
		ipcv1.StatusCode_STATUS_CODE_INVALID_ARGUMENT,
		ipcv1.StatusCode_STATUS_CODE_FAILED_PRECONDITION,
		ipcv1.StatusCode_STATUS_CODE_DEADLINE_EXCEEDED,
		ipcv1.StatusCode_STATUS_CODE_UNAVAILABLE,
		ipcv1.StatusCode_STATUS_CODE_RESOURCE_EXHAUSTED,
		ipcv1.StatusCode_STATUS_CODE_NOT_FOUND:
		return true
	default:
		return false
	}
}

// Cancelled: the Provider confirms it cancelled and stopped controlling the resource -> CANCELLED. Only CANCEL_REQUESTED
// can be confirmed as cancelled (canTransition guarantees it: RUNNING straight to Cancelled is an illegal transition, refused and audited).
func (m *Manager) Cancelled(id uint64) error {
	return m.reportTerminal(id, StateCancelled, ipcv1.StatusCode_STATUS_CODE_CANCELLED, nil, actionCancelled, nil)
}

// reportTerminal is the shared implementation of the three terminal reports, enforcing "the terminal state is written once":
//
//   - Already the same terminal state: idempotent no-op, returns nil.
//   - Already a different terminal state: terminal states can't be overwritten; records audit and returns ErrAlreadyTerminal
//     (concurrent Succeed/Fail/Cancelled get a single winner decided by mu; losers end up here).
//   - Past a revocation boundary / past the deadline: late Provider progress is never allowed to succeed; it's synchronously
//     converged to FAILED and the reason returned (see the "pre-emption check" below).
//   - Illegal transition (such as RUNNING -> CANCELLED): records audit and returns ErrIllegalTransition.
//   - Otherwise: CAS-write the terminal state (terminateLocked) and return nil.
//
// The pre-emption check is this function's key line of defence: SafetySupersede and deadline convergence both happen asynchronously in the background,
// leaving a window between "the boundary has been crossed" and "the background scan has seen it". Without a synchronous re-check here, a late
// Succeed in that window would write the operation as SUCCEEDED, and the background would then skip it as already terminal.
// That would let one late success report break through Safety / lease revocation / the deadline. So here, holding the lock
// and before the operation has finished, any operation that should be taken over or has timed out is converged to FAILED on the spot, never allowed
// to succeed. That way, whether or not the background has seen it, the synchronous path fails closed first.
func (m *Manager) reportTerminal(id uint64, to State, status ipcv1.StatusCode, detail []byte, action string, cause error) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	op, ok := m.ops[id]
	if !ok {
		return ErrNotFound
	}
	if op.State == to {
		return nil // idempotent: a repeated report of the same terminal state is a no-op
	}
	if op.State.Terminal() {
		m.record(actionIllegal, op, true, ErrAlreadyTerminal)
		return ErrAlreadyTerminal
	}
	// Pre-emption check (before accepting any Provider terminal state): the motion world may have crossed a revocation boundary beneath this
	// operation, or it may be past its deadline. In either case, whether the Provider reports success
	// or failure, the system's conclusion must be FAILED: converge on the spot and don't let a latecomer rewrite the decision.
	if code, pcause, ok := m.supersededOrExpiredLocked(op); ok {
		m.terminateLocked(op, StateFailed, code, nil, actionFor(code), pcause)
		return pcause
	}
	if !canTransition(op.State, to) {
		m.record(actionIllegal, op, true, ErrIllegalTransition)
		return ErrIllegalTransition
	}
	m.terminateLocked(op, to, status, detail, action, terminalCauseFor(to, cause))
	return nil
}

// supersededOrExpiredLocked decides whether op should be taken over by the system right now (returning the convergence code +
// reason + true). Order: the Safety revocation boundary first (strongest), then the deadline. Requires holding mu.
func (m *Manager) supersededOrExpiredLocked(op *Operation) (ipcv1.StatusCode, error, bool) {
	// Motion, and born before the current revocation boundary -> taken over by Safety/lease revocation.
	if op.LeaseID != 0 {
		if boundary := m.supersedeEpoch.Load(); boundary != 0 && op.MotionEpoch < boundary {
			return ipcv1.StatusCode_STATUS_CODE_FAILED_PRECONDITION, ErrSuperseded, true
		}
	}
	// The deadline has passed.
	if !op.Deadline.IsZero() && m.now().After(op.Deadline) {
		return ipcv1.StatusCode_STATUS_CODE_DEADLINE_EXCEEDED, ErrDeadlineExceeded, true
	}
	return ipcv1.StatusCode_STATUS_CODE_UNSPECIFIED, nil, false
}

// actionFor maps a pre-emption convergence code to the matching audit Action, named the same as the background convergence path.
func actionFor(code ipcv1.StatusCode) string {
	if code == ipcv1.StatusCode_STATUS_CODE_DEADLINE_EXCEEDED {
		return actionDeadlineExceeded
	}
	return actionSafetySuperseded
}

// terminalCauseFor sets the audit's Denied/Err only for abnormal endings (FAILED); normal
// SUCCEEDED/CANCELLED don't count as refused. A non-nil cause (such as a Provider contract violation) is passed through first,
// otherwise FAILED gets the generic provider-failure reason.
func terminalCauseFor(to State, cause error) error {
	if cause != nil {
		return cause
	}
	if to == StateFailed {
		return errProviderFailed
	}
	return nil
}

var errProviderFailed = errStr("provider reported failure")
