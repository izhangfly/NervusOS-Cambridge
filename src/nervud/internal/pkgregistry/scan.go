// This file implements the fixed boot scan: it only scans two controlled sources, never recursing through arbitrary directories:
//
//	/usr/lib/nervus/system-packages/*/manifest.json   Packages built into the system image
//	/var/lib/nervus/registry/                         the index of dynamically installed versions nervud committed itself
//	                                                  and marked active
//
// It also handles persisting "one stable UID per Package": once a UID is allocated,
// it's written to a bookkeeping file under /var/lib/nervus/registry/ and stays the same across reboots
package pkgregistry

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/nervus-os/nervud/internal/authority"
	"github.com/nervus-os/nervud/internal/identity"
)

const (
	// DefaultSystemPackagesDir is the fixed source of Packages built into the read-only system image
	DefaultSystemPackagesDir = "/usr/lib/nervus/system-packages"

	// DefaultRegistryStateDir holds the trusted state of the Package Registry, active versions and UID allocation;
	// only nervud may change it
	DefaultRegistryStateDir = "/var/lib/nervus/registry"

	allocatorStateFile = "_allocator.json"
)

// ScanResult is the result of one boot scan
type ScanResult struct {
	Entries []Entry
	Skipped []SkippedPackage
}

// SkippedPackage records a Package that couldn't be loaded and why, for audit and diagnostics
type SkippedPackage struct {
	Path string
	Err  error
}

// Scan runs one complete boot scan.
//
// packageRoot is usually authority.DefaultInvariants.PackageRoot; the caller
// passes it explicitly rather than this function hard-coding it, so tests can isolate it with t.TempDir.
//
// trust is the root-of-trust view for signature verification: a system image package's trust is decided by the real
// verification result of its manifest.sig (through Arbitrate), not "TrustPlatform because it's in the system directory". When trust
// is the zero value (LoadTrustStore failed), platform/oem signatures all fail verification -> everything fails closed
// to Ordinary
func Scan(stateDir, systemPackagesDir, packageRoot string, trust TrustStore, log *slog.Logger) ScanResult {
	var result ScanResult

	sysEntries, sysSkipped := scanSystemImage(stateDir, systemPackagesDir, trust, log)
	result.Entries = append(result.Entries, sysEntries...)
	result.Skipped = append(result.Skipped, sysSkipped...)

	dynEntries, dynSkipped := scanDynamicInstalls(stateDir, packageRoot, trust, log)
	result.Entries = append(result.Entries, dynEntries...)
	result.Skipped = append(result.Skipped, dynSkipped...)

	return result
}

