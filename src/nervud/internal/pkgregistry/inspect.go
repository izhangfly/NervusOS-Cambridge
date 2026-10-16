// This file is the read-only "look before installing" path: parse and verify a package in staging, and work out the
// USER_CONSENT permissions it requests, writing no state.
//
// # Why this path is needed
//
// The runtime state of USER_CONSENT permissions defaults to NOT_REQUESTED, and apps have no "request permission"
// API. So the only moment it can become GRANTED is at install: the confirmation screen lays the package's sensitive permissions
// out for the user, and the ones the user agrees to come in with the InstallRequest (see applyInstallConsent).
//
// But until now the confirmation screen had no way to know what those permissions were: ListGrants and List only cover installed packages,
// and the package about to be installed isn't in the Catalog yet. All it has is a .nspkg path.
//
// # Why parsing stays in the kernel
//
// The alternative is for the confirmation screen to unpack and read the manifest itself. That would put parsing of untrusted content
// before signature verification into a process holding perm.permission.admin, the UI with the highest privilege in the whole system
// and the worst place on the attack surface for a parser.
//
// More fundamentally, it's a division of labour: the confirmation screen makes no security decisions, and shouldn't even decide for itself
// "is this permission USER_CONSENT". That needs the permission definitions in the Catalog, which only the kernel has.
package pkgregistry

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	ipcv1 "github.com/nervus-os/nervus-ipc/protocol/ipcv1"

	"github.com/nervus-os/nervud/internal/audit"
	"github.com/nervus-os/nervud/internal/catalog"
)

// ErrPermissionDefinitionUnknown: the package requests a permission the Catalog doesn't define.
//
// Inspect doesn't fail because of it: an unknown permission is refused by IntersectAt at the install decision,
// which is the authoritative conclusion. Here it's just not listed on the confirmation screen; showing a permission with no text
// would leave the user no way to judge whether to agree.
var ErrPermissionDefinitionUnknown = errors.New("pkgregistry: permission is not defined in the catalog")

// ConsentPermission is one permission awaiting consent, for the confirmation screen to show.
//
// The text comes from the Catalog's permission definitions rather than being hard-coded in the UI: third-party packages can define their own permissions,
// and the UI can't know their names and descriptions in advance.
type ConsentPermission struct {
	ID              string
	DisplayNameZhCN string
	DisplayNameEN   string
	DescriptionZhCN string
	DescriptionEN   string
	RiskClass       ipcv1.RiskClass
}

// InspectResult is the result of one read-only inspection.
type InspectResult struct {
	// The identity the package claims. It comes from the manifest and has passed signature verification but not the install decision yet:
	// Inspect succeeding doesn't mean Install will (ABI mismatches, version downgrades, missing OEM countersignatures
	// and so on are only decided at the real install).
	PackageID   string
	Version     string
	VersionCode uint64

	// ConsentPermissions are the sensitive permissions this package requests that need the user's agreement.
	//
	// Already filtered to the GrantMode == USER_CONSENT tier: other modes have no question of "agree or not";
	// NORMAL takes effect on install, PRIVILEGED / SYSTEM_ONLY are decided by origin and signature.
	// Mixing them in would only make the confirmation screen show a pile of entries the user has no choice about.
	//
	// Sorted by permission ID, so the confirmation screen's order is stable (the same package shows the same order every time).
	ConsentPermissions []ConsentPermission

	// ManifestDigest is the hex string of sha256(the raw manifest bytes), used to bind this
	// Inspect's result and the following Install to the same content.
	//
	// # Which gap it closes
	//
	// The .nspkg sits in the user-data area shared across packages (see pkgmanagerd's handoffRoot),
	// so the caller could perfectly well swap the file between Inspect and Install: the permission list the user saw on the confirmation screen
	// comes from A, and what's actually installed is B. Each call is legitimate on its own, and no
	// single step can discover this; only binding the two together can.
	//
	// Usage: the confirmation screen passes this field back to Install unchanged, and the kernel refuses on a mismatch.
	//
	// # Why the manifest's digest is enough, without walking the whole tree
	//
	// The manifest contains the digests of every file, and the manifest itself is covered by the signature. So
	// "the manifest bytes are the same" implies "every file's expected digest is the same", and whether file contents match those digests
	// is re-checked by Install's VerifyDigests. Swapping any file would either fail
	// VerifyDigests or require changing the manifest, which would change this digest.
	//
	// So the file tree needn't be walked again here: Install already does that, and redoing it in Inspect
	// just to compute an identifier would make "take a look at what permissions it wants" take several seconds longer.
	ManifestDigest string
}

