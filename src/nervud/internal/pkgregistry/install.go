// This file orchestrates the final install decision and the transactional commit: it connects the separate re-check logic of manifest/digest/signature/arbitrate/
// upgrade to authority.Gate's two privileged operations, and produces one
// Entry registered in the Registry
package pkgregistry

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	ipcv1 "github.com/nervus-os/nervus-ipc/protocol/ipcv1"

	"github.com/nervus-os/nervud/internal/audit"
	"github.com/nervus-os/nervud/internal/authority"
	"github.com/nervus-os/nervud/internal/catalog"
	"github.com/nervus-os/nervud/internal/identity"
	"github.com/nervus-os/nervud/internal/permission"
)

// ErrStagingMetadataMismatch: the manifest.json/manifest.sig in staging differ from the verified bytes the caller submitted,
// a sign of "verify A, write B"; the install is refused
var ErrStagingMetadataMismatch = errors.New("pkgregistry: staging manifest/signature differs from verified bytes")

// ErrReloadPending means the package/catalog commit succeeded, but the running
// instance could not yet be switched to the new package generation. The new
// registry state is authoritative and the caller must retry the lifecycle
// reload; reporting success here would leave a committed but unavailable
// provider indistinguishable from a healthy upgrade.
var ErrReloadPending = errors.New("pkgregistry: package committed, runtime reload pending")

// verifyStagingMetadata checks that the manifest.json and manifest.sig in the staging directory are byte for byte identical to the
// already verified bytes the caller passed in. That way the manifest/sig in the tree written to disk (committed)
// are the same object that was verified, closing the gap left by digests exempting these two files, where what was verified and what was written weren't the same copy.
// Payload files are covered by VerifyDigests; this only adds the two exempted metadata files
func verifyStagingMetadata(stagingDir string, manifestBytes, sigBlock []byte) error {
	mGot, err := os.ReadFile(filepath.Join(stagingDir, ManifestFileName))
	if err != nil {
		return fmt.Errorf("%w: read staging manifest: %v", ErrStagingMetadataMismatch, err)
	}
	if !bytes.Equal(mGot, manifestBytes) {
		return fmt.Errorf("%w: manifest.json", ErrStagingMetadataMismatch)
	}
	sGot, err := os.ReadFile(filepath.Join(stagingDir, SignatureFileName))
	if err != nil {
		return fmt.Errorf("%w: read staging signature: %v", ErrStagingMetadataMismatch, err)
	}
	if !bytes.Equal(sGot, sigBlock) {
		return fmt.Errorf("%w: manifest.sig", ErrStagingMetadataMismatch)
	}
	return nil
}

// PackageInstaller is pkgregistry's narrow interface dependency on authority.Gate.
//
// main.go's wiring pattern: the consumer package defines the minimal interface it needs, and *authority.Gate satisfies it implicitly
type PackageInstaller interface {
	InstallVerifiedPackage(ctx context.Context, subj authority.Subject, req authority.InstallVerifiedPackageRequest) error
	CreatePrivateDataDirectory(ctx context.Context, subj authority.Subject, req authority.CreateDataDirRequest) (authority.DirHandle, error)
	// RemovePackageTree recursively deletes an installed Package's code or data directory (uninstall / install failure compensation)
	RemovePackageTree(ctx context.Context, subj authority.Subject, req authority.RemovePackageTreeRequest) error
	// EnsureAppUser registers an allocated App UID in the system user database. Idempotent.
	//
	// systemd's User= requires even a numeric UID to resolve through NSS, otherwise spawn fails at
	// step USER (217/USER). Without this step, no Package component could start.
	EnsureAppUser(ctx context.Context, subj authority.Subject, req authority.EnsureAppUserRequest) error
	// SetUserDataAccess adds or removes a UID's entry in the user documents area's ACL.
	//
	// The uninstall path needs it: ClearPackage only clears runtime state in _grants.json, while access
	// rights now live in the filesystem ACL. Without removing that entry, when the UID is reused by the next package it
	// gets write access to the user documents area for free, and no UI ever showed that grant
	SetUserDataAccess(ctx context.Context, subj authority.Subject, req authority.SetUserDataAccessRequest) error
}

