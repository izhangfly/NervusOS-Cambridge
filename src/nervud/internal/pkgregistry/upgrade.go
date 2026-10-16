// This file is the upgrade decision.
//
// In order of importance:
//  1. Signing continuity (lineage succession): protection against identity hijack and data theft. UIDs and private data directories are both
//     allocated by package_id, so without a continuity check anyone could build a package with the same package_id, sign it with their own
//     key, install it, and inherit the original package's UID, data and granted permissions. Not relaxed even by devmode.
//  2. Downgrade protection (monotonic version_code): stops old versions with known vulnerabilities being slipped back in. devmode may relax it.
//
// Fresh installs (including reinstalls after uninstall) don't come here: the data is gone and the UID is new, so there's nothing to
// inherit and no question of hijack or downgrade; any version_code / any signer is accepted.
package pkgregistry

import (
	"errors"
	"fmt"
)

var (
	// ErrDowngradeNotAllowed: the upgrade package's version_code is lower than the installed version (devmode may relax this)
	ErrDowngradeNotAllowed = errors.New("pkgregistry: downgrade not allowed")

	// ErrSignerMismatch: the upgrade package's signing lineage isn't a successor of the installed version's lineage. This is a sign of identity
	// hijack and is never relaxed (not even by devmode)
	ErrSignerMismatch = errors.New("pkgregistry: upgrade signer is not a lineage successor of the installed package")

	// ErrOEMCountersignRequired: the device policy requires an OEM countersignature, but the package lacks a valid OEM signature
	ErrOEMCountersignRequired = errors.New("pkgregistry: device policy requires an OEM countersignature")
)

// checkUpgrade is the upgrade decision run when a package with the same package_id is already installed.
//
// prev is the installed version's persisted bookkeeping; signers is the new package's multi-role verification result; dev is the current devmode
func checkUpgrade(prev registryState, m Manifest, signers SignerSet, dev DevMode) error {
	// ---- 1. Downgrade protection (the secondary goal; devmode may relax it) ----
	switch {
	case m.VersionCode > prev.VersionCode:
		// A normal upgrade
	case m.VersionCode == prev.VersionCode:
		// A same-version reinstall (repairing a damaged install), allowed
	default:
		if !dev.AllowDowngrade {
			return fmt.Errorf("%w: %d < installed %d", ErrDowngradeNotAllowed, m.VersionCode, prev.VersionCode)
		}
	}

	// ---- 2. Signing continuity (the primary goal: identity hijack protection; never relaxed) ----
	if len(prev.LineageKeyIDs) == 0 {
		// The previous version was an unverified install (let through by devmode, no identity anchor): there's no continuity to enforce.
		// Allowed: there was no identity guarantee to hijack in the first place
		return nil
	}
	if signers.Dev == nil {
		// The previous version has an identity anchor, yet a package without a developer signature is trying to replace it and inherit its
		// UID and data directory: exactly the hijack this guards against
		return fmt.Errorf("%w: installed package is signed but replacement carries no developer identity", ErrSignerMismatch)
	}
	if signers.Dev.RootKeyID != prev.LineageRootKeyID {
		return fmt.Errorf("%w: root %q != installed root %q",
			ErrSignerMismatch, signers.Dev.RootKeyID, prev.LineageRootKeyID)
	}
	// A successor, not a fork: the new lineage must be at least as long as the old one, with the first len(prev) nodes equal one by one
	if signers.Dev.LineageLen < len(prev.LineageKeyIDs) {
		return fmt.Errorf("%w: lineage shrank from %d to %d",
			ErrSignerMismatch, len(prev.LineageKeyIDs), signers.Dev.LineageLen)
	}
	for i, id := range prev.LineageKeyIDs {
		if signers.Dev.KeyIDs[i] != id {
			return fmt.Errorf("%w: lineage diverges at node %d", ErrSignerMismatch, i)
		}
	}
	return nil
}
