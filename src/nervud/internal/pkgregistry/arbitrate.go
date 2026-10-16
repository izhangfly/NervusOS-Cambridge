// This file holds the core rule of the final install decision: from the install source and the verified multi-role signatures, decide the trust
// profile this Package finally gets
package pkgregistry

import (
	"errors"

	"github.com/nervus-os/nervud/internal/identity"
)

// ErrInvalidSource: an Install transaction's Source isn't a legal source (missing / zero value).
// Admission checks (developer signature required, OEM countersignature) only trigger for known sources, so unknown sources must be refused as a whole,
// or they'd bypass every admission check and become ghost packages
var ErrInvalidSource = errors.New("pkgregistry: install transaction has invalid source")

// Source is where the manifest came from, which decides whether it's eligible for non-Ordinary trust
type Source int

const (
	// SourceUnspecified: the zero value is invalid, so uninitialised calls can't pass as a legal source
	SourceUnspecified Source = iota

	// SourceSystemImage: a Package built into the read-only system image (/usr/lib/nervus/system-packages/)
	SourceSystemImage

	// SourceDynamicInstall: a dynamic install at run time (pkgmanagerd's staging flow)
	SourceDynamicInstall
)

func (s Source) String() string {
	switch s {
	case SourceSystemImage:
		return "system-image"
	case SourceDynamicInstall:
		return "dynamic-install"
	default:
		return "unspecified"
	}
}

// Arbitrate is the final trust decision: source + multi-role signature conclusion -> final TrustProfile.
//
// Only packages from the read-only system image are eligible
// for non-Ordinary trust. A manifest with platform/oem signatures that comes through the dynamic
// install path still only gets Ordinary: the criterion is "came from the system image", not the signature itself.
// A manifest can't escalate privilege on the dynamic install path just by carrying a high-role signature.
//
// SignerSet.Trust is already the highest trust VerifySignature worked out from the multi-role signatures (keys endorsed by the platform root/OEM
// root -> Platform/OEM, developer self-signed -> Ordinary); this function only handles the source threshold:
// dynamic installs always drop to Ordinary, and the system image passes SignerSet.Trust through (but Unspecified normalises to Ordinary).
//
// The intersection of requested permissions, registered permissions and the trust threshold is computed by permission.Intersect
// (see install.go), not here; this function only answers how much trust to give
func Arbitrate(src Source, signers SignerSet) identity.TrustProfile {
	if src != SourceSystemImage {
		return identity.TrustOrdinary
	}
	// System image source: pass VerifySignature's conclusion through, but Unspecified (unverified / failed verification)
	// normalises to Ordinary: no privileges are issued without a verification result
	if signers.Trust == identity.TrustUnspecified {
		return identity.TrustOrdinary
	}
	return signers.Trust
}