// IdentityUpdater is pkgregistry's narrow interface dependency on identity.Registry:
// it only needs to replace that thin projection in full, not read-side methods like Lookup/Resolve
type IdentityUpdater interface {
	Replace(pkgs []identity.Package) error
}

// PermissionArbiter is pkgregistry's narrow interface dependency on permission.Registry:
// Intersect makes the install-time permission decision, and Replace pushes the full GrantedPermissions projection out.
// At run time both methods are satisfied by the same *permission.Registry instance
type PermissionArbiter interface {
	IntersectAt(
		definitions *catalog.Snapshot,
		requested []string,
		source catalog.SourceKind,
		trust identity.TrustProfile,
		signers catalog.SignerEvidence,
	) (granted, denied []string)
	Replace(grants []permission.Grant) error
	// ClearPackage deletes a Package's runtime grant state (for uninstall)
	ClearPackage(packageID string) error
	// SetRuntimeState sets the runtime grant state of one USER_CONSENT permission.
	//
	// Install-time default grants are stored with it: after install, that batch of permissions becomes Granted directly.
	// It has two built-in prechecks (it must be USER_CONSENT, and it must already be in the install-time grant set)
	SetRuntimeState(packageID, permissionID string, state permission.GrantState) error
	// GrantStateOf reads one permission's current runtime grant state.
	//
	// The upgrade path needs it: only uninstall clears _grants.json (see the ClearPackage
	// call site in lifecycle.go), upgrades don't. So old decisions are still there on reinstall, and "a permission the user switched off last time" must never
	// be quietly switched back on by a version update, which would give every app a way round the user's decision.
	GrantStateOf(packageID, permissionID string) permission.GrantState
}

// InstallTransaction is the input of one install transaction
type InstallTransaction struct {
	ManifestBytes []byte // the raw, unparsed manifest.json bytes; the signature is verified against these raw bytes
	SigBlock      []byte // the detached signature manifest.sig
	StagingDir    string // the expanded staging directory produced by pkgmanagerd
	Source        Source

	// ConsentedPermissions are the permissions the user agreed to on the install confirmation screen.
	//
	// Collected by the confirmation UI and passed in unchanged. The kernel doesn't trust this list itself and only treats it as an
	// upper bound: what's actually stored is its intersection with the install-time grant set and the USER_CONSENT tier
	//  (see applyInstallConsent). An inflated list doesn't get any package a single extra permission.
	//
	// Empty means "no permission was agreed to", and the install goes ahead as normal; those permissions are simply refused at run time,
	// and the user can still grant them in Settings later
	ConsentedPermissions []string

	// ExpectedManifestDigest is the InspectResult.ManifestDigest returned by the confirmation screen's Inspect,
	// passed back unchanged. A mismatch is refused.
	//
	// # What it closes: "looked at A, installed B"
	//
	// The .nspkg sits in the user-data area shared across packages, so the caller could swap the file between Inspect and Install.
	// Each call is legitimate on its own, and neither reveals anything when looked at alone; but the user agreed based on A's
	// permission list, while the stored ConsentedPermissions would be applied to B.
	//
	// Empty means no check. It must be allowed to be empty: system setup scripts and deploy call Install directly
	// and never had an Inspect step, and requiring it would break all of them. Deciding "which callers must
	// include it" is policy and doesn't belong in this layer; this layer only guarantees "if it's included, it's always checked".
	ExpectedManifestDigest string
}

