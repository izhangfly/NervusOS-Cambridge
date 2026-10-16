// This file plugs pkgregistry into the kernel.Module lifecycle and holds every dependency Install (see
// install.go) needs for orchestration. Both share the same Module type rather than splitting into
// two types that each hold a *Registry: the Registry is authoritative state and there can only be one
package pkgregistry

import (
	"context"
	"log/slog"
	"sync"

	"github.com/nervus-os/nervud/internal/audit"
	"github.com/nervus-os/nervud/internal/catalog"
	"github.com/nervus-os/nervud/internal/identity"
	"github.com/nervus-os/nervud/internal/permission"
)

// Module plugs pkgregistry into the kernel.Module lifecycle and orchestrates install transactions.
//
// Start only does one boot scan (see scan.go): it builds the initial Registry snapshot and pushes the thin projection
// to identity.Registry. There's no background loop to watch at this stage; the install transaction loop that really
// talks to pkgmanagerd was left for the next step, so kernel.FatalReporter isn't implemented;
// whether it's needed can be assessed once that loop lands
type Module struct {
	auth        PackageInstaller
	idReg       IdentityUpdater
	perm        PermissionArbiter
	registry    *Registry
	definitions *catalog.Registry
	trust       TrustStore
	aud         audit.Recorder
	log         *slog.Logger

	stateDir          string // /var/lib/nervus/registry
	systemPackagesDir string // /usr/lib/nervus/system-packages
	packageRoot       string // authority.DefaultInvariants.PackageRoot
	dataRoot          string // authority.DefaultInvariants.DataRoot
	// The two roots of the shared area between services. Empty means not enabled (tests and minimal wiring).
	// Under each, every package has a subdirectory owned by itself with mode 0755; see provisionEntry.
	sharedRuntimeRoot string // authority.DefaultInvariants.SharedRuntimeRoot (tmpfs)
	sharedPersistRoot string // authority.DefaultInvariants.SharedPersistRoot (disk)

	// mu serialises every state change (Install / Uninstall / SetComponentEnabled). Without it, concurrent installs would fight over the UID allocator (the read-modify-write of _allocator.json),
	// and commit's List -> Replace would lose updates, splitting the registry/identity/permission
	// projections. Install/uninstall isn't a hot path, so one big lock is enough
	mu sync.Mutex

	// stopper/revoker are the external collaborators for uninstall/disable (service stops components, control revokes leases),
	// injected through SetLifecycleHooks; may be nil (seams left for stages where they aren't wired). Reads and writes are protected by mu
	stopper         ComponentStopper
	revoker         LeaseRevoker
	transferRevoker PackageTransferRevoker
}

// New constructs pkgregistry's Module.
//
// auth is injected as a narrow interface (main.go's wiring pattern: the consumer defines the interface, and *authority.Gate
// satisfies it implicitly), rather than passing the whole *authority.Gate down. The caller builds registry explicitly with
// NewRegistry during wiring and passes it in, rather than this function creating it internally: if later something needs to hold the same *Registry
// on its own before Register (for example giving a diagnostics endpoint read-only access),
// the caller and the Module must share the same state, not each hold a copy the other doesn't know about.
// trust is the root-of-trust view for signature verification. main.go loads it at wiring with
// LoadTrustStore; when loading fails (a development build without the embedded root, or a bundle that fails verification) a
// zero TrustStore is passed in. Then developer self-signatures can still be verified (the public key is embedded), but any platform/oem
// role fails closed because it can't be found, and dynamic installs only ever get Ordinary, consistent with "fail closed when verification fails"
// fail-closed
func New(
	auth PackageInstaller, idReg IdentityUpdater, perm PermissionArbiter, registry *Registry,
	definitions *catalog.Registry,
	trust TrustStore, aud audit.Recorder, log *slog.Logger,
	stateDir, systemPackagesDir, packageRoot, dataRoot string,
) *Module {
	return &Module{
		auth: auth, idReg: idReg, perm: perm, registry: registry, definitions: definitions,
		trust: trust, aud: aud, log: log,
		stateDir: stateDir, systemPackagesDir: systemPackagesDir,
		packageRoot: packageRoot, dataRoot: dataRoot,
	}
}

// SetSharedRoots enables the shared area between services. Set once at wiring, read-only after Start.
//
// Separate from New rather than two extra parameters: New already has 12 parameters, and the shared area is optional infrastructure;
// minimal wiring and many tests don't need it, and forcing them to pass two empty strings would only make call sites harder to read.
func (m *Module) SetSharedRoots(runtimeRoot, persistRoot string) {
	m.sharedRuntimeRoot = runtimeRoot
	m.sharedPersistRoot = persistRoot
}

