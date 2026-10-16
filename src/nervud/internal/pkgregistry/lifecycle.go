// This file implements uninstall and disable/enable, sharing Module.mu with install.go for
// serialisation, so uninstall/disable never interleave with install and the three projections never split.
package pkgregistry

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/nervus-os/nervud/internal/audit"
	"github.com/nervus-os/nervud/internal/authority"
	"github.com/nervus-os/nervud/internal/identity"
)

var (
	// ErrPackageNotInstalled: uninstalling/disabling a Package that isn't installed
	ErrPackageNotInstalled = errors.New("pkgregistry: package not installed")
	// ErrComponentNotFound: the target Component isn't in the Package's manifest
	ErrComponentNotFound = errors.New("pkgregistry: component not found")
	// ErrComponentProtected: the target Component is on the protected list or declared not disableable, so disabling is refused
	ErrComponentProtected = errors.New("pkgregistry: component cannot be disabled")
	// ErrSystemPackageImmutable: a Package from the system image can't be uninstalled dynamically (it follows whole-image OTA)
	ErrSystemPackageImmutable = errors.New("pkgregistry: system-image package cannot be uninstalled")
	// ErrPackageRemovalPending blocks install/lifecycle mutations while a prior
	// uninstall is waiting for its idempotent filesystem cleanup to complete.
	ErrPackageRemovalPending = errors.New("pkgregistry: package removal is pending")
)

// isProtectedComponent reports whether "<pkg>/<comp>" is on the compile-time hard-coded list of components that can't be disabled.
// The reason is the same as the central catalog's built-in permission baseline: this protection rule can't be decided by filesystem write
// permissions, so it's a switch in code rather than data that could be changed. Disabling the Settings app that provides the disable UI
// itself, the permission confirmation channel, or session / package management / safety recovery would all cost the system its ability to repair itself. A function rather than
// a package-level map avoids gochecknoglobals and also makes it plainer that the list is code
func isProtectedComponent(pkgSlashComp string) bool {
	switch pkgSlashComp {
	case "nervus.pkgmanagerd/main",
		"nervus.settings/main",     // whatever provides the disable UI can't be disabled itself
		"nervus.permissionui/main", // the permission confirmation channel can't be disabled
		"nervus.sessiond/main",
		"nervus.safety.recovery/main":
		return true
	default:
		return false
	}
}

// CanDisable reports whether a Component can be disabled, and the reason when it can't.
//
// Ordinary packages: users have full control over what they installed, so they can always be disabled (a manifest declaring
// disableable:false has no effect, as in Install). System packages: only if they're not on the protected list and the manifest explicitly
// declares disableable:true
func (e Entry) CanDisable(compID string) (bool, string) {
	if _, ok := e.Manifest.Component(compID); !ok {
		return false, "component not found"
	}
	if e.Trust == identity.TrustOrdinary {
		return true, ""
	}
	if isProtectedComponent(e.Manifest.PackageID + "/" + compID) {
		return false, "protected"
	}
	c, _ := e.Manifest.Component(compID)
	if !c.Disableable {
		return false, "not disableable"
	}
	return true, ""
}

// ComponentStopper is the narrow interface to service.Manager for uninstall/disable/upgrade: stop a component's
// running instance, or switch the whole package to the new version after an upgrade. Skipped when nil (a seam left for the stage when endpoint/service
// weren't wired yet)
type ComponentStopper interface {
	StopComponent(ctx context.Context, pkg, comp string) error
	// ReloadPackage stops every old instance of the package and restarts always-on components with the new version from the current Registry
	//  (for upgrades, so the old version doesn't keep running / restarting)
	ReloadPackage(ctx context.Context, pkg string) error
}

// LeaseRevoker is the narrow interface to control for uninstall/revocation: revoke every ControlLease held by a Package
// (control increments the motion epoch if motion is involved). Skipped when nil (wired in Step 9)
type LeaseRevoker interface {
	RevokeByPackage(pkgID string) error
}

