// This file defines the lease handle and the lease itself. A Lease is an immutable value: no field changes after issue,
// and a renewal produces a new value that replaces it whole (deadline changes, ID doesn't), so readers get
// a self-consistent snapshot with one atomic Load, with no risk of reading a half-changed state
package control

import (
	"crypto/rand"
	"encoding/hex"
	"time"

	"github.com/nervus-os/nervud/internal/identity"
	"github.com/nervus-os/nervud/internal/motiongate"
)

// ResourceBaseMain is the base handle for the protocol's implicit default and the legacy ControlSnapshot.
// Other non-empty handles resolved by the catalog need no new constant or branch in control.
const ResourceBaseMain = "base.main"

// ID is the opaque lease handle the system issues (the wire's lease_id).
//
// A fixed-length array rather than a string: a comparable value type that fits in an immutable Lease, and comparing and copying allocate
// nothing on the heap. Check compares it on every motion command, so it must not allocate
type ID [16]byte

// String returns the hex form, for audit and diagnostics. Only call it off hot paths (it allocates)
func (id ID) String() string { return hex.EncodeToString(id[:]) }

// newID generates an unpredictable lease handle.
//
// Not an incrementing counter: handles must be opaque, and predictable ones would make "guess a lease_id and try it" an attack path
// worth trying. crypto/rand.Read never returns an error in current Go versions (it panics itself
// if the entropy source fails), so there's no error branch here: crashing when there's really no entropy is safer than issuing a predictable handle
func newID() ID {
	var id ID
	_, _ = rand.Read(id[:])
	return id
}

// ConnID is the connection / remote session handle a lease is bound to (the wire's owner_connection).
//
// The IPC side mints it at connection admission and guarantees it's unique within the process; 0 is always illegal and used as the unset sentinel.
// Leases can't be transferred: every operation must give both ID and ConnID, and only accepts them if both match
type ConnID uint64

// Lease is one issued control lease.
//
// Immutable: no field is modified in place after issue
type Lease struct {
	ID    ID
	Conn  ConnID
	Class Class

	// Resource is the stable public handle resolved by the catalog; each handle has its own lease slot.
	Resource string
	// ResourceGeneration pins the exact catalog definition that authorized this
	// lease. A stable handle may be reused after an upgrade, but old authority
	// must never carry into the replacement definition.
	ResourceGeneration uint64

	// Owner is the trusted caller identity at issue, used only for audit attribution; permission re-checks don't read it
	//  (permissions are dynamic and re-checked on every call; see the notes on permission.Registry.Allowed)
	Owner identity.Caller

	IssuedAt time.Time
	Deadline time.Time

	// TTL is how long each renewal of this lease extends it. It's kept in the lease rather than looked up in Policy each time: the requester
	// may ask for a TTL shorter than the Policy limit, and renewals must keep using it, or it would grow longer renewal by renewal
	TTL time.Duration

	// Epoch is the monotonic token taken from the global motion epoch allocator at issue. Ordinary boundaries only revoke the same
	// Resource; a Safety Trip's token is a floor across Resources, invalidating every older token.
	Epoch uint64

	// Deadman is the command freshness window; 0 means this lease doesn't require a deadman.
	// With no fresh input within the window the lease is revoked, the epoch incremented, and it goes back to NONE
	Deadman time.Duration
}

// LeaseProof is the immutable subset of a valid Lease that IPC may project
// into a Provider ExecutionContext. It is returned by value so callers cannot
// mutate the authoritative slot snapshot.
type LeaseProof struct {
	ID                 ID
	Class              Class
	Resource           string
	ResourceGeneration uint64
	Deadline           time.Time
	Epoch              uint64
}

// Snapshot is a consistent read-only snapshot of the control plane, for diagnostics and a future IPC observation surface
type Snapshot struct {
	// Source is the effective control source. Always SourceSafety when Safety isn't NORMAL,
	// because the latch state must take precedence over any lease still lingering
	Source Source

	State motiongate.State
	// Epoch is the global motion token allocator's value at sampling time; with several resources it can be greater than Lease.Epoch.
	Epoch uint64

	// When Held is false, Lease is the zero value (currently NONE)
	Held  bool
	Lease Lease
}
