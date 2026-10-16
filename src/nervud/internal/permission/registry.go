// This file is permission's authoritative runtime state: Package ID -> set of granted permissions,
// queried by ipc's Request dispatch pipeline when it decides.
package permission

import (
	"fmt"
	"sync"
	"sync/atomic"

	ipcv1 "github.com/nervus-os/nervus-ipc/protocol/ipcv1"

	"github.com/nervus-os/nervud/internal/audit"
	"github.com/nervus-os/nervud/internal/catalog"
	"github.com/nervus-os/nervud/internal/identity"
)

// ErrDuplicatePackageID: two Grants in one Replace share the same Package ID
var ErrDuplicatePackageID = fmt.Errorf("permission: duplicate package id in grant snapshot")

// Grant is a Package's full set of granted permissions at one moment.
//
// It's the unit of projection pkgregistry pushes to permission, in the same way identity.Package projects
// the fields identity needs: it carries only the two fields permission uses.
type Grant struct {
	PackageID   string
	Permissions []string

	// ConsentExempt means this package's USER_CONSENT permissions need no runtime consent.
	//
	// The criterion is "system software": shipped on the read-only system image + Platform trust + a platform role signature.
	// pkgregistry computes it during projection (see projectGrants); this package doesn't judge it, because a package's
	// origin and trust are pkgregistry's facts.
	//
	// # Why system software skips runtime consent
	//
	// Runtime consent asks "do you trust this third-party app?". System software ships in the same batch as the kernel, on the same
	// signing chain, and the user accepted it when setting up the machine. Asking again adds no security: the user's
	// only real choice is "agree", or the file manager can't open files and Settings can't install packages.
	//
	// The apps that really need this gate are ordinary ones: permissions like the camera, user files and motion control
	// must be asked for each time. That path goes through GrantState and has nothing to do with this field.
	//
	// Note it can NOT exempt the install-time decision: system software still only gets the permission set IntersectAt
	// approved. This field only skips the question "does the user agree right now?".
	ConsentExempt bool
}

// Registry is permission's authoritative runtime state.
//
// It copies the copy-on-write + atomic pointer + full-replacement pattern of identity.Registry / pkgregistry.Registry:
// many reads, few writes (every Request on every connection checks it; it's only written on install / uninstall /
// revocation), and full replacement avoids the not-found window that "delete then add" opens in between.
//
// The Registry also holds an immutable Catalog: the Intersect method closes it into the decision,
// so a single *Registry instance satisfies both pkgregistry.PermissionArbiter (install-time
// decisions) and run-time queries (Allowed), with no need to split it into two types that each own half the state.
//
// The zero value isn't usable; it must be built with NewRegistry.
type Registry struct {
	definitions *catalog.Registry
	snap        atomic.Pointer[snapshot]
	// stateMu serializes install-set publication with runtime grant writes. A
	// SetRuntimeState that loses the race with uninstall must observe the removed
	// package and fail instead of recreating persisted authority after cleanup.
	stateMu sync.Mutex
	// grants is the runtime grant state of GrantUser (dangerous) permissions. The install set (snap) answers
	// what was granted at install time, grants answers what the user confirmed/revoked at run time, and Allowed looks at both.
	grants *grantStore

	revokerMu sync.RWMutex
	revoker   PermissionRevoker

	installRevokerMu sync.RWMutex
	installRevoker   InstallGrantRevoker

	projectorMu sync.RWMutex
	projector   RuntimePermissionProjector
}

type snapshot struct {
	byPackage map[string]map[string]struct{}
	// consentExempt is the "system software" set; see Grant.ConsentExempt. It's swapped in atomically in the same Replace
	// as byPackage. Updating them separately would open a window in which a package already
	// has its new permissions but still carries the old exemption decision.
	consentExempt map[string]struct{}
}

// NewRegistry binds runtime grants to the one central definition catalog.
func NewRegistry(definitions *catalog.Registry) *Registry {
	r := &Registry{definitions: definitions, grants: newGrantStore()}
	r.snap.Store(&snapshot{
		byPackage:     map[string]map[string]struct{}{},
		consentExempt: map[string]struct{}{},
	})
	return r
}

// NewDefaultRegistry is primarily useful in tests and standalone tools. The
// production assembly constructs one catalog.Registry and shares it explicitly.
func NewDefaultRegistry() *Registry {
	definitions, err := catalog.NewDefaultRegistry()
	if err != nil {
		panic(fmt.Sprintf("permission: invalid default catalog: %v", err))
	}
	return NewRegistry(definitions)
}

