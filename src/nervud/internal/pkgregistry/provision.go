// This file fills in a Package's run prerequisites: the system user and the private data directory.
//
// # Why a separate file
//
// These two used to be done only on the dynamic install path (install.go), while system image packages go through
// scanSystemImage, which allocates UIDs and registers Entries but never creates users or data directories.
// The result was that system packages could never start on a clean machine:
//
//	no passwd entry      -> systemd fails at step USER, 217/USER
//	no data directory    -> systemd fails at step NAMESPACE, 226/NAMESPACE
//	                        (WorkingDirectory and ReadWritePaths both point at it)
//
// Both were actually hit, not theoretical.
//
// It's pulled out so "what must exist on the system for a package to run" has one clear
// place, rather than being half done in each of two branches of the install flow.
package pkgregistry

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"

	"github.com/nervus-os/nervud/internal/audit"
	"github.com/nervus-os/nervud/internal/authority"
)

// PermissionStorageShared is the bar for the shared area between services. It must have the same name as the entry in the central catalog
// bootstrap: that's the definition, and this decides whether to create directories for the package.
const PermissionStorageShared = "perm.storage.shared"

// provisionEntry makes sure the system user and private data directory for e exist. Idempotent.
//
// One package failing doesn't block the others: one package's user failing to be created shouldn't stop the whole machine starting. Failures
// are audited and an error returned for the caller to log as it sees fit; that package's components will then fail on the systemd side,
// and service's supervision chain will handle it by criticality, which is the path made for "components won't start"
// in the first place.
func (m *Module) provisionEntry(ctx context.Context, e Entry) error {
	subj := authority.Subject{PackageID: e.Manifest.PackageID, UID: e.UID}

	// 1. The system user. GID always equals UID (see GID: e.UID in service.buildStartReq).
	if err := m.auth.EnsureAppUser(ctx, subj, authority.EnsureAppUserRequest{
		UID:  e.UID,
		GID:  e.UID,
		Name: authority.AppUserName(e.UID),
	}); err != nil {
		return fmt.Errorf("ensure app user for %s: %w", e.Manifest.PackageID, err)
	}

	// 2. The private data directory. Per package, not per version: upgrades don't touch it.
	//
	// When it already exists CreatePrivateDataDirectory fails with EEXIST, which is normal here
	// rather than an error: this function runs for every package at every boot scan.
	dataDir := filepath.Join(m.dataRoot, e.Manifest.PackageID)
	_, err := m.auth.CreatePrivateDataDirectory(ctx, subj, authority.CreateDataDirRequest{
		Path: dataDir, UID: e.UID, GID: e.UID, Perm: 0o700,
	})
	if err != nil && !errors.Is(err, authority.ErrAlreadyExists) {
		return fmt.Errorf("ensure data dir for %s: %w", e.Manifest.PackageID, err)
	}

	// 3. The two subdirectories in the shared area between services. Only created for packages that requested perm.storage.shared.
	//
	// Why the kernel creates them rather than services doing their own mkdir: letting services create them would require the root to be writable, and then any
	// package could grab someone else's directory name first. A malicious package could create nervus.camerad/ first and take it over,
	// and camerad would find it can't write to its own path when it starts. The sticky bit stops "deleting other people's",
	// not "grabbing the name first". With the root writable only by nervud, this attack doesn't exist.
	//
	// Why on demand rather than for everyone: most services don't use the shared area. Creating it for every package would
	// waste a batch of inodes on tmpfs and make the directory listing disagree with "who's actually using it".
	//
	// 0755 rather than 0700: that's exactly the "anyone can read, only the owner can write" semantics, the whole point
	// of the shared area. Not 0777: with write access open to everyone, any package could tamper with
	// configuration or models others put there.
	//
	// Recreated unconditionally (rather than only at first install like the data directory): SharedRuntimeRoot is on
	// tmpfs and empty after every reboot, so without filling it in at the boot scan a service's first write would get ENOENT.
	for _, shared := range m.sharedDirsFor(e) {
		_, err := m.auth.CreatePrivateDataDirectory(ctx, subj, authority.CreateDataDirRequest{
			Path: shared, UID: e.UID, GID: e.UID, Perm: 0o755,
		})
		if err != nil && !errors.Is(err, authority.ErrAlreadyExists) {
			return fmt.Errorf("ensure shared dir %s for %s: %w", shared, e.Manifest.PackageID, err)
		}
	}
	return nil
}

// sharedDirsFor gives the subdirectories that need creating in the shared area for this package.
//
// Three cases return empty, none of them errors:
//   - the shared area isn't enabled (empty roots): minimal wiring and many tests
//   - the package didn't request perm.storage.shared: most services don't use the shared area
//   - requested but not granted: the permission decision didn't approve it
//
// The criterion is GrantedPermissions rather than manifest.Permissions: the former is the kernel's conclusion,
// the latter only a request. Creating directories by request would let any package take a slot by writing one manifest line.
func (m *Module) sharedDirsFor(e Entry) []string {
	if !slices.Contains(e.GrantedPermissions, PermissionStorageShared) {
		return nil
	}
	var out []string
	if m.sharedRuntimeRoot != "" {
		out = append(out, filepath.Join(m.sharedRuntimeRoot, e.Manifest.PackageID))
	}
	if m.sharedPersistRoot != "" {
		out = append(out, filepath.Join(m.sharedPersistRoot, e.Manifest.PackageID))
	}
	return out
}

// provisionAll fills in run prerequisites for every installed Package and returns how many succeeded.
func (m *Module) provisionAll(ctx context.Context, entries []Entry) int {
	ok := 0
	for _, e := range entries {
		if err := m.provisionEntry(ctx, e); err != nil {
			m.aud.Record(ctx, audit.Event{
				Action: "pkgregistry.Provision", Subject: e.Manifest.PackageID,
				Denied: true, Err: err,
			})
			if m.log != nil {
				// WARN rather than ERROR: the kernel starts as normal; only this package's components won't start.
				// ERROR would make ops think the kernel itself has a problem.
				m.log.Warn("pkgregistry: failed to provision package runtime prerequisites",
					"package_id", e.Manifest.PackageID, "err", err)
			}
			continue
		}
		ok++
	}
	return ok
}
