// This file is pkgregistry's authoritative in-memory state: after install / uninstall / the boot scan, the single source of truth for which
// Packages are installed and what UID and trust profile they got. identity.Registry
// only keeps a thin projection of ID/UID/Trust from it for IPC admission (see module.go);
// the version, the full manifest and so on are kept only here, once
package pkgregistry

import (
	"errors"
	"fmt"
	"sync/atomic"

	"github.com/nervus-os/nervud/internal/authority"
	"github.com/nervus-os/nervud/internal/identity"
)

// ErrDuplicatePackageID: two Entries in a snapshot share the same Package ID
var ErrDuplicatePackageID = errors.New("pkgregistry: duplicate package id in registry snapshot")

// Entry is the authoritative state of an installed Package
type Entry struct {
	Manifest      Manifest
	ActiveVersion string
	VersionCode   uint64

	// RuntimeGeneration changes on every successful install or replacement of
	// this Package. It is an in-memory kernel token, not a manifest claim. The
	// service instance and IPC identity projections must carry the same value so
	// old code cannot reconnect under a newly published permission set.
	RuntimeGeneration uint64
	UID               uint32
	Trust             identity.TrustProfile
	Source            Source

	// GrantedPermissions is recomputed for every Entry against the exact
	// candidate Catalog before each startup/install/uninstall publication.
	// Persisted values are diagnostic state only and are never boot authority.
	GrantedPermissions []string

	// SignerRoles is populated only from the current successful signature
	// verification. Both system-image and dynamic packages are reverified on
	// boot; role strings are always paired with VerifiedSigners below.
	SignerRoles []string

	// VerifiedSigners and DeveloperRootID are the signer identities used by the
	// central Catalog for definition ownership and GRANT_MODE_SIGNATURE. A role
	// string alone never proves that two packages share a signer.
	VerifiedSigners []VerifiedSigner
	DeveloperRootID string

	// provider is the verified descriptor/schema pair loaded from the exact
	// bytes covered by Manifest.Digests. It is private so Lookup/List callers
	// cannot mutate Catalog input.
	provider *loadedProviderArtifacts

	// DisabledComponents is the set of disabled Component IDs.
	// Disabling is per Component; the service lifecycle and the IPC handshake refuse the component based on it
	DisabledComponents []string
}

// ComponentDisabled reports whether a Component is currently disabled
func (e Entry) ComponentDisabled(compID string) bool {
	for _, id := range e.DisabledComponents {
		if id == compID {
			return true
		}
	}
	return false
}

// Registry is pkgregistry's authoritative in-memory state: Package ID -> Entry.
//
// It copies identity.Registry's copy-on-write + atomic pointer pattern (many reads, few writes: only install /
// uninstall / the boot scan write, everything else reads), with full replacement rather than incremental add/remove. An incremental interface would
// tempt callers into "delete then add", leaving a window where lookups fail, architecturally the same
// problem identity faces.
//
// The zero value isn't usable; it must be built with NewRegistry
type Registry struct {
	snap atomic.Pointer[snapshot]
}

type snapshot struct {
	byID map[string]Entry
}

func NewRegistry() *Registry {
	r := &Registry{}
	r.snap.Store(&snapshot{byID: map[string]Entry{}})
	return r
}