// scanSystemImage scans the Packages built into the system image.
//
// Trust isn't "TrustPlatform because it's in the system directory": that way, when the trust store failed,
// the "non-Ordinary trust disabled" main.go logged would be false, and any package written into the system directory could
// get Platform for free. Instead: read manifest.sig -> verify with the TrustStore checked by the embedded root ->
// Arbitrate(SourceSystemImage, signers) decides trust. Failing verification or a missing sig -> fail closed
// to Ordinary (without skipping the whole package: a system package should still load and run even with only Ordinary, just without
// privileges), while a digest mismatch is still a hard failure (a sign of a corrupt or tampered image)
func scanSystemImage(stateDir, systemPackagesDir string, trust TrustStore, log *slog.Logger) ([]Entry, []SkippedPackage) {
	var entries []Entry
	var skipped []SkippedPackage

	matches, err := filepath.Glob(filepath.Join(systemPackagesDir, "*", "manifest.json"))
	if err != nil {
		skipped = append(skipped, SkippedPackage{Path: systemPackagesDir, Err: err})
		return entries, skipped
	}

	for _, manifestPath := range matches {
		pkgDir := filepath.Dir(manifestPath)

		manifestBytes, err := os.ReadFile(manifestPath)
		if err != nil {
			skipped = append(skipped, SkippedPackage{Path: manifestPath, Err: err})
			continue
		}
		m, err := ParseManifest(manifestBytes)
		if err != nil {
			skipped = append(skipped, SkippedPackage{Path: manifestPath, Err: err})
			continue
		}

		diff, err := VerifyDigests(pkgDir, m.Digests)
		if err != nil {
			skipped = append(skipped, SkippedPackage{Path: manifestPath, Err: err})
			continue
		}
		if !diff.Clean() {
			skipped = append(skipped, SkippedPackage{
				Path: manifestPath,
				Err:  fmt.Errorf("%w: %+v", ErrDigestMismatch, diff),
			})
			continue
		}

		// Signature verification decides trust. A missing sig or failed verification -> Ordinary (fail-closed), still loaded;
		// valid platform/oem signatures -> the matching privileged trust through Arbitrate
		pkgTrust := identity.TrustOrdinary
		var signerRoles []string
		var verifiedSigners []VerifiedSigner
		var developerRootID string
		sigPath := filepath.Join(pkgDir, SignatureFileName)
		if sigBytes, serr := os.ReadFile(sigPath); serr == nil {
			if signers, verr := trust.VerifySignature(manifestBytes, sigBytes); verr == nil {
				pkgTrust = Arbitrate(SourceSystemImage, signers)
				signerRoles = signers.RoleStrings()
				verifiedSigners = append([]VerifiedSigner(nil), signers.VerifiedSigners...)
				if signers.Dev != nil {
					developerRootID = signers.Dev.RootKeyID
				}
			} else if log != nil {
				log.Warn("pkgregistry: system package signature not verified; downgrading to Ordinary",
					"package_id", m.PackageID, "err", verr)
			}
		} else if log != nil {
			log.Warn("pkgregistry: system package missing manifest.sig; treating as Ordinary",
				"package_id", m.PackageID)
		}

		provider, err := loadRequiredProviderArtifacts(pkgDir, m)
		if err != nil {
			skipped = append(skipped, SkippedPackage{Path: manifestPath, Err: err})
			continue
		}

		uid, err := stableUID(stateDir, m.PackageID, m.Version, pkgTrust, SourceSystemImage)
		if err != nil {
			skipped = append(skipped, SkippedPackage{Path: manifestPath, Err: err})
			continue
		}

		if log != nil {
			log.Info("pkgregistry: loaded system package", "package_id", m.PackageID, "version", m.Version, "trust", pkgTrust.String())
		}
		// Scan never supplies grants. Module.Start prepares the complete Catalog
		// candidate and recomputes every Entry against that exact snapshot.
		entries = append(entries, Entry{
			Manifest:        m,
			ActiveVersion:   m.Version,
			VersionCode:     m.VersionCode,
			UID:             uid,
			Trust:           pkgTrust,
			Source:          SourceSystemImage,
			SignerRoles:     signerRoles,
			VerifiedSigners: verifiedSigners,
			DeveloperRootID: developerRootID,
			provider:        provider,
		})
	}
	return entries, skipped
}

