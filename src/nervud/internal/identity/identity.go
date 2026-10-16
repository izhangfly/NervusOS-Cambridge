// This file is the core of identity resolution: it maps the SO_PEERCRED credentials the kernel gives into a trusted
// Caller. The credentials themselves are read by internal/sysprobe; this package doesn't touch syscall
package identity

import (
	"errors"
	"fmt"
	"sync/atomic"

	"github.com/nervus-os/nervud/internal/sysprobe"
)

var (
	// ErrUnknownUID: this UID doesn't correspond to any registered Package
	//
	// It must be kept apart from an out-of-range UID: out of range is an invariant violation (authority.CheckUID),
	// while this error means the range is valid but no such package exists, usually because the Package was uninstalled while the process is still alive,
	// or someone created a system user by hand that falls in the App range
	ErrUnknownUID = errors.New("identity: uid maps to no registered package")

	// ErrDuplicateUID: two Packages in the snapshot share one UID
	//
	// That isn't a tolerable configuration issue. If every Package shared one UID,
	// SO_PEERCRED couldn't provide Package-level isolation,
	// so it's always forbidden. Once it happens, a UID no longer identifies the caller uniquely and every conclusion of this package
	// loses its meaning, so it's refused when loading the snapshot rather than discovered at resolution time
	ErrDuplicateUID = errors.New("identity: duplicate uid in package snapshot")

	// ErrDuplicateID: two UIDs in the snapshot map to the same Package ID
	//
	// The mirror of ErrDuplicateUID: Packages and UIDs must correspond one to one.
	// Only checking that UIDs are unique can't stop the same Package ID appearing under two UIDs, which would make
	// permission ownership, Endpoint ownership and audit attribution all point at an ambiguous Package
	ErrDuplicateID = errors.New("identity: duplicate package id in snapshot")
)

// TrustProfile is the trust and permission profile
//
// It isn't a third kind of software: only platform/OEM-signed Packages in the read-only system image qualify for
// a non-Ordinary profile; a dynamically installed developer-signed Package is still Ordinary even if it declares itself a Service.
// A manifest can't declare itself system
type TrustProfile uint8

const (
	// TrustUnspecified is the zero value and never a valid conclusion.
	// Same reason as proto's *_UNSPECIFIED = 0: a missing value must fail closed,
	// not silently get some real profile just because nothing was assigned
	TrustUnspecified TrustProfile = iota
	TrustOrdinary
	TrustOEM
	TrustPlatform
)

func (t TrustProfile) String() string {
	switch t {
	case TrustOrdinary:
		return "ordinary"
	case TrustOEM:
		return "oem"
	case TrustPlatform:
		return "platform"
	default:
		return "unspecified"
	}
}

// Valid reports whether t is a defined profile
func (t TrustProfile) Valid() bool {
	return t == TrustOrdinary || t == TrustOEM || t == TrustPlatform
}

// Package holds the Package facts needed for identity resolution
//
// Only fields identity resolution uses. Version, file list, permission set and so on belong to the Package Registry
// and aren't copied here; two copies would drift
type Package struct {
	ID         string
	UID        uint32
	Trust      TrustProfile
	Generation uint64
}

// Caller is a connection's trusted identity
//
// UID/GID/PID come from the Linux kernel and PackageID/Trust from a Registry lookup;
// none of them comes from any bytes the peer sent
type Caller struct {
	PackageID string

	// Generation identifies the exact installed runtime that this connection
	// was admitted for. Package IDs and UIDs are intentionally stable across an
	// upgrade, so neither can distinguish an old process from its replacement.
	Generation uint64

	// ComponentID is empty at resolution time: when the connection is first set up we only know which Package it is,
	// not which of its Components. It's filled in during the handshake after checking the value claimed in Hello,
	// because nervud must verify claims rather than trust what the client says
	ComponentID string

	Trust TrustProfile

	UID uint32
	GID uint32

	// A PID is reclaimed by the kernel when the process exits; for diagnostics and log correlation only.
	// Don't use it long after the connection was set up to look things up in /proc and trust the result
	PID int32
}

// String produces the audit attribution string, in the same format as authority.Subject.String,
// so audit records written by the two packages can be processed by the same offline analysis rules
func (c Caller) String() string {
	if c.PackageID == "" {
		return "kernel"
	}
	return fmt.Sprintf("pkg:%s uid:%d", c.PackageID, c.UID)
}