// Inspect parses and verifies the package in staging and returns the USER_CONSENT permissions it requests.
//
// # What it does and doesn't do
//
//	does:    parse the manifest -> multi-role signature checks (devmode relaxation as in Install) ->
//	         filter the USER_CONSENT tier by the Catalog's permission definitions
//	doesn't: anything after the trust decision: no ABI check, no digest check, no upgrade decision,
//	         no UID allocation, no disk writes, no changes to the Registry / Catalog / permission projection
//
// Signature verification is required. For a package with an invalid signature, the permission list in its manifest can't be taken seriously;
// showing it to the user would let an attacker decide what text appears on the confirmation screen. So this takes
// the same VerifySignature as Install, including devmode's relaxation conditions (otherwise installing your own package on a development machine would
// get stuck at the confirmation screen, while Install would have let it through).
//
// # Why no digest check
//
// A digest re-check walks the whole tree computing sha256, while Inspect only wants to read the permission list in the manifest,
// and that manifest is already covered by the signature. The real digest re-check is in Install,
// which is where "should these files go into the system" is decided. Doing it here too would only make the confirmation screen wait
// several seconds more and stop nothing: a package whose digests don't match is refused at Install, and by then the user
// still hasn't agreed.
//
// # No lock
//
// Inspect changes no state, so it doesn't need m.mu. It reads m.definitions.Current() for the permission
// definitions, an immutable snapshot, so a concurrent install publishing a new snapshot can't make this read see half a state.
func (m *Module) Inspect(ctx context.Context, stagingDir string) (InspectResult, error) {
	manifestBytes, err := os.ReadFile(filepath.Join(stagingDir, ManifestFileName))
	if err != nil {
		return InspectResult{}, fmt.Errorf("pkgregistry: read %s: %w", ManifestFileName, err)
	}
	sigBlock, err := os.ReadFile(filepath.Join(stagingDir, SignatureFileName))
	if err != nil {
		return InspectResult{}, fmt.Errorf("pkgregistry: read %s: %w", SignatureFileName, err)
	}

	manifest, err := ParseManifest(manifestBytes)
	if err != nil {
		return InspectResult{}, err
	}

	// ---- Signature checks. The same path as Install, including devmode relaxation ----
	dev := loadDevMode(m.stateDir)
	if _, sigErr := m.trust.VerifySignature(manifestBytes, sigBlock); sigErr != nil {
		m.aud.Record(ctx, audit.Event{
			Action:  "pkgregistry.Inspect.VerifySignature",
			Subject: manifest.PackageID,
			Denied:  true,
			Err:     sigErr,
		})
		if !dev.Enabled || !dev.AllowUnverifiedSignature {
			return InspectResult{}, sigErr
		}
		m.aud.Record(ctx, audit.Event{
			Action:  "pkgregistry.Inspect.devmode",
			Subject: manifest.PackageID,
			Detail:  "relaxed: allow_unverified_signature",
		})
	}

	// ---- Filter the USER_CONSENT tier by the Catalog's definitions ----
	//
	// It uses Current() rather than Preparing a candidate Catalog as Install does: Inspect is
	// read-only, while Prepare would load this package's ProviderArtifacts and validate the whole candidate catalog,
	// which would make "take a look at what permissions this package wants" fail because of a catalog conflict unrelated to permissions.
	//
	// The cost is that permissions the package defines itself can't be seen: they only get definitions once the package enters the Catalog, which
	// is after Install. So those permissions don't appear on the confirmation screen, the user can't agree to them,
	// and they stay NOT_REQUESTED at run time. That fails closed; nothing is granted for free because it couldn't be seen.
	// The user can still grant them later in permission management.
	snapshot := m.definitions.Current()
	if snapshot == nil {
		return InspectResult{}, ErrCatalogUnavailable
	}

	out := InspectResult{
		PackageID:   manifest.PackageID,
		Version:     manifest.Version,
		VersionCode: manifest.VersionCode,
		// The digest is computed over the raw bytes the signature covers, not by re-serialising the struct after
		// ParseManifest: the latter would drop fields in the manifest we don't know, so two
		// packages with different contents could get the same digest, which is exactly what this field guards against.
		ManifestDigest: hex.EncodeToString(sha256Sum(manifestBytes)),
	}
	for _, permissionID := range manifest.Permissions {
		definition, ok := snapshot.Permission(permissionID)
		if !ok {
			// An unknown permission isn't listed and doesn't fail the whole Inspect: the authoritative decision is
			// Install's IntersectAt, which refuses it. Record an audit entry, otherwise
			// "the permission I requested didn't appear on the confirmation screen" would leave no trace
			m.aud.Record(ctx, audit.Event{
				Action:  "pkgregistry.Inspect.permission",
				Subject: manifest.PackageID,
				Denied:  true,
				Err:     ErrPermissionDefinitionUnknown,
				Detail:  permissionID,
			})
			continue
		}
		if definition.GrantMode != ipcv1.GrantMode_GRANT_MODE_USER_CONSENT {
			continue
		}
		out.ConsentPermissions = append(out.ConsentPermissions, consentPermission(definition))
	}
	// Sorting keeps the confirmation screen's order stable. The order of manifest.Permissions is up to the package
	// author and shouldn't decide the order the user sees
	sort.Slice(out.ConsentPermissions, func(i, j int) bool {
		return out.ConsentPermissions[i].ID < out.ConsentPermissions[j].ID
	})

	m.aud.Record(ctx, audit.Event{
		Action:  "pkgregistry.Inspect",
		Subject: manifest.PackageID,
		Detail: fmt.Sprintf("version=%s consent_permissions=%d",
			manifest.Version, len(out.ConsentPermissions)),
	})
	return out, nil
}

func consentPermission(d catalog.PermissionDefinition) ConsentPermission {
	return ConsentPermission{
		ID:              d.ID,
		DisplayNameZhCN: d.DisplayNameZhCN,
		DisplayNameEN:   d.DisplayNameEN,
		DescriptionZhCN: d.DescriptionZhCN,
		DescriptionEN:   d.DescriptionEN,
		RiskClass:       d.RiskClass,
	}
}

// sha256Sum is the slice form of sha256.Sum256, saving the [:] conversion at call sites.
func sha256Sum(b []byte) []byte {
	sum := sha256.Sum256(b)
	return sum[:]
}

// ErrManifestDigestMismatch: what's being installed isn't the same copy the user saw when confirming.
//
// It can only happen when InstallTransaction.ExpectedManifestDigest is non-empty. It doesn't mean "the package is broken":
// the signature and digests may be perfectly valid; this just isn't the package the confirmation screen showed the user.
var ErrManifestDigestMismatch = errors.New("pkgregistry: manifest digest does not match the confirmed package")
