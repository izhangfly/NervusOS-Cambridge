package pkgregistry

import (
	"context"
	"errors"
	"fmt"

	"github.com/nervus-os/nervud/internal/audit"
	"github.com/nervus-os/nervud/internal/catalog"
)

// prepareEntriesQuarantining is prepareEntries for the boot scan only: when building the Catalog
// fails, quarantine the offending source and rebuild, until it's clean or there's nothing left to quarantine.
//
// # Why install and boot need two different failure semantics
//
// Install (install.go) goes through prepareEntries and stays all or nothing: if a new package stops the Catalog from being
// built, the right reaction is to refuse that package and not touch the existing system at all.
//
// The boot scan can't work like that. Module.Start's comment promises "one bad package shouldn't bring down the whole kernel start
// sequence", but until now that promise only held at the Scan layer: manifest parsing, digest and signature
// failures are skipped package by package. Catalog-level conflicts (a resource with several managers, inconsistent interface contracts, namespace
// overreach) were all or nothing, so one misconfigured vendor package could stop the whole machine booting.
//
// With several Providers that isn't a theoretical risk: two vendors' camera services declaring the same
// (resource_type, stable_role) make Build fail with "multiple managers".
// A robot shouldn't fail to boot because one camera package is misconfigured.
//
// # The quarantine unit is a source (the whole package), not a single definition
//
// A package's Provider contract is a whole: interfaces, resources and permissions refer to each other. Dropping one
// and keeping the rest would give an "installed but half-broken" package that starts, registers some endpoints,
// and then fails mysteriously on some method. That state is much harder to diagnose than "this package isn't installed".
//
// # The kernel bootstrap is never quarantined
//
// The bootstrap sources are added by catalog.Registry.Prepare itself and aren't in entries.
// So if the culprit is nervus.kernel, no matching entry is found below and the function returns an error directly:
// a problem in the kernel's own definitions is a wiring-level fault, and must fail startup rather than run degraded.
func (m *Module) prepareEntriesQuarantining(
	ctx context.Context, entries []Entry,
) (preparedEntries, []Entry, error) {
	remaining := entries
	var quarantined []Entry

	// At least one package is removed per round, so there are at most len(entries) rounds. The bound is explicit rather than for{}:
	// if a future bug ever "still reports the same package after removing it", this exits with an error instead of looping forever
	for round := 0; round <= len(entries); round++ {
		prepared, err := m.prepareEntries(ctx, remaining)
		if err == nil {
			return prepared, quarantined, nil
		}

		var srcErr *catalog.SourceError
		if !errors.As(err, &srcErr) {
			// Not a problem with a specific source (the Catalog is unavailable, revisions exhausted and so on).
			// Quarantining any package wouldn't help
			return preparedEntries{}, quarantined, err
		}

		next, victim, found := dropEntry(remaining, srcErr.PackageID)
		if !found {
			// The culprit isn't in the set we can remove: the kernel bootstrap itself has a problem.
			// That's a wiring-level fault, and startup must fail
			return preparedEntries{}, quarantined, fmt.Errorf(
				"pkgregistry: catalog rejected non-package source %q: %w", srcErr.PackageID, err)
		}

		m.aud.Record(ctx, audit.Event{
			Action:  "pkgregistry.QuarantinePackage",
			Subject: srcErr.PackageID,
			Denied:  true,
			Err:     srcErr.Err,
			Detail:  "package excluded from catalog at boot; the rest of the system continues",
		})
		if m.log != nil {
			m.log.Error("pkgregistry: package quarantined at boot",
				"package_id", srcErr.PackageID, "err", srcErr.Err)
		}

		quarantined = append(quarantined, victim)
		remaining = next
	}

	return preparedEntries{}, quarantined, errors.New(
		"pkgregistry: catalog still rejected after quarantining every package")
}

// dropEntry returns the slice with packageID removed, the removed Entry, and whether it was found.
// It doesn't modify the input slice: the caller still holds the original set for audit.
func dropEntry(entries []Entry, packageID string) ([]Entry, Entry, bool) {
	for i, e := range entries {
		if e.Manifest.PackageID != packageID {
			continue
		}
		next := make([]Entry, 0, len(entries)-1)
		next = append(next, entries[:i]...)
		next = append(next, entries[i+1:]...)
		return next, e, true
	}
	return entries, Entry{}, false
}