// scanDynamicInstalls reads dynamic package ledgers, then revalidates the
// manifest signature, developer lineage, current device countersign policy,
// payload digests, and ProviderArtifacts. The persisted grant list is ignored.
func scanDynamicInstalls(
	stateDir, packageRoot string,
	trust TrustStore,
	log *slog.Logger,
) ([]Entry, []SkippedPackage) {
	var entries []Entry
	var skipped []SkippedPackage

	matches, err := filepath.Glob(filepath.Join(stateDir, "*.json"))
	if err != nil {
		skipped = append(skipped, SkippedPackage{Path: stateDir, Err: err})
		return entries, skipped
	}

	for _, statePath := range matches {
		if filepath.Base(statePath) == allocatorStateFile ||
			filepath.Base(statePath) == "_grants.json" {
			continue
		}

		st, err := readRegistryState(statePath)
		if err != nil {
			skipped = append(skipped, SkippedPackage{Path: statePath, Err: err})
			continue
		}
		if st.Source != SourceDynamicInstall.String() {
			continue // system packages' bookkeeping files are handled on the scanSystemImage side
		}
		if st.RemovalPending {
			// A tombstone is authoritative even if code/data cleanup did not finish.
			// Uninstall can retry from the ledger; startup must never reactivate it.
			continue
		}

		manifestPath := filepath.Join(packageRoot, st.PackageID, st.ActiveVersion, "manifest.json")
		manifestBytes, err := os.ReadFile(manifestPath)
		if err != nil {
			skipped = append(skipped, SkippedPackage{Path: manifestPath, Err: err})
			continue
		}
		m, err := ParseManifest(manifestBytes)
		if err != nil {
			skipped = append(skipped, SkippedPackage{Path: manifestPath, Err: err})
			continue
		}
		if m.PackageID != st.PackageID ||
			m.Version != st.ActiveVersion ||
			m.VersionCode != st.VersionCode {
			skipped = append(skipped, SkippedPackage{
				Path: manifestPath,
				Err: fmt.Errorf(
					"pkgregistry: dynamic manifest identity/version differs from ledger: "+
						"manifest=%s@%s/%d ledger=%s@%s/%d",
					m.PackageID, m.Version, m.VersionCode,
					st.PackageID, st.ActiveVersion, st.VersionCode,
				),
			})
			continue
		}

		sigPath := filepath.Join(filepath.Dir(manifestPath), SignatureFileName)
		sigBytes, err := os.ReadFile(sigPath)
		if err != nil {
			skipped = append(skipped, SkippedPackage{Path: sigPath, Err: err})
			continue
		}
		signers, err := trust.VerifySignature(manifestBytes, sigBytes)
		if err != nil {
			skipped = append(skipped, SkippedPackage{Path: sigPath, Err: err})
			continue
		}
		if !signers.HasDeveloper || signers.Dev == nil {
			skipped = append(skipped, SkippedPackage{
				Path: sigPath,
				Err:  ErrNoDeveloperSignature,
			})
			continue
		}
		if st.LineageRootKeyID == "" ||
			signers.Dev.RootKeyID != st.LineageRootKeyID {
			skipped = append(skipped, SkippedPackage{
				Path: sigPath,
				Err: fmt.Errorf("%w: ledger root %q, verified root %q",
					ErrSignerMismatch, st.LineageRootKeyID, signers.Dev.RootKeyID),
			})
			continue
		}
		if trust.policyRequireOEMCountersign() && !signers.HasOEMCountersign {
			skipped = append(skipped, SkippedPackage{
				Path: sigPath,
				Err:  ErrOEMCountersignRequired,
			})
			continue
		}

		diff, err := VerifyDigests(filepath.Dir(manifestPath), m.Digests)
		if err != nil {
			skipped = append(skipped, SkippedPackage{Path: manifestPath, Err: err})
			continue
		}
		if !diff.Clean() {
			skipped = append(skipped, SkippedPackage{
				Path: manifestPath,
				Err:  fmt.Errorf("%w: %+v", ErrDigestMismatch, diff),
			})
			continue
		}

		provider, err := loadRequiredProviderArtifacts(filepath.Dir(manifestPath), m)
		if err != nil {
			skipped = append(skipped, SkippedPackage{Path: manifestPath, Err: err})
			continue
		}

		if log != nil {
			log.Info("pkgregistry: loaded dynamic package", "package_id", m.PackageID, "version", m.Version)
		}
		entries = append(entries, Entry{
			Manifest:           m,
			ActiveVersion:      st.ActiveVersion,
			VersionCode:        st.VersionCode,
			UID:                st.UID,
			Trust:              identity.TrustOrdinary,
			Source:             SourceDynamicInstall,
			SignerRoles:        signers.RoleStrings(),
			VerifiedSigners:    append([]VerifiedSigner(nil), signers.VerifiedSigners...),
			DisabledComponents: st.DisabledComponents,
			DeveloperRootID:    signers.Dev.RootKeyID,
			provider:           provider,
		})
	}
	return entries, skipped
}

