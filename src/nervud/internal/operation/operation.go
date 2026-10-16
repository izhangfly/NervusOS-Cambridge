// This file is the Operation data model and state machine: the State enum, the Origin binding, the Operation record,
// outward events, and the single authoritative legal-transition check canTransition. The state machine is this package's correctness
// core, independent of concurrency or wiring, and lives in its own file so unit tests can enumerate every transition.
package operation

import (
	"errors"
	"time"

	ipcv1 "github.com/nervus-os/nervus-ipc/protocol/ipcv1"

	"github.com/nervus-os/nervud/internal/identity"
)

// State is an Operation's lifecycle state.
//
// The zero value StateUnspecified is never a legal conclusion: for the same reason as every *_UNSPECIFIED=0 enum,
// a missing value must fail closed, never silently land in some real state just because it wasn't set.
type State uint8

const (
	// StateUnspecified is the zero-value sentinel, never a legal state
	StateUnspecified State = iota
	StatePending
	StateRunning
	StateCancelRequested
	StateSucceeded
	StateFailed
	StateCancelled
)

func (s State) String() string {
	switch s {
	case StatePending:
		return "pending"
	case StateRunning:
		return "running"
	case StateCancelRequested:
		return "cancel_requested"
	case StateSucceeded:
		return "succeeded"
	case StateFailed:
		return "failed"
	case StateCancelled:
		return "cancelled"
	default:
		return "unspecified"
	}
}

// Terminal reports whether s is terminal. No transition is accepted after a terminal state.
func (s State) Terminal() bool {
	return s == StateSucceeded || s == StateFailed || s == StateCancelled
}

// canTransition is the single authoritative legal-transition check.
// Everything else is illegal; the caller refuses it and records audit.
//
// The only allowed set:
//
//	PENDING -> RUNNING | CANCEL_REQUESTED | FAILED
//	RUNNING -> CANCEL_REQUESTED | SUCCEEDED | FAILED
//	CANCEL_REQUESTED -> SUCCEEDED | FAILED | CANCELLED
//	(terminal states have no outgoing edges)
//
// Note: PENDING -> SUCCEEDED isn't allowed (a real success must pass through RUNNING), and neither is PENDING ->
// CANCELLED (a cancel must pass through CANCEL_REQUESTED). The state diagram's rejectBeforeStart branch does draw
// PENDING -> CANCELLED, but the authoritative list wins:
// reject-before-start goes PENDING -> FAILED.
func canTransition(from, to State) bool {
	switch from {
	case StatePending:
		return to == StateRunning || to == StateCancelRequested || to == StateFailed
	case StateRunning:
		return to == StateCancelRequested || to == StateSucceeded || to == StateFailed
	case StateCancelRequested:
		return to == StateSucceeded || to == StateFailed || to == StateCancelled
	default:
		// Terminal states and Unspecified have no legal outgoing edges
		return false
	}
}

// OriginBinding is the trusted type information bound when an operation is created.
//
// It lets the SDK resume decoding typed progress/result/error after a reconnect: the Protobuf type of the detail
// is decided by this binding, never by google.protobuf.Any or a client-supplied type URL.
// Get and event projections must return it.
type OriginBinding struct {
	InterfaceID string
	IfaceMajor  uint32
	IfaceMinor  uint32
	MethodID    uint32
	SchemaHash  []byte // consistent with the RobotCatalog/endpoint projection
}

// ConnHandle is the opaque identity of the IPC connection that owns this operation, satisfied by internal/ipc's
// *conn (copying the existing endpoint.ConnHandle idiom).
//
// This package doesn't know or need to know conn's concrete shape (avoiding a reverse dependency on ipc); it only needs to be comparable,
// for converging unfinished operations per connection on disconnect (ReleaseByConn) and visibility-independent cleanup.
// The zero value nil = created internally by the system (no owning connection).
type ConnHandle any

