// This file implements operation's LeaseValidator on the ipc side.
//
// # Why it's implemented here, not in operation or control
//
// Validating the lease of a motion operation needs three things at once:
//
//	the wire handle -> control.ID mapping     lives in ipc's conn (connection-scoped)
//	the control module                         injected into ipc at wiring
//	"which resource this lease covers"         answered by control.CheckLease
//
// Only ipc has all three. operation shouldn't know control (dependency direction), and control shouldn't
// know connection-scoped wire handles (a transport-layer concept). So this adapter lands in ipc,
// and main.go injects it into operation.Manager at wiring.
//
// # What it stops
//
// "Using the left arm's lease to move the right arm". The lease_id and the resource are both legal and each passes
// its own check; only looking at them together shows they don't match. In the field this kind of error shows up as
// "the other arm moved", with no log saying the wrong lease was used.
package ipc

import (
	"github.com/nervus-os/nervud/internal/operation"
)

// OperationLeases implements operation.LeaseValidator.
//
// It's a view of *Server rather than a separate type: all the state it needs is on Server,
// and another wrapper would only be one more thing to keep in sync.
type OperationLeases struct {
	s *Server
}

// OperationLeaseValidator returns a validator that can be injected into operation.Manager.
//
// When s is nil the returned validator is always false (fail closed). If the wiring order is wrong,
// motion operations are all refused rather than all allowed.
func (s *Server) OperationLeaseValidator() *OperationLeases {
	return &OperationLeases{s: s}
}

// ValidLease re-checks one motion operation's lease binding.
//
// Any step in doubt returns false. Downstream of this path is "the arm starts moving along a trajectory",
// and its inputs include a connection-scoped handle and a caller-supplied epoch, both of which can go stale
// through timing. Better to refuse one legitimate creation than to allow a motion based on a lapsed authorisation.
func (l *OperationLeases) ValidLease(
	owner operation.ConnHandle, leaseID, epoch uint64, resource string,
) bool {
	if l == nil || l.s == nil || l.s.leases == nil {
		return false
	}
	if leaseID == 0 || resource == "" {
		return false
	}

	// owner must be a connection on this Server. A handle across Servers means nothing, and a failed type assertion
	// here means wiring connected something else; allowing it would authorise motion with a handle
	// of unknown origin.
	co, ok := owner.(*conn)
	if !ok || co == nil || co.s != l.s {
		return false
	}

	id, ok := co.lookupLease(leaseID)
	if !ok {
		// Not a handle this connection holds. It may be forged, or already released;
		// both cases are handled the same.
		return false
	}

	proof, err := l.s.leases.CheckLease(id, co.leaseConnID())
	if err != nil {
		// The lease has expired, been pre-empted, or Safety has stopped the machine. CheckLease folds all of these
		// into an error internally, and this layer needn't tell them apart: none should be allowed.
		return false
	}

	// The resource must match. A valid handle pointing at another resource is "using the left arm's lease to move
	// the right arm": each field is legal on its own, and only together do they show the problem.
	if proof.Resource != resource {
		return false
	}

	// The epoch must be equal, not "not less than". The caller's epoch comes from the one it got
	// at Acquire; a larger current epoch means a Safety takeover or pre-emption happened in between,
	// and that authorisation is void. Allowing it would start a motion based on an old authorisation.
	return proof.Epoch == epoch
}