// Registry is the UID -> Package index
//
// Read-heavy: each new connection reads once, while writes only happen on install/uninstall. So it uses copy-on-write +
// an atomic pointer with completely lock-free reads, avoiding every new connection stalling while an install holds a write lock
//
// The zero value isn't usable; construct it with NewRegistry
type Registry struct {
	snap atomic.Pointer[snapshot]
}

type snapshot struct {
	byUID map[uint32]Package
}

func NewRegistry() *Registry {
	r := &Registry{}
	r.snap.Store(&snapshot{byUID: map[uint32]Package{}})
	return r
}

// Replace atomically replaces the whole index
//
// A full replacement rather than incremental changes: the Package Registry gives a consistent snapshot after committing a transaction,
// and an incremental interface would tempt callers to delete then add, with a window in between in which a UID can't be found;
// connections landing in that window would be misjudged as unknown UIDs
//
// A failed check rejects the whole thing and keeps the old snapshot: better to keep using the last known-good index
// than to load a self-contradictory one
func (r *Registry) Replace(pkgs []Package) error {
	next := make(map[uint32]Package, len(pkgs))
	seenID := make(map[string]uint32, len(pkgs))
	for _, p := range pkgs {
		switch {
		case p.ID == "":
			return fmt.Errorf("identity: package with uid %d has empty id", p.UID)
		case p.UID == 0:
			// UID 0 is Linux's special root identity, and App UID 0 is always forbidden.
			// Blocked again here because the Registry's content comes from the install flow, and no slip anywhere in
			// that flow should let root become a valid caller that can be resolved
			return fmt.Errorf("identity: package %q has uid 0", p.ID)
		case !p.Trust.Valid():
			return fmt.Errorf("identity: package %q has invalid trust profile %d", p.ID, p.Trust)
		case p.Generation == 0:
			return fmt.Errorf("identity: package %q has zero runtime generation", p.ID)
		}
		if prev, dup := next[p.UID]; dup {
			return fmt.Errorf("%w: uid %d claimed by both %q and %q",
				ErrDuplicateUID, p.UID, prev.ID, p.ID)
		}
		if prevUID, dup := seenID[p.ID]; dup {
			return fmt.Errorf("%w: package %q maps to both uid %d and %d",
				ErrDuplicateID, p.ID, prevUID, p.UID)
		}
		next[p.UID] = p
		seenID[p.ID] = p.UID
	}
	r.snap.Store(&snapshot{byUID: next})
	return nil
}

// Lookup finds a Package by UID
//
// Lock-free: one atomic Load plus one map lookup. This path runs once per new connection,
// and with Packages numbering tens to low hundreds, the whole table is small enough to stay in CPU cache
//
// An uninitialised Registry (a &Registry{} not made with NewRegistry, even a typed nil)
// is always treated as an empty index returning not-found, never a panic. A wiring error should show up as recognising nobody and
// refusing everyone, a fail-safe state; the first connection must never crash the whole accept path,
// which would amplify a wiring bug into a denial of service
func (r *Registry) Lookup(uid uint32) (Package, bool) {
	if r == nil {
		return Package{}, false
	}
	snap := r.snap.Load()
	if snap == nil {
		return Package{}, false
	}
	p, ok := snap.byUID[uid]
	return p, ok
}

// Len returns the number of Packages in the current index, for diagnostics and tests
func (r *Registry) Len() int {
	if r == nil {
		return 0
	}
	snap := r.snap.Load()
	if snap == nil {
		return 0
	}
	return len(snap.byUID)
}

// Resolve resolves kernel credentials into a trusted identity
//
// It only answers which Package this UID belongs to. Whether the UID is inside the App range is
// authority.Invariants' job, done by the caller before this; that check is a system-wide invariant
// and shouldn't be implemented a second time here
func (r *Registry) Resolve(cred sysprobe.Ucred) (Caller, error) {
	p, ok := r.Lookup(cred.UID)
	if !ok {
		return Caller{}, fmt.Errorf("%w: uid %d", ErrUnknownUID, cred.UID)
	}
	return Caller{
		PackageID:  p.ID,
		Generation: p.Generation,
		Trust:      p.Trust,
		UID:        cred.UID,
		GID:        cred.GID,
		PID:        cred.PID,
	}, nil
}
