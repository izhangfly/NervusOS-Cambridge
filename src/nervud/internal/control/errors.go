// This file defines every refusal reason of the package in one place. Sentinel errors (checkable with errors.Is) rather than free text:
// the layer above decides which StatusCode and which typed error_detail to return by reason, which string matching can't do
package control

import "errors"

// Mapping to the wire (position A is the Envelope's generic StatusCode, position B the typed detail):
//
//	ErrControlNotHeld   FAILED_PRECONDITION + CONTROL_NOT_HELD
//	ErrLeaseExpired     FAILED_PRECONDITION + CONTROL_NOT_HELD (the same to the caller: you don't have control now)
//	ErrDeadmanExpired   FAILED_PRECONDITION + CONTROL_NOT_HELD
//	ErrHeldByHuman      FAILED_PRECONDITION + RESOURCE_BUSY
//	ErrHeldByAI         FAILED_PRECONDITION + RESOURCE_BUSY
//	ErrSafetyLatched    FAILED_PRECONDITION + SAFETY_LATCHED
//	ErrStaleEpoch       FAILED_PRECONDITION + STALE_EPOCH
//	ErrUnknownResource  NOT_FOUND + RESOURCE_NOT_FOUND
//	ErrInvalidRequest   INVALID_ARGUMENT (the request itself is malformed, regardless of current state)
//	ErrPolicyViolation  INVALID_ARGUMENT (the requested TTL/deadman exceeds the system limits)
//
// ErrHeldByHuman and ErrHeldByAI are deliberately separate rather than merged into one BUSY: an Agent needs to know a person
// is remote-controlling right now so it can ask the user "shall I take over?". The decision to give way belongs to the person, and a generic BUSY can't
// express that. When projected to ordinary callers both become RESOURCE_BUSY; the difference exists only on the kernel side and in audit
var (
	// ErrControlNotHeld: no valid lease, or the lease doesn't belong to this (ID, ConnID)
	ErrControlNotHeld = errors.New("control: no valid lease held by caller")

	// ErrLeaseExpired: the lease deadline has passed. Normally the Control Lane collects it first;
	// this error covers the window where expiry and a call happen at the same time
	ErrLeaseExpired = errors.New("control: lease deadline expired")

	// ErrDeadmanExpired: command freshness lapsed; no fresh input within the deadman window
	ErrDeadmanExpired = errors.New("control: deadman expired")

	// ErrHeldByHuman: the target Resource is currently held by HUMAN, and the requester has no right to pre-empt
	ErrHeldByHuman = errors.New("control: resource held by HUMAN")

	// ErrHeldByAI: the target Resource is currently held by AI, and the requester has no right to pre-empt (also AI)
	ErrHeldByAI = errors.New("control: resource held by AI")

	// ErrSafetyLatched: the Safety Gate isn't NORMAL. While latched no ordinary lease is issued
	// and no motion is allowed; recovery must go through OEM Recovery / re-arm, not by requesting control again
	ErrSafetyLatched = errors.New("control: safety gate is latched")

	// ErrStaleEpoch: the motion epoch the lease belongs to has been revoked
	ErrStaleEpoch = errors.New("control: lease belongs to a revoked motion epoch")

	// ErrUnknownResource: the request didn't carry a non-empty Resource handle already resolved by the catalog above
	ErrUnknownResource = errors.New("control: unknown resource scope")

	// ErrResourceCapacity means the process-lifetime slot directory reached its
	// hard Safety scan bound. Restarting clears retired slots; silently growing
	// the directory would make RevokeAll's real-time cost unbounded.
	ErrResourceCapacity = errors.New("control: resource slot capacity exhausted")

	// ErrInvalidRequest: the request itself is malformed: Class unspecified, ConnID 0 and so on
	ErrInvalidRequest = errors.New("control: malformed lease request")

	// ErrPolicyViolation: the requested TTL/deadman exceeds the limits Policy allows.
	//
	// Over the limit is refused rather than silently clamped: silently shortening it would make the caller think it has 5 seconds when it really has
	// 2, so its renewals go out too late and the lease lapses mid-motion, which is far more dangerous than refusing outright
	ErrPolicyViolation = errors.New("control: requested lease exceeds policy limits")

	// ErrShuttingDown: the module has entered shutdown: Stop has been called and the Control Lane is exiting or has exited.
	// No new leases are issued after shutdown, otherwise it would issue an orphan lease with no Lane left to clear its slot on expiry / revoke the epoch,
	// which could keep authorising motion through Check until the process really exits (the consequence of Stop not being final).
	// It's also used as the audit reason for revoking leases at shutdown.
	ErrShuttingDown = errors.New("control: module shutting down")
)

// The sentinels below aren't for callers; they're only revocation reasons in audit records, so offline rules can tell
// normal releases apart from passively losing control
var (
	errConnectionClosed = errors.New("control: owner connection closed")
	errSafetyRevoked    = errors.New("control: revoked by safety trip")
	errResourceRevoked  = errors.New("control: resource definition revoked or replaced")

	// errPackageRevoked is the reason written to audit when RevokeByPackage revokes leases: the package was uninstalled, or its
	// motion-group permission was revoked by the user. Separate from errConnectionClosed,
	// so offline rules can tell passively losing control through a disconnect from revocation due to package lifecycle / permission changes
	errPackageRevoked = errors.New("control: revoked by package lifecycle or permission change")
)