// Install runs the stretch of the dynamic install flow from "nervud's independent re-check" through to Registry
// registration. Steps: parse the manifest -> multi-role signature checks (+ devmode relaxation) -> trust decision -> OEM countersignature
// admission -> host ABI match -> digest re-check -> upgrade decision (downgrade protection + identity hijack protection) -> permission decision ->
// allocate a stable UID -> Authority atomic commit -> registration.
//
// Any failing step returns an error for the whole thing, leaving nothing half-done
func (m *Module) Install(ctx context.Context, tx InstallTransaction) (Entry, error) {
	// Serialise all state changes. Installing isn't a hot path, and one big lock stops concurrent installs fighting over the UID
	// allocator and losing updates between List -> Replace
	m.mu.Lock()
	defer m.mu.Unlock()
	previousEntries := m.registry.List()

	// Install is the dynamic-install-only entry point and only accepts SourceDynamicInstall (a fix).
	// System image packages never come this way: scanSystemImage builds their Entry directly. If callers were allowed
	// to pass SourceSystemImage, they could bypass admission checks that only run on the dynamic branch, such as "developer signature required" / OEM countersignatures,
	// and could even get system trust for free through Arbitrate. The two entry points must not be mixed
	if tx.Source != SourceDynamicInstall {
		err := fmt.Errorf("%w: Install only accepts dynamic-install source, got %q", ErrInvalidSource, tx.Source)
		m.auditInstall(ctx, tx, false, err)
		return Entry{}, err
	}

	manifest, err := ParseManifest(tx.ManifestBytes)
	if err != nil {
		m.auditInstall(ctx, tx, false, err)
		return Entry{}, err
	}

	// A fix: the manifest/signature verified must be byte for byte identical to the ones in the tree written to disk. digest.go deliberately
	// exempts manifest.json/manifest.sig from the Extra check (they can't hash themselves), so without comparing them
	// explicitly here, an attacker could verify A and write B: tx.ManifestBytes passes verification while staging
	// holds a different manifest.json, and after commit the restart scan would read the unverified one
	if err := verifyStagingMetadata(tx.StagingDir, tx.ManifestBytes, tx.SigBlock); err != nil {
		m.auditInstall(ctx, tx, false, err)
		return Entry{}, err
	}

	// ---- Confirmation screen binding: what's installed must be the copy the user saw ----
	//
	// It comes after verifyStagingMetadata: that step already guarantees tx.ManifestBytes is byte for byte identical to the copy
	// written in staging, so hashing tx.ManifestBytes here is the same as hashing
	// the copy in staging. The other order (compare hashes first) couldn't stop "verify A, write B".
	//
	// Empty = no check; see the notes on ExpectedManifestDigest.
	if tx.ExpectedManifestDigest != "" {
		actual := hex.EncodeToString(sha256Sum(tx.ManifestBytes))
		if actual != tx.ExpectedManifestDigest {
			// Don't write the two digests into the error for the caller. This path means "the content was swapped
			// after confirmation", and the caller is exactly the party that might have swapped it; echoing the digests would tell
			// it the result of its probe. Details go into audit, and the caller gets a one-line conclusion.
			err := fmt.Errorf("%w: package content changed after user confirmation", ErrManifestDigestMismatch)
			m.aud.Record(ctx, audit.Event{
				Action:  "pkgregistry.Install.ManifestDigestMismatch",
				Subject: manifest.PackageID,
				Denied:  true,
				Detail:  fmt.Sprintf("expected=%s actual=%s", tx.ExpectedManifestDigest, actual),
			})
			m.auditInstall(ctx, tx, false, err)
			return Entry{}, err
		}
	}

	// A fix: dynamic installs may not take a system image package's package_id. Otherwise they could overwrite the system Entry,
	// inherit its identity, and create duplicate IDs at the restart scan
	if cur, ok := m.registry.Lookup(manifest.PackageID); ok && cur.Source == SourceSystemImage {
		err := fmt.Errorf("%w: %q is a system-image package", ErrSystemPackageImmutable, manifest.PackageID)
		m.auditInstall(ctx, tx, false, err)
		return Entry{}, err
	}

	dev := loadDevMode(m.stateDir)

	// ---- Multi-role signature checks ----
	signers, sigErr := m.trust.VerifySignature(tx.ManifestBytes, tx.SigBlock)
	if sigErr != nil {
		// The audit distinguishes "verified but failed" (attack/corruption) from "couldn't verify", but the decision is the same for both:
		// refused by default, allowed only when devmode explicitly relaxes it
		m.aud.Record(ctx, audit.Event{
			Action: "pkgregistry.VerifySignature", Subject: manifest.PackageID, Denied: true, Err: sigErr,
		})
		if !dev.Enabled || !dev.AllowUnverifiedSignature {
			m.auditInstall(ctx, tx, false, sigErr)
			return Entry{}, sigErr
		}
		// devmode relaxation: allow installing a package with an unverified signature, but its identity is treated as unanchored and trust stays Ordinary
		m.aud.Record(ctx, audit.Event{
			Action: "pkgregistry.Install.devmode", Subject: manifest.PackageID,
			Detail: "relaxed: allow_unverified_signature",
		})
		signers = SignerSet{Trust: identity.TrustOrdinary}
	} else if tx.Source == SourceDynamicInstall && !signers.HasDeveloper {
		// The signature checks passed but there's no developer role signature: a dynamic install lacks an identity anchor, so refuse
		m.auditInstall(ctx, tx, false, ErrNoDeveloperSignature)
		return Entry{}, ErrNoDeveloperSignature
	}

	trust := Arbitrate(tx.Source, signers)

	// ---- OEM countersignature admission ----
	// Uses HasOEMCountersign rather than HasOEM: oem-trust-software meets the countersignature bar but doesn't raise trust
	if tx.Source == SourceDynamicInstall && m.trust.policyRequireOEMCountersign() && !signers.HasOEMCountersign {
		if !dev.Enabled || !dev.SkipOEMCountersign {
			m.auditInstall(ctx, tx, false, ErrOEMCountersignRequired)
			return Entry{}, ErrOEMCountersignRequired
		}
		m.aud.Record(ctx, audit.Event{
			Action: "pkgregistry.Install.devmode", Subject: manifest.PackageID,
			Detail: "relaxed: skip_oem_countersign",
		})
	}

	// ---- Host ABI match (for both fresh installs and upgrades) ----
	if err := checkHostABI(manifest.SupportedABIs); err != nil {
		m.auditInstall(ctx, tx, false, err)
		return Entry{}, err
	}

	// ---- Digest re-check ----
	diff, err := VerifyDigests(tx.StagingDir, manifest.Digests)
	if err != nil {
		m.auditInstall(ctx, tx, false, err)
		return Entry{}, err
	}
	if !diff.Clean() {
		err := fmt.Errorf("%w: %+v", ErrDigestMismatch, diff)
		m.auditInstall(ctx, tx, false, err)
		return Entry{}, err
	}

	provider, err := loadRequiredProviderArtifacts(tx.StagingDir, manifest)
	if err != nil {
		m.auditInstall(ctx, tx, false, err)
		return Entry{}, err
	}

	// ---- Upgrade decision (prev must be read before any state is written) ----
	prev, hadPrev := m.readPrevState(manifest.PackageID)
	var carriedDisabled []string
	if hadPrev {
		if prev.RemovalPending {
			err := fmt.Errorf("%w: %q", ErrPackageRemovalPending, manifest.PackageID)
			m.auditInstall(ctx, tx, false, err)
			return Entry{}, err
		}
		if err := checkUpgrade(prev, manifest, signers, dev); err != nil {
			m.auditInstall(ctx, tx, false, err)
			return Entry{}, err
		}
		carriedDisabled = prev.DisabledComponents // disabled state is kept across upgrades
	}

	entry := Entry{
		Manifest:           manifest,
		ActiveVersion:      manifest.Version,
		VersionCode:        manifest.VersionCode,
		Trust:              trust,
		Source:             tx.Source,
		DisabledComponents: append([]string(nil), carriedDisabled...),
		SignerRoles:        signers.RoleStrings(),
		VerifiedSigners:    append([]VerifiedSigner(nil), signers.VerifiedSigners...),
		provider:           provider,
	}
	if signers.Dev != nil {
		entry.DeveloperRootID = signers.Dev.RootKeyID
	}

	// Validate the complete new catalog and recompute every package grant before
	// allocating a UID, writing the package tree, or updating the ledger.
	prepared, err := m.prepareEntries(ctx, upsertEntry(previousEntries, entry))
	if err != nil {
		m.auditInstall(ctx, tx, false, err)
		return Entry{}, err
	}
	entry, ok := findEntry(prepared.entries, manifest.PackageID)
	if !ok {
		err := fmt.Errorf("pkgregistry: prepared entry %q disappeared", manifest.PackageID)
		m.auditInstall(ctx, tx, false, err)
		return Entry{}, err
	}

	uid, err := stableUID(m.stateDir, manifest.PackageID, manifest.Version, trust, tx.Source)
	if err != nil {
		m.auditInstall(ctx, tx, false, err)
		return Entry{}, err
	}
	entry.UID = uid
	for i := range prepared.entries {
		if prepared.entries[i].Manifest.PackageID == manifest.PackageID {
			prepared.entries[i].UID = uid
			break
		}
	}
	subj := authority.Subject{PackageID: manifest.PackageID, UID: uid}

	destDir := filepath.Join(m.packageRoot, manifest.PackageID, manifest.Version)
	// A same-version reinstall (repairing a damaged install) must explicitly ask to overwrite. checkUpgrade above already allowed
	// this case, but authority uses RENAME_NOREPLACE by default to refuse any existing target;
	// without stating that intent, the policy couldn't be delivered and the install would fail with a bare renameat2 EEXIST.
	//
	// The criterion is ActiveVersion, not VersionCode: what collides is the path
	// <PackageRoot>/<id>/<version>, and the path is decided by the version string. The same version_code
	// with a different version string is a malformed manifest, but that should be caught by manifest validation,
	// not lead this code to overwrite a directory it shouldn't because of a wrong judgement
	replacing := hadPrev && prev.ActiveVersion == manifest.Version
	if err := m.auth.InstallVerifiedPackage(ctx, subj, authority.InstallVerifiedPackageRequest{
		StagingDir:      tx.StagingDir,
		DestDir:         destDir,
		ReplaceExisting: replacing,
	}); err != nil {
		m.auditInstall(ctx, tx, false, err)
		return Entry{}, err
	}

	// Failure compensation: InstallVerifiedPackage has already written the code to disk (RENAME_NOREPLACE guarantees destDir
	// was created by this call), and any later failing step would leave an orphan directory with code on disk but nothing in the Registry,
	// which the next same-version reinstall would hit with RENAME_NOREPLACE and never fix. committed is set true after a successful commit;
	// otherwise this closure deletes the code directory just written (in an upgrade it deletes the new version directory, leaving the old one alone)
	committed := false
	newDataDir := ""
	defer func() {
		if committed {
			return
		}
		// An overwrite install doesn't delete the code directory. In that case destDir is the package's only code
		// directory: the old tree was swapped out by RENAME_EXCHANGE and deleted. Deleting it would give "records say
		// version X is installed, and there's nothing on disk", far worse than not rolling back.
		//
		// Not deleting is safe: the tree in destDir has passed full signature and digest re-checks, and
		// the records still point at version X, so records and disk agree; only the content was replaced with the newly committed copy.
		// And the premise of a reinstall is that the old copy was broken anyway.
		if !replacing {
			if rerr := m.auth.RemovePackageTree(ctx, subj, authority.RemovePackageTreeRequest{
				Root: m.packageRoot, Path: destDir,
			}); rerr != nil {
				m.aud.Record(ctx, audit.Event{
					Action: "pkgregistry.Install.compensate", Subject: manifest.PackageID, Denied: true, Err: rerr,
				})
			}
		}
		if newDataDir != "" {
			_ = m.auth.RemovePackageTree(ctx, subj, authority.RemovePackageTreeRequest{
				Root: m.dataRoot, Path: newDataDir,
			})
		}
	}()

	// The system user must be ensured on every install, not only when !hadPrev.
	//
	// It's different from the data directory: the data directory is local state and stays once created, while /etc/passwd may
	// be replaced by an image OTA, cleaned up by hand by ops, or this package may have been installed elsewhere and restored along with
	// its bookkeeping files. EnsureAppUser is idempotent (does nothing when the entry exists), so calling it unconditionally
	// costs one file read, while missing it means this package's components can never start (217/USER).
	//
	// Before the data directory: the directory's owner is this UID, and with the user in place first the owner has a name.
	if err := m.auth.EnsureAppUser(ctx, subj, authority.EnsureAppUserRequest{
		UID: uid, GID: uid, Name: authority.AppUserName(uid),
	}); err != nil {
		m.auditInstall(ctx, tx, false, err)
		return Entry{}, err
	}

	// The private data directory is per package (not per version), and upgrades don't touch it: it's created on the first
	// install and kept across upgrades. Creating it again unconditionally on upgrade would fail with EEXIST from Linux mkdirat
	// and bring down the whole upgrade (osCreateDataDir in ops.go). So it's only created on a fresh install
	if !hadPrev {
		dataDir := filepath.Join(m.dataRoot, manifest.PackageID)
		if _, err := m.auth.CreatePrivateDataDirectory(ctx, subj, authority.CreateDataDirRequest{
			Path: dataDir, UID: uid, GID: uid, Perm: 0o700,
		}); err != nil {
			m.auditInstall(ctx, tx, false, err)
			return Entry{}, err
		}
		newDataDir = dataDir // created this time; rolled back with the compensation
	}

	st := registryState{
		PackageID: manifest.PackageID, ActiveVersion: manifest.Version, VersionCode: manifest.VersionCode,
		UID: uid, Trust: trust.String(), Source: tx.Source.String(),
		GrantedPermissions: entry.GrantedPermissions, DisabledComponents: carriedDisabled,
	}
	if signers.Dev != nil {
		st.LineageRootKeyID = signers.Dev.RootKeyID
		st.LineageKeyIDs = signers.Dev.KeyIDs
	}
	if err := saveRegistryState(m.stateDir, st); err != nil {
		m.auditInstall(ctx, tx, false, err)
		return Entry{}, err
	}

	if err := m.publishCatalogLast(ctx, previousEntries, prepared); err != nil {
		m.auditInstall(ctx, tx, false, err)
		return Entry{}, err
	}
	committed = true // commit succeeded: no more compensating deletion

	// Default grants must be stored after the commit: SetRuntimeState requires the permission to be in the install-time
	// grant set already, and that set is only pushed by perm.Replace in publishCatalogLast above.
	// Calling it a step earlier always gets "no installed grant"
	m.applyInstallConsent(ctx, entry, prepared.candidate.Snapshot())
	// A successful upgrade or same-version replacement invalidates every route
	// and stream authorized by the old package generation. Revoke after the
	// catalog commit (a failed pre-commit install must not disturb the live
	// version) and before asking service.Manager to reload the processes.
	if hadPrev && m.transferRevoker != nil {
		m.transferRevoker.RevokePackage(manifest.PackageID)
	}

	// Upgrade: switch the running old version's components to the new version. Otherwise the old version keeps running and, after a crash, is restarted from the old
	// Entry (an upgrade fix). ReloadPackage stops old instances before starting the new version, so start/stop on the shared unit
	// name don't race. Skipped when stopper isn't wired (nil); then there are no running components either
	if hadPrev && m.stopper != nil {
		if rerr := m.stopper.ReloadPackage(ctx, manifest.PackageID); rerr != nil {
			m.aud.Record(ctx, audit.Event{
				Action: "pkgregistry.Install.reload", Subject: manifest.PackageID, Denied: true, Err: rerr,
			})
			err := fmt.Errorf("%w: %w", ErrReloadPending, rerr)
			m.auditInstall(ctx, tx, false, err)
			return entry, err
		}
	}

	m.auditInstall(ctx, tx, true, nil)
	return entry, nil
}