// Replace atomically replaces the whole Registry.
//
// If validation fails the whole thing is refused and the old snapshot stays as it was: better to keep using the last known-good state
// than to load a self-contradictory one (the same reason as identity.Registry.Replace).
//
// UID validation reuses authority.DefaultInvariants.CheckUID rather than keeping another copy of
// the App UID range constants in this package: with two copies, changing only one later would drift silently, and
// pkgregistry already depends on authority's request types (see module.go),
// so depending on this one read-only check function adds no extra coupling
func (r *Registry) Replace(entries []Entry) error {
	next := make(map[string]Entry, len(entries))
	for _, e := range entries {
		if e.Manifest.PackageID == "" {
			return fmt.Errorf("pkgregistry: entry with uid %d has empty package id", e.UID)
		}
		if err := authority.DefaultInvariants().CheckUID(e.UID); err != nil {
			return fmt.Errorf("pkgregistry: package %q: %w", e.Manifest.PackageID, err)
		}
		if !e.Trust.Valid() {
			return fmt.Errorf("pkgregistry: package %q has invalid trust profile %d",
				e.Manifest.PackageID, e.Trust)
		}
		if _, dup := next[e.Manifest.PackageID]; dup {
			return fmt.Errorf("%w: %q", ErrDuplicatePackageID, e.Manifest.PackageID)
		}
		next[e.Manifest.PackageID] = cloneEntry(e)
	}
	r.snap.Store(&snapshot{byID: next})
	return nil
}

// Lookup looks up an Entry by Package ID.
//
// On an uninitialised Registry (a zero &Registry{} or typed nil) it's fail-safe and returns
// "no such package" rather than panicking: a wiring bug shouldn't be amplified into a crash on the startup path,
// for the same reason as identity.Registry.Lookup
func (r *Registry) Lookup(id string) (Entry, bool) {
	if r == nil {
		return Entry{}, false
	}
	snap := r.snap.Load()
	if snap == nil {
		return Entry{}, false
	}
	e, ok := snap.byID[id]
	if !ok {
		return Entry{}, false
	}
	return cloneEntry(e), true
}

// List returns a snapshot copy of every current Entry.
//
// One use is the bulk projection to identity.Registry.Replace after the boot scan (see
// projectIdentity in module.go), another is diagnostics
func (r *Registry) List() []Entry {
	if r == nil {
		return nil
	}
	snap := r.snap.Load()
	if snap == nil {
		return nil
	}
	out := make([]Entry, 0, len(snap.byID))
	for _, e := range snap.byID {
		out = append(out, cloneEntry(e))
	}
	return out
}

func cloneEntry(in Entry) Entry {
	out := in
	out.Manifest = cloneManifest(in.Manifest)
	out.GrantedPermissions = append([]string(nil), in.GrantedPermissions...)
	out.SignerRoles = append([]string(nil), in.SignerRoles...)
	out.VerifiedSigners = append([]VerifiedSigner(nil), in.VerifiedSigners...)
	out.DisabledComponents = append([]string(nil), in.DisabledComponents...)
	// provider is an immutable, package-private value. Sharing it does not expose
	// mutation through Registry Lookup/List.
	return out
}

func cloneManifest(in Manifest) Manifest {
	out := in
	if in.Labels != nil {
		out.Labels = make(map[string]string, len(in.Labels))
		for key, value := range in.Labels {
			out.Labels[key] = value
		}
	}
	out.SupportedABIs = append([]string(nil), in.SupportedABIs...)
	out.Permissions = append([]string(nil), in.Permissions...)
	out.UsesFeatures = append([]Feature(nil), in.UsesFeatures...)
	out.Components = make([]Component, len(in.Components))
	for i, component := range in.Components {
		copyComponent := component
		copyComponent.Exports = append([]Export(nil), component.Exports...)
		copyComponent.Interfaces = append([]string(nil), component.Interfaces...)
		copyComponent.Capabilities = append([]string(nil), component.Capabilities...)
		copyComponent.AddressFamilies = append([]string(nil), component.AddressFamilies...)
		out.Components[i] = copyComponent
	}
	if in.Provider != nil {
		copyProvider := *in.Provider
		out.Provider = &copyProvider
	}
	if in.Digests != nil {
		out.Digests = make(map[string]string, len(in.Digests))
		for path, digest := range in.Digests {
			out.Digests[path] = digest
		}
	}
	return out
}

// Len returns the number of Packages currently in the Registry, for diagnostics and tests
func (r *Registry) Len() int {
	if r == nil {
		return 0
	}
	snap := r.snap.Load()
	if snap == nil {
		return 0
	}
	return len(snap.byID)
}