// Operation is the authoritative record of one system-coordinated long task.
//
// v1 is in memory and not persisted across nervud restarts. The terminal payload (TerminalStatus/Result/Error)
// is non-empty only in terminal states; its type is decided by the Origin binding, never Any.
type Operation struct {
	ID          uint64          // assigned by nervud, host-session scoped, monotonic, never reused
	Origin      OriginBinding   // trusted type binding
	Caller      identity.Caller // creator identity, for visibility decisions and audit attribution
	Resources   []string        // bound resource_handle set (usually one, {arm.main}, in v1)
	LeaseID     uint64          // associated ControlLease (required for motion; 0 = non-motion, no lease)
	MotionEpoch uint64          // the motion epoch at creation, for Safety supersede staleness checks
	Deadline    time.Time       // finite lifetime; still unfinished at that point -> FAILED(DEADLINE_EXCEEDED)
	State       State

	// Terminal payload (non-empty only in terminal states)
	TerminalStatus ipcv1.StatusCode // OK (-> SUCCEEDED) / CANCELLED / anything else (-> FAILED)
	TerminalResult []byte           // optional typed success result (SUCCEEDED)
	TerminalError  []byte           // optional typed error detail (FAILED)

	CreatedAt time.Time
	UpdatedAt time.Time

	// conn is the caller connection that owns this operation. Unexported, so outside snapshots can't see it;
	// it's only used by ReleaseByConn's per-connection convergence. nil when created internally by the system.
	conn ConnHandle

	// providerConn is the executing connection, i.e. who nervud dispatched this call to.
	//
	// It's the ownership credential on the reporting side: a Provider can only Accept/Progress/Complete
	// what nervud dispatched to it. Without this binding, any system service could report someone else's
	// operation as failed, and the caller would see a "normal" failure, with even its detailed cause
	// forged.
	//
	// Operations carried by built-in endpoints have no external executor, so it's nil.
	providerConn ConnHandle
}

// clone returns a deep copy safe to hand out: slice fields are independent, so caller changes don't write back to internal state.
func (o *Operation) clone() Operation {
	cp := *o
	cp.Origin.SchemaHash = cloneBytes(o.Origin.SchemaHash)
	cp.Resources = cloneStrings(o.Resources)
	cp.TerminalResult = cloneBytes(o.TerminalResult)
	cp.TerminalError = cloneBytes(o.TerminalError)
	return cp
}

func cloneBytes(b []byte) []byte {
	if len(b) == 0 {
		return nil
	}
	return append([]byte(nil), b...)
}

func cloneStrings(s []string) []string {
	if len(s) == 0 {
		return nil
	}
	return append([]string(nil), s...)
}

// EventKind distinguishes the kinds of event on a subscription channel (*_UNSPECIFIED=0 fail-closed).
type EventKind uint8

const (
	// EventKindUnspecified is the zero-value sentinel
	EventKindUnspecified EventKind = iota
	// EventProgress is a typed progress event; it doesn't change state (Provider.Progress)
	EventProgress
	// EventState is a state transition (including terminal ones)
	EventState
)

// Event is one event projected to the SDK on a subscription channel.
//
// It carries the Origin binding, so the SDK can resume decoding typed payload/result/error after a reconnect.
type Event struct {
	OperationID uint64
	Kind        EventKind
	State       State // the current state (meaningful for EventState; for EventProgress, the state before any transition)
	Origin      OriginBinding
	Payload     []byte // EventProgress's typed progress payload

	// Terminal payload (non-empty only for EventState with a terminal State)
	TerminalStatus ipcv1.StatusCode
	TerminalResult []byte
	TerminalError  []byte
}

// Sentinel errors shared outward and inside the package. Provider reports and internal transitions use them to tell failure causes apart,
// for offline audit analysis and caller branching.
var (
	// ErrNotFound means the id doesn't exist or isn't visible to the caller. Both cases use the same error,
	// so whether someone else's operation exists isn't leaked.
	ErrNotFound = errors.New("operation: not found")

	// ErrIllegalTransition: the requested transition isn't in the allowed set (refused after recording audit).
	ErrIllegalTransition = errors.New("operation: illegal state transition")

	// ErrAlreadyTerminal: the target operation is already terminal, and the terminal state is written once (idempotent / audited).
	ErrAlreadyTerminal = errors.New("operation: already in a terminal state")

	// ErrStaleEpoch: the motion epoch the Provider reported doesn't match the one bound at creation;
	// the world has crossed a revocation boundary beneath it (Safety trip / lease change), so it's refused fail-closed.
	ErrStaleEpoch = errors.New("operation: motion epoch is stale")

	// ErrSuperseded: the operation was born before a motion epoch that a Safety/revocation boundary has crossed,
	// and the system has already taken it over as FAILED; any further progress from the Provider is refused synchronously.
	ErrSuperseded = errors.New("operation: superseded by a revocation boundary")

	// ErrDeadlineExceeded: the operation's deadline has passed and it has been converged to FAILED;
	// any further progress from the Provider is refused synchronously.
	ErrDeadlineExceeded = errors.New("operation: deadline exceeded")

	// ErrProviderContract: the Provider broke the reporting contract (such as passing Fail a decision code only nervud
	// can produce, or an unknown/out-of-bounds enum). The system fails closed by normalising it to INTERNAL
	// and audits it as a contract violation.
	ErrProviderContract = errors.New("operation: provider contract violation")

	// ErrNotRunning: Progress can only be reported during RUNNING/CANCEL_REQUESTED.
	ErrNotRunning = errors.New("operation: not in a running state")

	// ErrShuttingDown: the Manager has shut down and accepts no new operations.
	ErrShuttingDown = errors.New("operation: manager is shutting down")
)