// applyInstallConsent stores the USER_CONSENT permissions this package was granted as runtime Granted.
//
// # Why "granted by default" rather than "granted only after asking"
//
// The runtime state of USER_CONSENT permissions defaults to NOT_REQUESTED, and apps have no API for
// requesting permissions. So before there was an install confirmation screen, sensitive permissions an ordinary app requested could never be obtained:
// the app couldn't help itself, the user had no UI to approve them, and the symptom was features silently not working.
//
// This system's choice is to grant on install, and the user can switch them off one by one in permission management later:
//
//	installed         -> the USER_CONSENT permissions it was entitled to are Granted directly, and the app works at once
//	don't want it     -> Settings → Permissions; whichever is switched off stops working immediately (the ACL and routing both update live)
//	after switch-off  -> the app can make one more dynamic request, and the user decides on the spot
//
// This is a deliberate trade-off, not an oversight. The cost is that an app gets all the sensitive capabilities it requested
// (motion control included) the moment it's installed, and the user is informed afterwards rather than asked first. In return the permission system goes from
// "nothing can be obtained" to "everything works and can be revoked". The former isn't safer; it just makes sensitive features unusable as a whole,
// and nobody switches off a feature that doesn't work.
//
// # It still only accepts the intersection of the two
//
// What's stored is entry.GrantedPermissions ∩ USER_CONSENT:
//
//	not in GrantedPermissions   -> the install-time decision never approved it (not enough trust, wrong signing role,
//	                               or the origin isn't the system image). A default grant can't conjure it up
//	not USER_CONSENT            -> it has no "runtime state": NORMAL takes effect on install,
//	                               PRIVILEGED/SYSTEM_ONLY are decided by origin and signature.
//	                               Writing it would only get an error back from SetRuntimeState
//
// # Upgrades don't overwrite the user's existing decisions
//
// Only uninstall clears _grants.json (ClearPackage is only called on the uninstall path); upgrades don't. So on reinstall
// the previous decisions are still there, and a permission the user explicitly switched off must never be switched back on by a version update:
// that would give every app a way round the user's decision, getting the permission back by shipping a new version.
//
// So only the ones still NOT_REQUESTED (never decided) are granted by default. Denied /
// DeniedPermanent are always skipped, and ones already Granted needn't be written again.
//
// # Why a failure doesn't roll back the install
//
// By this point the package has been committed. If one permission wasn't stored, the user can click once more in Settings; rolling back
// the whole installed package for it would cost far more than it gains. But it must leave an audit entry, otherwise the user would see
// a permission that's "installed but not in effect", with no trace in the system explaining why
func (m *Module) applyInstallConsent(
	ctx context.Context, entry Entry, snap *catalog.Snapshot,
) {
	if m.perm == nil || snap == nil {
		return
	}
	for _, permissionID := range entry.GrantedPermissions {
		definition, ok := snap.Permission(permissionID)
		if !ok || definition.GrantMode != ipcv1.GrantMode_GRANT_MODE_USER_CONSENT {
			continue
		}
		// See "Upgrades don't overwrite the user's existing decisions" above: only grant the ones never decided
		if state := m.perm.GrantStateOf(entry.Manifest.PackageID, permissionID); state != permission.GrantStateNotRequested {
			m.aud.Record(ctx, audit.Event{
				Action:  "pkgregistry.Install.defaultGrant.kept",
				Subject: entry.Manifest.PackageID,
				Detail:  fmt.Sprintf("%s state=%v", permissionID, state),
			})
			continue
		}
		if err := m.perm.SetRuntimeState(
			entry.Manifest.PackageID, permissionID, permission.GrantStateGranted,
		); err != nil {
			m.aud.Record(ctx, audit.Event{
				Action:  "pkgregistry.Install.defaultGrant",
				Subject: entry.Manifest.PackageID,
				Denied:  true,
				Err:     err,
				Detail:  permissionID,
			})
		}
	}
}

// readPrevState reads the persisted records of a Package's installed version; the second return value is false if there are none
func (m *Module) readPrevState(packageID string) (registryState, bool) {
	sp, err := stateFilePath(m.stateDir, packageID)
	if err != nil {
		return registryState{}, false
	}
	st, err := readRegistryState(sp)
	if err != nil {
		return registryState{}, false
	}
	return st, true
}

func (m *Module) auditInstall(ctx context.Context, tx InstallTransaction, ok bool, err error) {
	m.aud.Record(ctx, audit.Event{
		Action: "pkgregistry.Install", Subject: tx.Source.String(), Denied: !ok, Err: err,
	})
}