// SetGrantStore wires up the persistence directory for runtime grant state and the revocation hook.
// Called by main.go during wiring; stateDir is /var/lib/nervus/registry and revoker is implemented by control
//
// (revoking a motion-group permission bumps the motion epoch). After the call, existing state is loaded from disk.
func (r *Registry) SetGrantStore(stateDir string, revoker LeaseRevoker, aud audit.Recorder) {
	r.grants.mu.Lock()
	r.grants.stateDir = stateDir
	r.grants.revoker = revoker
	r.grants.aud = aud
	r.grants.mu.Unlock()
	r.grants.load()
}

// PermissionRevoker invalidates data-plane state authorized by one permission.
// IPC owns this hook so it can close route tokens before scanning transfers.
type PermissionRevoker interface {
	RevokePermission(packageID, permission string)
}

// InstallGrantRevoker removes authority already projected into a running
// process when Replace drops an install-time grant. Implementations must not
// perform package lookups, systemd I/O, or waits: Replace may be called while
// pkgregistry holds its transaction mutex.
type InstallGrantRevoker interface {
	RevokeInstallGrant(packageID, permission string)
}

// RuntimePermissionProjector applies a USER_CONSENT decision to authority
// outside the IPC data plane, such as writable paths in an already-running
// process sandbox. It is invoked only by SetRuntimeState, never by Replace:
// package projection may execute while pkgregistry holds its transaction lock.
type RuntimePermissionProjector interface {
	ProjectRuntimePermission(packageID, permission string, allowed bool) error
}

// SetPermissionRevoker installs the runtime revocation hook.
func (r *Registry) SetPermissionRevoker(revoker PermissionRevoker) {
	if r == nil {
		return
	}
	r.revokerMu.Lock()
	r.revoker = revoker
	r.revokerMu.Unlock()
}

// SetInstallGrantRevoker installs the non-blocking process-authority hook used
// by Replace after the data-plane revoker has run.
func (r *Registry) SetInstallGrantRevoker(revoker InstallGrantRevoker) {
	if r == nil {
		return
	}
	r.installRevokerMu.Lock()
	r.installRevoker = revoker
	r.installRevokerMu.Unlock()
}

// SetRuntimePermissionProjector installs the runtime sandbox projection hook.
func (r *Registry) SetRuntimePermissionProjector(projector RuntimePermissionProjector) {
	if r == nil {
		return
	}
	r.projectorMu.Lock()
	r.projector = projector
	r.projectorMu.Unlock()
}

// Intersect decides an install request with the Registry's own Catalog (see intersect.go).
//
// It satisfies pkgregistry.PermissionArbiter: the install flow only knows "requested permissions + trust";
// the Catalog is permission's internal state and needn't (and shouldn't) be passed in by the caller.
//
// Fail-safe on an uninitialised Registry: a zero Catalog means "no registered permissions at all",
// so every request is refused instead of panicking.
func (r *Registry) IntersectAt(
	definitions *catalog.Snapshot,
	requested []string,
	source catalog.SourceKind,
	trust identity.TrustProfile,
	signers catalog.SignerEvidence,
) (granted, denied []string) {
	if r == nil || definitions == nil {
		return nil, append([]string(nil), requested...)
	}
	for _, permissionID := range requested {
		definition, ok := definitions.Permission(permissionID)
		if !ok || trust < definition.MinimumTrust {
			denied = append(denied, permissionID)
			continue
		}
		if definition.RequiredSignerRole != "" && !signers.HasRole(definition.RequiredSignerRole) {
			denied = append(denied, permissionID)
			continue
		}
		switch definition.GrantMode {
		case ipcv1.GrantMode_GRANT_MODE_NORMAL,
			ipcv1.GrantMode_GRANT_MODE_USER_CONSENT:
			// USER_CONSENT enters the install set here; AllowedAt additionally
			// requires the system-owned runtime grant state.
		case ipcv1.GrantMode_GRANT_MODE_SIGNATURE:
			if !signers.SameIdentity(definition.Owner.Signers) {
				denied = append(denied, permissionID)
				continue
			}
		case ipcv1.GrantMode_GRANT_MODE_PRIVILEGED:
			if source != catalog.SourceKindSystemImage ||
				(trust != identity.TrustOEM && trust != identity.TrustPlatform) {
				denied = append(denied, permissionID)
				continue
			}
		case ipcv1.GrantMode_GRANT_MODE_SYSTEM_ONLY:
			if source != catalog.SourceKindSystemImage || trust != identity.TrustPlatform {
				denied = append(denied, permissionID)
				continue
			}
		default:
			denied = append(denied, permissionID)
			continue
		}
		granted = append(granted, permissionID)
	}
	return granted, denied
}