/*func readManifest(path string) (Manifest, error) {
	data, err:= os.ReadFile(path)
	if err != nil {
		return Manifest{}, fmt.Errorf("pkgregistry: read manifest %s: %w", path, err)
	}
	return ParseManifest(data)
}*/

// stableUID returns the UID already persisted for packageID; if this is the first time the Package is seen,
// it allocates a new one and writes it atomically to the bookkeeping file
func stableUID(stateDir, packageID, version string, trust identity.TrustProfile, src Source) (uint32, error) {
	sp, err := stateFilePath(stateDir, packageID)
	if err != nil {
		return 0, err
	}
	existing, err := readRegistryState(sp)
	if err == nil {
		return existing.UID, nil
	}
	if !os.IsNotExist(err) {
		return 0, err
	}

	uid, err := allocateUID(stateDir)
	if err != nil {
		return 0, err
	}
	st := registryState{
		PackageID:     packageID,
		ActiveVersion: version,
		UID:           uid,
		Trust:         trust.String(),
		Source:        src.String(),
	}
	if err := saveRegistryState(stateDir, st); err != nil {
		return 0, err
	}
	return uid, nil
}

// ---- Bookkeeping files (one JSON file per Package) ------------------------------

type registryState struct {
	PackageID     string `json:"package_id"`
	ActiveVersion string `json:"active_version"`
	VersionCode   uint64 `json:"version_code"`
	UID           uint32 `json:"uid"`
	Trust         string `json:"trust"`
	Source        string `json:"source"`
	// GrantedPermissions is retained for diagnostics/backward-compatible state
	// decoding only. Startup never trusts it and always recomputes from the
	// current verified evidence and candidate Catalog.
	GrantedPermissions []string `json:"granted_permissions,omitempty"`

	// LineageRootKeyID / LineageKeyIDs are the developer signature's lineage digest, for the upgrade-time
	// signing continuity check. Empty for unverified (devmode) installs:
	// no identity anchor, and checkUpgrade relaxes continuity based on that (see upgrade.go)
	LineageRootKeyID string   `json:"lineage_root_key_id,omitempty"`
	LineageKeyIDs    []string `json:"lineage_key_ids,omitempty"`

	// DisabledComponents is the list of disabled Component IDs.
	// Disabling is recorded per Component and still holds after upgrades/reboots
	DisabledComponents []string `json:"disabled_components,omitempty"`

	// RemovalPending is an uninstall tombstone. It is persisted before any
	// runtime projection is removed and remains until code, data, grants, and
	// the ledger itself have all been cleaned. RemovalComponents lets an
	// in-process retry finish stopping units after the active Registry entry is
	// deliberately gone.
	RemovalPending    bool     `json:"removal_pending,omitempty"`
	RemovalComponents []string `json:"removal_components,omitempty"`
}

// stateFilePath computes the path of a Package's bookkeeping file, with a defence-in-depth check.
//
// This path doesn't go through the Authority Gate (bookkeeping files don't cross a trust boundary; see the notes on writeFileAtomic),
// so if the packageID it receives came from a hostile manifest and wasn't checked, it would be a hole letting
// nervud write files outside stateDir as root. The root fix is in
// manifest.validate's validPackageID; this is the second line: even if upstream missed it, the joined
// path is guaranteed to stay strictly under stateDir, and refusals include the full resolved path after Clean for forensics
func stateFilePath(dir, packageID string) (string, error) {
	if !validPackageID(packageID) {
		return "", fmt.Errorf("%w: %q", ErrInvalidPackageID, packageID)
	}
	p := filepath.Join(dir, packageID+".json")
	cleanDir := filepath.Clean(dir)
	rel, err := filepath.Rel(cleanDir, p)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", fmt.Errorf("%w: state path for %q escapes %q (resolved %q)",
			ErrInvalidPackageID, packageID, cleanDir, filepath.Clean(p))
	}
	return p, nil
}