// PackageTransferRevoker is the narrow control/data-plane lifecycle hook. It
// avoids a dependency on concrete IPC or transfer implementation types.
type PackageTransferRevoker interface {
	RevokePackage(packageID string)
	RevokeResource(resourceHandle string, generation uint64)
}

// SetLifecycleHooks injects the external collaborators uninstall/disable need (service stops components, control revokes leases).
// Called by main.go at wiring; either may be nil (seams left for stages where they aren't wired)
func (m *Module) SetLifecycleHooks(stopper ComponentStopper, revoker LeaseRevoker) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stopper = stopper
	m.revoker = revoker
}

// SetTransferRevoker injects the optional fast-data-plane revocation hook.
func (m *Module) SetTransferRevoker(revoker PackageTransferRevoker) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.transferRevoker = revoker
}

// SetComponentEnabled disables/enables a Component and persists it.
//
// What disabling covers (all required; this function handles persistence + projection + stopping running instances, while refusing the component at the IPC handshake
// by DisabledComponents is done on the ipc/verifyComponent side):
//   - update registryState.DisabledComponents and write it to disk atomically
//   - Replace the three projections (DisabledComponents goes into the Entry, so it still holds after a scan restart)
//   - stop the running instance (through ComponentStopper)
//
// Disabling is per Component and doesn't reclaim the Package UID. Enabling is always reversible
func (m *Module) SetComponentEnabled(ctx context.Context, pkgID, compID string, enabled bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	e, ok := m.registry.Lookup(pkgID)
	if !ok {
		return fmt.Errorf("%w: %q", ErrPackageNotInstalled, pkgID)
	}
	if _, ok := e.Manifest.Component(compID); !ok {
		return fmt.Errorf("%w: %s/%s", ErrComponentNotFound, pkgID, compID)
	}

	if !enabled {
		if can, why := e.CanDisable(compID); !can {
			err := fmt.Errorf("%w: %s/%s (%s)", ErrComponentProtected, pkgID, compID, why)
			m.aud.Record(ctx, audit.Event{Action: "pkgregistry.SetComponentEnabled", Subject: pkgID, Denied: true, Err: err})
			return err
		}
	}

	// Compute the new DisabledComponents set
	next := setMembership(e.DisabledComponents, compID, !enabled)

	// Persist: write the bookkeeping to disk first, then Replace the in-memory projection (if the disk write fails, memory isn't touched)
	st, ok := m.readState(pkgID)
	if !ok {
		return fmt.Errorf("%w: %q (no ledger)", ErrPackageNotInstalled, pkgID)
	}
	st.DisabledComponents = next
	if err := saveRegistryState(m.stateDir, st); err != nil {
		return err
	}

	e.DisabledComponents = next
	if err := m.replaceEntry(e); err != nil {
		return err
	}

	// Disable: stop the running instance (enabling is left to the next Start/EnsureStarted; this function doesn't start anything)
	if !enabled && m.stopper != nil {
		if err := m.stopper.StopComponent(ctx, pkgID, compID); err != nil {
			// A failure to stop the instance doesn't roll back the persisted intent to disable: the persisted disabled flag is authoritative, and the instance
			// will be cleared at the next re-check/restart; it's only audited
			m.aud.Record(ctx, audit.Event{Action: "pkgregistry.SetComponentEnabled.stop", Subject: pkgID, Denied: true, Err: err})
		}
	}

	m.aud.Record(ctx, audit.Event{
		Action: "pkgregistry.SetComponentEnabled", Subject: pkgID,
		Detail: fmt.Sprintf("%s enabled=%v", compID, enabled),
	})
	return nil
}