// Replace atomically replaces the whole index of granted permissions.
//
// pkgregistry pushes it in full at the boot scan and after every Install commit (the same way identity receives
// pkgregistry's projection; see projectIdentity in pkgregistry/module.go).
// Full replacement rather than incremental changes, for the same reason as identity.Registry.Replace: an incremental interface
// tempts callers into "delete then add", which leaves a window where lookups fail.
//
// If validation fails the whole thing is refused and the old snapshot stays as it was: better to keep using the last known-good grant state
// than to load a self-contradictory one.
func (r *Registry) Replace(grants []Grant) error {
	next := make(map[string]map[string]struct{}, len(grants))
	exempt := make(map[string]struct{})
	for _, g := range grants {
		if g.PackageID == "" {
			return fmt.Errorf("permission: grant has empty package id")
		}
		if _, dup := next[g.PackageID]; dup {
			return fmt.Errorf("%w: %q", ErrDuplicatePackageID, g.PackageID)
		}
		perms := make(map[string]struct{}, len(g.Permissions))
		for _, p := range g.Permissions {
			perms[p] = struct{}{}
		}
		next[g.PackageID] = perms
		if g.ConsentExempt {
			exempt[g.PackageID] = struct{}{}
		}
	}
	r.stateMu.Lock()
	previous := r.snap.Swap(&snapshot{byPackage: next, consentExempt: exempt})
	r.stateMu.Unlock()
	if previous != nil {
		for packageID, oldPermissions := range previous.byPackage {
			newPermissions := next[packageID]
			for permissionID := range oldPermissions {
				if _, retained := newPermissions[permissionID]; !retained {
					// Route/Transfer authority closes before any process sandbox
					// teardown is requested.
					r.revokePermission(packageID, permissionID)
					r.revokeInstallGrant(packageID, permissionID)
				}
			}
		}
	}
	return nil
}

// Allowed reports whether packageID has been granted permission.
//
// On an uninitialised Registry (a &Registry{} not built with NewRegistry, even a typed nil)
// it is also fail-safe and returns false (refuse) instead of panicking. The fail-safe direction needs
// particular care here: identity.Lookup returning "no such caller" on uninitialised state is a safe default refusal,
// and Allowed returning false is also a default refusal. Both point the same way, so there's no risk of
// "allowing when uninitialised".
//
// The requirement is "still do a quick permission and liveness re-check on every call, to support dynamic revocation": this method
// reads the latest snapshot every time and nothing is cached by the caller. Copy-on-write + an atomic pointer naturally make
// the next Allowed after Replace see the new state immediately.
func (r *Registry) Allowed(packageID, permission string) bool {
	if r == nil || r.definitions == nil {
		return false
	}
	return r.AllowedAt(r.definitions.Current(), packageID, permission)
}

// AllowedAt performs the grant and definition checks against one caller-owned
// immutable Snapshot, so an endpoint Route cannot mix catalog revisions.
func (r *Registry) AllowedAt(
	definitions *catalog.Snapshot,
	packageID string,
	permission string,
) bool {
	if r == nil || definitions == nil {
		return false
	}
	definition, defined := definitions.Permission(permission)
	if !defined {
		return false
	}
	snap := r.snap.Load()
	if snap == nil {
		return false
	}
	perms, ok := snap.byPackage[packageID]
	if !ok {
		return false
	}
	if _, ok = perms[permission]; !ok {
		return false // not granted at install time (or uninstalled / projected out by a downgrade)
	}
	// USER_CONSENT permissions: the install set only proves eligibility; actual
	// access requires the system-owned runtime grant state.
	// == Granted (both must pass to allow)
	if definition.GrantMode == ipcv1.GrantMode_GRANT_MODE_USER_CONSENT {
		// Except system software: it ships with the kernel, on the same signing chain, and the user accepted it
		// when setting up the machine. Asking again adds no security: the user's only real choice is "agree", or
		// the file manager can't open files. See Grant.ConsentExempt for the criterion.
		if _, exempt := snap.consentExempt[packageID]; exempt {
			return true
		}
		return r.grants.state(packageID, permission) == GrantStateGranted
	}
	return true
}