func (m *Module) Name() string { return "pkgregistry" }

// Start runs one boot scan, loads the result atomically into the Registry, and projects it to identity.
//
// A single Package failing to scan is only audited and skipped, without making Start itself return an error,
// because one bad package shouldn't bring down the whole kernel start sequence. Start only returns an error when the Registry/
// identity Replace itself fails, which means the scan results contradict themselves
// (such as duplicate Package IDs), a wiring-level problem
func (m *Module) Start(ctx context.Context) error {
	previous := m.registry.List()
	result := Scan(m.stateDir, m.systemPackagesDir, m.packageRoot, m.trust, m.log)

	for _, s := range result.Skipped {
		m.aud.Record(ctx, audit.Event{
			Action: "pkgregistry.Scan", Subject: s.Path, Denied: true, Err: s.Err,
		})
		if m.log != nil {
			m.log.Warn("pkgregistry: skipped package during scan", "path", s.Path, "err", s.Err)
		}
	}

	// Quarantine rather than fail as a whole. Scan's per-package skipping only covers the manifest/digest/signature layer;
	// if Catalog-level conflicts (a resource with several managers, inconsistent interface contracts, namespace overreach) also made
	// Start return an error, one misconfigured vendor package could stop the whole machine booting, directly contradicting this function's comment's
	// promise that "one bad package shouldn't bring down kernel startup".
	//
	// The install path (install.go) doesn't come this way and stays all or nothing: if a new package has a problem, the new package is refused.
	prepared, quarantined, err := m.prepareEntriesQuarantining(ctx, result.Entries)
	if err != nil {
		return err
	}
	if len(quarantined) != 0 && m.log != nil {
		ids := make([]string, 0, len(quarantined))
		for _, e := range quarantined {
			ids = append(ids, e.Manifest.PackageID)
		}
		m.log.Error("pkgregistry: boot continued without quarantined packages",
			"count", len(ids), "packages", ids)
	}

	// Fill in run prerequisites: the system user + the private data directory.
	//
	// It must be here, not only in install.go. System image packages go through scanSystemImage,
	// and that path allocates UIDs and registers Entries but never creates users or data directories, so system packages
	// could never start on a clean machine (217/USER and 226/NAMESPACE).
	//
	// Before Replace: once the projection is pushed out, the service module may use it to start processes at any moment,
	// so the prerequisites must already be in place.
	if n := m.provisionAll(ctx, prepared.entries); m.log != nil {
		m.log.Info("pkgregistry: provisioned package runtime prerequisites",
			"ok", n, "total", len(prepared.entries))
	}
	return m.publishCatalogLast(ctx, previous, prepared)
}

// Stop: purely in-memory state, nothing to release; the bookkeeping files were written atomically at install/uninstall
func (m *Module) Stop(_ context.Context) error {
	return nil
}

// projectIdentity projects the Registry's full state into the thin view identity.Registry needs:
// only ID/UID/Trust; version, manifest and other fields stay in pkgregistry's
// authoritative state, so the two Registries don't each keep copies that can drift
func projectIdentity(entries []Entry) []identity.Package {
	out := make([]identity.Package, 0, len(entries))
	for _, e := range entries {
		out = append(out, identity.Package{
			ID: e.Manifest.PackageID, UID: e.UID, Trust: e.Trust,
			Generation: e.RuntimeGeneration,
		})
	}
	return out
}

// projectGrants projects the Registry's full state into the thin view permission.Registry needs:
// only the ID and the set of granted permissions. The same principle as projectIdentity; this only projects
// the results prepareEntries has already recomputed against the same candidate Catalog.
func projectGrants(entries []Entry) []permission.Grant {
	out := make([]permission.Grant, 0, len(entries))
	for _, e := range entries {
		out = append(out, permission.Grant{
			PackageID:     e.Manifest.PackageID,
			Permissions:   e.GrantedPermissions,
			ConsentExempt: isSystemSoftware(e),
		})
	}
	return out
}

// isSystemSoftware decides whether a package ships with the read-only system image and is platform trusted.
//
// These packages' USER_CONSENT permissions need no runtime consent (see permission.Grant.ConsentExempt).
//
// Both conditions are needed: looking only at Source would let a low-trust package slipped into the image directory get through; looking only at Trust
// would let a dynamically installed platform-signed package skip the question too, while the reason "the user accepted it
// when setting up the machine" only holds for the batch shipped with the image
func isSystemSoftware(e Entry) bool {
	return e.Source == SourceSystemImage && e.Trust == identity.TrustPlatform
}