// Uninstall removes a Package completely: code, data, bookkeeping and projections are all cleared, and the UID isn't reused
// (guaranteed by allocateUID's monotonic high-water mark). Packages from the system image can't be uninstalled dynamically
func (m *Module) Uninstall(ctx context.Context, pkgID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	e, installed := m.registry.Lookup(pkgID)
	st, hasState := m.readState(pkgID)
	if !installed && (!hasState || !st.RemovalPending) {
		return fmt.Errorf("%w: %q", ErrPackageNotInstalled, pkgID)
	}
	if (installed && e.Source == SourceSystemImage) ||
		(hasState && st.Source == SourceSystemImage.String()) {
		return fmt.Errorf("%w: %q", ErrSystemPackageImmutable, pkgID)
	}
	if installed && !hasState {
		return fmt.Errorf("pkgregistry: installed package %q has no ledger", pkgID)
	}

	previous := m.registry.List()
	previousCatalog := m.definitions.Current()
	prepared, err := m.prepareEntries(ctx, removeEntryFrom(previous, pkgID))
	if err != nil {
		return err
	}

	if !st.RemovalPending {
		st.RemovalPending = true
		st.RemovalComponents = st.RemovalComponents[:0]
		for _, component := range e.Manifest.Components {
			st.RemovalComponents = append(st.RemovalComponents, component.ID)
		}
		if err := saveRegistryState(m.stateDir, st); err != nil {
			return err
		}
	}

	// Removal is the inverse of installation: first remove provider authority
	// from the catalog. From this point no new route can bind to this package,
	// even if stopping a process or deleting its files later fails.
	if !m.definitions.Publish(prepared.candidate) {
		return ErrCatalogPublishConflict
	}
	m.revokeChangedResources(previousCatalog, prepared.candidate.Snapshot())

	if m.transferRevoker != nil {
		m.transferRevoker.RevokePackage(pkgID)
	}

	// Remove all discoverable package authority before waiting on processes or
	// touching files. The persisted tombstone is now the retry anchor; retaining
	// a live Registry entry would let ordinary lifecycle APIs re-enable it.
	var projectionErrs []error
	if err := m.registry.Replace(prepared.entries); err != nil {
		projectionErrs = append(projectionErrs, fmt.Errorf("remove registry projection: %w", err))
	}
	if err := m.idReg.Replace(projectIdentity(prepared.entries)); err != nil {
		projectionErrs = append(projectionErrs, fmt.Errorf("remove identity projection: %w", err))
	}
	if err := m.perm.Replace(projectGrants(prepared.entries)); err != nil {
		projectionErrs = append(projectionErrs, fmt.Errorf("remove permission projection: %w", err))
	}

	// Stop all package components after route/data-plane authority is gone.
	if m.stopper != nil {
		for _, componentID := range st.RemovalComponents {
			if err := m.stopper.StopComponent(ctx, pkgID, componentID); err != nil {
				m.aud.Record(ctx, audit.Event{Action: "pkgregistry.Uninstall.stop", Subject: pkgID, Denied: true, Err: err})
				projectionErrs = append(projectionErrs,
					fmt.Errorf("stop component %q: %w", componentID, err))
			}
		}
	}

	// Revoke control authority before touching package files.
	if m.revoker != nil {
		if err := m.revoker.RevokeByPackage(pkgID); err != nil {
			m.aud.Record(ctx, audit.Event{Action: "pkgregistry.Uninstall.revoke", Subject: pkgID, Denied: true, Err: err})
			projectionErrs = append(projectionErrs, fmt.Errorf("revoke control leases: %w", err))
		}
	}
	if err := errors.Join(projectionErrs...); err != nil {
		return err
	}

	if cerr := m.perm.ClearPackage(pkgID); cerr != nil {
		m.aud.Record(ctx, audit.Event{Action: "pkgregistry.Uninstall.clearGrants", Subject: pkgID, Denied: true, Err: cerr})
		return cerr
	}

	// Delete code and private data through authority.
	subj := authority.Subject{PackageID: pkgID, UID: st.UID}

	// Access rights to the user documents area live in the ACL, not in _grants.json, so ClearPackage can't clear
	// them. If that record stayed, the next package reusing the UID would get write access for free.
	//
	// This uses st.UID in hand rather than looking up the Registry: this function holds the transaction lock, and the ACL
	// recognises UIDs anyway
	if aerr := m.auth.SetUserDataAccess(ctx, subj,
		authority.SetUserDataAccessRequest{UID: st.UID, Allowed: false}); aerr != nil {
		m.aud.Record(ctx, audit.Event{
			Action: "pkgregistry.Uninstall.revokeUserData", Subject: pkgID, Denied: true, Err: aerr})
		return aerr
	}
	codeDir := filepath.Join(m.packageRoot, pkgID)
	if err := m.auth.RemovePackageTree(ctx, subj, authority.RemovePackageTreeRequest{Root: m.packageRoot, Path: codeDir}); err != nil {
		m.aud.Record(ctx, audit.Event{Action: "pkgregistry.Uninstall.rmCode", Subject: pkgID, Denied: true, Err: err})
		return err
	}
	dataDir := filepath.Join(m.dataRoot, pkgID)
	if err := m.auth.RemovePackageTree(ctx, subj, authority.RemovePackageTreeRequest{Root: m.dataRoot, Path: dataDir}); err != nil {
		m.aud.Record(ctx, audit.Event{Action: "pkgregistry.Uninstall.rmData", Subject: pkgID, Denied: true, Err: err})
		return err
	}

	// Remove the ledger only after both trees were removed.
	if sp, err := stateFilePath(m.stateDir, pkgID); err == nil {
		if rerr := os.Remove(sp); rerr != nil && !os.IsNotExist(rerr) {
			m.aud.Record(ctx, audit.Event{Action: "pkgregistry.Uninstall.rmLedger", Subject: pkgID, Denied: true, Err: rerr})
			return rerr
		}
	}

	m.aud.Record(ctx, audit.Event{Action: "pkgregistry.Uninstall", Subject: pkgID})
	return nil
}