// SetRuntimeState sets and persists the runtime grant state of a GrantUser permission. Only GrantUser permissions have runtime state; calling it for other Modes returns an error. Revoking
// a motion-group permission also makes control revoke leases and bump the motion epoch.
//
// The only entry point today is the admin socket (nervusctl), guarded by the socket's own peer credentials.
// There's no app-facing IPC grant interface yet: that first needs a decision on who shows the consent dialog, and that component must be
// a standalone platform-release-signed system service, not the Settings app.
func (r *Registry) SetRuntimeState(packageID, permission string, state GrantState) error {
	if r == nil {
		return fmt.Errorf("permission: nil registry")
	}
	if !state.valid() {
		return fmt.Errorf("permission: invalid grant state %d", state)
	}
	if r.definitions == nil {
		return fmt.Errorf("permission: definition catalog unavailable")
	}
	entry, ok := r.definitions.Current().Permission(permission)
	if !ok {
		return fmt.Errorf("permission: unknown permission %q", permission)
	}
	if entry.GrantMode != ipcv1.GrantMode_GRANT_MODE_USER_CONSENT {
		return fmt.Errorf("permission: %q is not a user-grantable (dangerous) permission", permission)
	}
	r.stateMu.Lock()
	snap := r.snap.Load()
	installed := false
	if snap != nil {
		if permissions := snap.byPackage[packageID]; permissions != nil {
			_, installed = permissions[permission]
		}
	}
	if !installed {
		r.stateMu.Unlock()
		return fmt.Errorf(
			"permission: package %q has no installed grant for %q",
			packageID, permission,
		)
	}
	if err := r.grants.set(
		packageID,
		permission,
		state,
		entry.RiskClass == ipcv1.RiskClass_RISK_CLASS_PHYSICAL_CONTROL,
	); err != nil {
		r.stateMu.Unlock()
		return err
	}
	r.stateMu.Unlock()
	if state != GrantStateGranted {
		// Close route tokens and transfers before rebuilding process sandboxes.
		// From this point no new IPC work can be authorized by the old grant.
		r.revokePermission(packageID, permission)
	}
	if err := r.projectRuntimePermission(packageID, permission); err != nil {
		// The persisted decision remains authoritative and is deliberately not
		// rolled back: restoring a grant because process teardown failed would
		// reopen IPC authority while the caller believes it revoked access.
		return fmt.Errorf("permission: project runtime state for %q/%q: %w", packageID, permission, err)
	}
	return nil
}

// GrantStateOf returns a permission's current runtime grant state (for the permission UI to show / diagnose)
func (r *Registry) GrantStateOf(packageID, permission string) GrantState {
	if r == nil {
		return GrantStateNotRequested
	}
	return r.grants.state(packageID, permission)
}

// ClearPackage deletes all runtime grant state of a Package, for the uninstall path. The install-time
// set (snap) is removed by pkgregistry's Replace projection; this only clears the runtime _grants.json,
// otherwise reinstalling the same ID would inherit old dangerous permission grants.
func (r *Registry) ClearPackage(packageID string) error {
	if r == nil {
		return nil
	}
	r.stateMu.Lock()
	defer r.stateMu.Unlock()
	return r.grants.clearPackage(packageID)
}

// Len returns the number of Packages that currently hold granted permission records, for diagnostics and tests
func (r *Registry) Len() int {
	if r == nil {
		return 0
	}
	snap := r.snap.Load()
	if snap == nil {
		return 0
	}
	return len(snap.byPackage)
}

func (r *Registry) revokePermission(packageID, permissionID string) {
	r.revokerMu.RLock()
	revoker := r.revoker
	r.revokerMu.RUnlock()
	if revoker != nil {
		revoker.RevokePermission(packageID, permissionID)
	}
}

func (r *Registry) revokeInstallGrant(packageID, permissionID string) {
	r.installRevokerMu.RLock()
	revoker := r.installRevoker
	r.installRevokerMu.RUnlock()
	if revoker != nil {
		revoker.RevokeInstallGrant(packageID, permissionID)
	}
}

func (r *Registry) projectRuntimePermission(packageID, permissionID string) error {
	r.projectorMu.RLock()
	projector := r.projector
	r.projectorMu.RUnlock()
	if projector == nil {
		return nil
	}
	return projector.ProjectRuntimePermission(
		packageID,
		permissionID,
		r.Allowed(packageID, permissionID),
	)
}