func readRegistryState(path string) (registryState, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return registryState{}, err // the caller uses os.IsNotExist to tell "never seen" from a real I/O error
	}
	var st registryState
	if err := json.Unmarshal(data, &st); err != nil {
		return registryState{}, fmt.Errorf("pkgregistry: decode state %s: %w", path, err)
	}
	return st, nil
}

func saveRegistryState(dir string, st registryState) error {
	sp, err := stateFilePath(dir, st.PackageID)
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return fmt.Errorf("pkgregistry: encode state for %s: %w", st.PackageID, err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("pkgregistry: create registry state dir %s: %w", dir, err)
	}
	return writeFileAtomic(sp, data, 0o644)
}

// ---- UID allocator -----------------------------------------------------------

type allocatorState struct {
	NextUID uint32 `json:"next_uid"`
}

// allocateUID allocates the next stable Package UID, persisted in dir/_allocator.json.
//
// v1 simplification: only monotonic high-water-mark allocation, never reclaiming released UIDs. That's the simplest safe implementation
// of the architectural requirement "never reuse unsafely right after uninstall"; the cost is that the UID space
// only ever shrinks, and a real "reclaim after a cool-down" policy is left for later design. For now, simplicity buys safety
func allocateUID(dir string) (uint32, error) {
	inv := authority.DefaultInvariants()
	path := filepath.Join(dir, allocatorStateFile)

	next := inv.MinAppUID
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		var st allocatorState
		if uerr := json.Unmarshal(data, &st); uerr != nil {
			return 0, fmt.Errorf("pkgregistry: decode allocator state: %w", uerr)
		}
		if st.NextUID > next {
			next = st.NextUID
		}
	case os.IsNotExist(err):
		// First allocation, starting at MinAppUID
	default:
		return 0, fmt.Errorf("pkgregistry: read allocator state: %w", err)
	}

	if next > inv.MaxAppUID {
		return 0, fmt.Errorf("pkgregistry: uid space exhausted (max %d)", inv.MaxAppUID)
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return 0, fmt.Errorf("pkgregistry: create registry state dir %s: %w", dir, err)
	}
	out, err := json.MarshalIndent(allocatorState{NextUID: next + 1}, "", "  ")
	if err != nil {
		return 0, fmt.Errorf("pkgregistry: encode allocator state: %w", err)
	}
	if err := writeFileAtomic(path, out, 0o644); err != nil {
		return 0, err
	}
	return next, nil
}

// writeFileAtomic writes data to path atomically: write a temporary file in the same directory, then rename,
// so no half-written state appears to a crash or a concurrent reader.
//
// This is ordinary file I/O with the standard library's os, not touching the three packages depguard restricts
// (syscall/x-sys/os-exec). pkgregistry's own bookkeeping state doesn't need to go through the Authority Gate; the Gate is only for
// operations crossing a trust boundary (committing staging as the final code directory, creating a private
// data directory belonging to some UID); see install.go
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("pkgregistry: create temp file in %s: %w", dir, err)
	}
	tmpPath := tmp.Name()
	// After a successful Rename tmpPath is no longer there, so this Remove fails and can be ignored;
	// only on paths that bail out before Rename does this defer really clean up
	defer func() { _ = os.Remove(tmpPath) }()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("pkgregistry: write temp file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("pkgregistry: sync temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("pkgregistry: close temp file: %w", err)
	}
	if err := os.Chmod(tmpPath, perm); err != nil {
		return fmt.Errorf("pkgregistry: chmod temp file: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("pkgregistry: rename into place: %w", err)
	}
	return nil
}