// ---- Internal helpers -------------------------------------------------------------

// setMembership returns a new slice with id added to (present=true) / removed from (present=false) set (de-duplicated, stable)
func setMembership(set []string, id string, present bool) []string {
	out := make([]string, 0, len(set)+1)
	seen := false
	for _, x := range set {
		if x == id {
			seen = true
			if present {
				out = append(out, x) // keep
			}
			continue
		}
		out = append(out, x)
	}
	if present && !seen {
		out = append(out, id)
	}
	return out
}

// readState reads a package's persisted bookkeeping (the caller holds mu)
func (m *Module) readState(pkgID string) (registryState, bool) {
	sp, err := stateFilePath(m.stateDir, pkgID)
	if err != nil {
		return registryState{}, false
	}
	st, err := readRegistryState(sp)
	if err != nil {
		return registryState{}, false
	}
	return st, true
}

// replaceEntry overwrites the item with the same ID in the Registry with the modified e, and re-projects the three states (holding mu)
func (m *Module) replaceEntry(e Entry) error {
	entries := m.registry.List()
	for i := range entries {
		if entries[i].Manifest.PackageID == e.Manifest.PackageID {
			entries[i] = e
		}
	}
	return m.commitEntries(entries)
}

// removeEntry removes an ID from the Registry and re-projects the three states (holding mu)
func (m *Module) removeEntry(pkgID string) error {
	existing := m.registry.List()
	entries := make([]Entry, 0, len(existing))
	for _, cur := range existing {
		if cur.Manifest.PackageID == pkgID {
			continue
		}
		entries = append(entries, cur)
	}
	return m.commitEntries(entries)
}

// commitEntries atomically replaces a whole set of Entries into the Registry and re-projects identity/permission
func (m *Module) commitEntries(entries []Entry) error {
	if err := m.registry.Replace(entries); err != nil {
		return err
	}
	if err := m.idReg.Replace(projectIdentity(entries)); err != nil {
		return err
	}
	return m.perm.Replace(projectGrants(entries))
}
