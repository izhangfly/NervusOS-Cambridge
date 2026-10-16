// This file is the cryptographic core of signature verification: Ed25519 multi-role parallel signature checks,
// key lineage chain checks, and loading and deciding on the three-layer root of trust (embedded platform root -> trust bundle ->
// each Package's signatures).
//
// Boundary discipline:
//   - Signatures are always verified against bytes, covering signDomain || the raw manifest.json bytes;
//     a parsed Manifest is never re-serialised for comparison (sigblock.go restates this).
//   - The root of trust isn't kept in data that file write permissions can change: the platform root is compiled into the binary, the bundle is
//     signed by it, and failing verification fails closed.
package pkgregistry

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/nervus-os/nervud/internal/identity"
)

var (
	// ErrSignatureInvalid: one signature failed cryptographic verification. This is "verified but failed",
	// which differs from "couldn't verify": the former is a real attack lead / corruption and must fail closed and be recorded as a violation
	ErrSignatureInvalid = errors.New("pkgregistry: signature verification failed")

	// ErrKeyIDMismatch: the embedded public key disagrees with its self-reported key_id (key_id must equal the public key's sha256)
	ErrKeyIDMismatch = errors.New("pkgregistry: key_id does not match embedded public key")

	// ErrUntrustedSigner: the key_id of some non-developer signature isn't in the trust bundle,
	// or the key isn't authorised to act in the role it claims
	ErrUntrustedSigner = errors.New("pkgregistry: signer key not trusted for its role")

	// ErrLineageBroken: some hop of the lineage chain has a signed_by_prev that fails verification, or a node key doesn't match its key_id
	ErrLineageBroken = errors.New("pkgregistry: lineage chain verification failed")

	// ErrDeveloperKeyNotCurrent: the developer signature wasn't made by the last node of the lineage (the currently valid key)
	ErrDeveloperKeyNotCurrent = errors.New("pkgregistry: developer signature not from current lineage key")

	// ErrNoDeveloperSignature: a dynamically installed package lacks a developer role signature. It's the anchor of identity and
	// lineage, and without it TOFU / identity hijack protection is impossible
	ErrNoDeveloperSignature = errors.New("pkgregistry: package has no developer signature")

	// ErrTrustBundleInvalid: the trust bundle's own platform root signature fails verification
	ErrTrustBundleInvalid = errors.New("pkgregistry: trust bundle signature invalid")
)

// keyIDOf computes a public key's canonical key_id: "sha256:" + hex(sha256(rawpubkey))
func keyIDOf(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// developerSignMessage returns the bytes the developer signature should cover.
//
// Without a lineage (single-node TOFU): only manifestSigDomain || manifestBytes, for backward compatibility.
// With a lineage: the whole lineage digest is bound in as well. Otherwise the developer only signs the manifest, and the lineage field
// is replaceable: a man in the middle could put any A_evil -> B lineage on a package genuinely signed by B, poisoning the root.
// The binding uses an ordered digest of every lineage node's key_id,
// each key_id followed by a NUL separator (key_id is "sha256:hex" and contains no NUL, so it's unambiguous)
func developerSignMessage(manifestBytes []byte, l *Lineage) []byte {
	msg := append(append([]byte{}, manifestSigDomain...), manifestBytes...)
	if l == nil {
		return msg
	}
	h := sha256.New()
	for _, n := range l.Nodes {
		h.Write([]byte(n.KeyID))
		h.Write([]byte{0})
	}
	msg = append(msg, lineageBindDomain...)
	msg = append(msg, h.Sum(nil)...)
	return msg
}

// decodePubKey decodes base64 and checks the length is exactly an ed25519 public key's
func decodePubKey(b64 string) (ed25519.PublicKey, error) {
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, fmt.Errorf("pkgregistry: decode pubkey: %w", err)
	}
	if len(raw) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("pkgregistry: pubkey has wrong length %d", len(raw))
	}
	return ed25519.PublicKey(raw), nil
}

// DevIdentity is the developer role's identity and lineage digest, for upgrade-time TOFU / identity hijack protection bookkeeping.
// Without a lineage it's a single node: Root==Current, Len==1
type DevIdentity struct {
	RootKeyID    string
	CurrentKeyID string
	LineageLen   int
	KeyIDs       []string // key_ids of every lineage node, in order; needed for the upgrade check "the first lineage_len are equal one by one"
}

// VerifiedSigner is one cryptographically verified signer identity. Roles are
// policy labels; KeyID is the identity needed for same-signer permission rules.
type VerifiedSigner struct {
	Role  SignerRole
	KeyID string
}

// SignerSet is the conclusion of one multi-role signature check
type SignerSet struct {
	// Trust is the highest trust these signatures alone can prove. Arbitrate also intersects it with the install
	// source, so a dynamic install can only be Ordinary even with a platform signature
	Trust        identity.TrustProfile
	HasDeveloper bool
	// HasOEM means there's a signature granting OEM trust (oem-service / oem-app).
	// Note oem-trust-software doesn't count here: it's only a countersignature and stays Ordinary (see HasOEMCountersign)
	HasOEM bool
	// HasPlatform means there's a signature granting Platform trust (platform-release / platform-systemapp)
	HasPlatform bool
	// HasOEMCountersign means there's any OEM role signature (oem-service / oem-app /
	// oem-trust-software), meeting the admission bar of the device policy require_oem_countersign.
	// Separate from HasOEM: oem-trust-software is the OEM's countersignature that a third-party package may be installed, meeting the countersignature
	// requirement without raising trust
	HasOEMCountersign bool
	// Roles are every signing role that appeared in this check (before de-duplication, in signature order). Used by the permission decision's
	// RequireSignerRole: some of the most dangerous permissions are only given to packages signed by particular roles
	Roles []SignerRole
	// VerifiedSigners preserves the verified key identity beside its role.
	// Comparing role strings is not a same-signature check.
	VerifiedSigners []VerifiedSigner
	// Dev is non-nil only when there's a developer role signature
	Dev *DevIdentity
}

// RoleStrings returns Roles as strings, for permission.Intersect's RequireSignerRole
// decision (permission doesn't import pkgregistry.SignerRole and only knows strings)
func (s SignerSet) RoleStrings() []string {
	out := make([]string, 0, len(s.Roles))
	for _, r := range s.Roles {
		out = append(out, string(r))
	}
	return out
}

// Policy is the device-level signing policy, from the trust bundle
type Policy struct {
	RequireOEMCountersign bool `json:"require_oem_countersign"`
}

// trustedKey is an authorised non-developer signing key in the trust bundle
type trustedKey struct {
	pub   ed25519.PublicKey
	roles map[SignerRole]struct{}
}

// TrustStore is the runtime view of the three-layer root of trust.
//
// The zero value isn't usable; build it with LoadTrustStore or (in tests) newTrustStore
type TrustStore struct {
	// byKeyID holds every authorised signing key in the bundle (platform intermediate keys + OEM keys),
	// each with the set of roles it's authorised for. The developer role isn't in this table: it signs itself, with the public key embedded in
	// manifest.sig and the key_id being the public key's sha256
	byKeyID map[string]trustedKey
	policy  Policy
}

func (ts TrustStore) policyRequireOEMCountersign() bool { return ts.policy.RequireOEMCountersign }

// ---- Trust bundle wire format ------------------------

const trustBundleFormatV1 = 1

// bundleKey is one authorised key in the bundle. roles declares which roles it may act in: a
// platform-release key can't be used to pose as oem-service, and vice versa (least authority)
type bundleKey struct {
	KeyID string       `json:"key_id"`
	Key   string       `json:"key"`
	Roles []SignerRole `json:"roles"`
}

// trustBundle is the content of /usr/share/nervus/trust/trust-bundle.json.
//
// v1 simplification (relative to the design's two-level OEM delegation): the "OEM root signs OEM sub-keys" level
// is folded away, and the bundle lists every authorised leaf signing key (the platform's and the OEMs') directly, each with its roles.
// The security properties don't change: who is authorised to sign is still decided only by the bundle the platform root signed, not by file write permissions.
// Two-level delegation is left for v2, when there's a real key management need
type trustBundle struct {
	Format       int         `json:"format"`
	PlatformKeys []bundleKey `json:"platform_keys"`
	OEMKeys      []bundleKey `json:"oem_keys"`
	Policy       Policy      `json:"policy"`
}

// embeddedPlatformRootB64 is the platform root public key compiled into the binary.
//
// Production builds inject it with -ldflags "-X...embeddedPlatformRootB64=<base64>", or by editing it here and
// recompiling. Empty = not injected (a development build): LoadTrustStore fails closed (refusing to load
// any bundle), so scanSystemImage can't get non-Ordinary trust. That's "fail closed when verification fails,
// never pretend it passed" in action, not an exploitable loose switch.
// It must be a package-level var (not a const): production builds inject it with -ldflags "-X...=<base64>",
// and -X can only change vars. gochecknoglobals flags this wrongly; it's the established shape of an ldflags injection point
//
//nolint:gochecknoglobals // ldflags -X injection target; cannot be const
var embeddedPlatformRootB64 = ""

// DefaultTrustDir is the trust material directory inside the read-only image
const DefaultTrustDir = "/usr/share/nervus/trust"

// LoadTrustStore verifies the trust bundle under dir with the embedded platform root and builds a TrustStore.
//
// Failing verification, a missing file or an empty embedded root -> an error; the caller (main.go) fails closed on that:
// no non-Ordinary trust can be issued at all, but the system can still install/run Ordinary packages
func LoadTrustStore(dir string) (TrustStore, error) {
	if embeddedPlatformRootB64 == "" {
		return TrustStore{}, fmt.Errorf("%w: no embedded platform root (dev build)", ErrTrustBundleInvalid)
	}
	root, err := decodePubKey(embeddedPlatformRootB64)
	if err != nil {
		return TrustStore{}, fmt.Errorf("pkgregistry: embedded platform root: %w", err)
	}
	bundleBytes, err := os.ReadFile(filepath.Join(dir, "trust-bundle.json"))
	if err != nil {
		return TrustStore{}, fmt.Errorf("pkgregistry: read trust bundle: %w", err)
	}
	sigBytes, err := os.ReadFile(filepath.Join(dir, "trust-bundle.sig"))
	if err != nil {
		return TrustStore{}, fmt.Errorf("pkgregistry: read trust bundle sig: %w", err)
	}
	return parseTrustBundle(root, bundleBytes, sigBytes)
}

// trustBundleSigDomain is the signature domain prefix covering the trust bundle's raw bytes. A const string like
// sigblock.go's three domain prefixes: a true constant that can't be rewritten at run time, which avoids gochecknoglobals
const trustBundleSigDomain = "nervus-trust-bundle-v1\x00"

// parseTrustBundle verifies the bundle's raw bytes with root, then parses it into a TrustStore.
//
// It's split out (rather than reading the file directly) so tests can run the full verification path with self-made key pairs,
// without putting a real embedded root in the test
func parseTrustBundle(root ed25519.PublicKey, bundleBytes, sigB64 []byte) (TrustStore, error) {
	sig, err := base64.StdEncoding.DecodeString(string(sigB64))
	if err != nil {
		return TrustStore{}, fmt.Errorf("pkgregistry: decode bundle sig: %w", err)
	}
	msg := append(append([]byte{}, trustBundleSigDomain...), bundleBytes...)
	if !ed25519.Verify(root, msg, sig) {
		return TrustStore{}, ErrTrustBundleInvalid
	}

	var tb trustBundle
	if err := json.Unmarshal(bundleBytes, &tb); err != nil {
		return TrustStore{}, fmt.Errorf("pkgregistry: decode trust bundle: %w", err)
	}
	if tb.Format != trustBundleFormatV1 {
		return TrustStore{}, fmt.Errorf("%w: format %d", ErrTrustBundleInvalid, tb.Format)
	}

	byID := make(map[string]trustedKey)
	add := func(bk bundleKey, allowed map[SignerRole]struct{}) error {
		pub, err := decodePubKey(bk.Key)
		if err != nil {
			return err
		}
		if keyIDOf(pub) != bk.KeyID {
			return fmt.Errorf("%w: bundle key %q", ErrKeyIDMismatch, bk.KeyID)
		}
		roles := make(map[SignerRole]struct{}, len(bk.Roles))
		for _, r := range bk.Roles {
			if _, ok := allowed[r]; !ok {
				return fmt.Errorf("%w: key %q claims disallowed role %q", ErrUntrustedSigner, bk.KeyID, r)
			}
			roles[r] = struct{}{}
		}
		byID[bk.KeyID] = trustedKey{pub: pub, roles: roles}
		return nil
	}
	platformRoles := map[SignerRole]struct{}{RolePlatformRelease: {}, RolePlatformSystemApp: {}}
	oemRoles := map[SignerRole]struct{}{RoleOEMService: {}, RoleOEMApp: {}, RoleOEMTrustSoftware: {}}
	for _, bk := range tb.PlatformKeys {
		if err := add(bk, platformRoles); err != nil {
			return TrustStore{}, err
		}
	}
	for _, bk := range tb.OEMKeys {
		if err := add(bk, oemRoles); err != nil {
			return TrustStore{}, err
		}
	}
	return TrustStore{byKeyID: byID, policy: tb.Policy}, nil
}

// VerifySignature checks manifestBytes and its detached signature sigBlock and returns the multi-role conclusion.
//
// manifestBytes must be the raw bytes the signatures cover. If any signature fails verification the whole thing is an error
// (ErrSignatureInvalid and so on). That's "verified but failed", which differs from "couldn't verify",
// and callers record it as a violation rather than a capability gap
func (ts TrustStore) VerifySignature(manifestBytes, sigBlock []byte) (SignerSet, error) {
	sb, err := ParseSignatureBlock(sigBlock)
	if err != nil {
		return SignerSet{}, err
	}

	// First check the lineage chain passes, to get the currently valid developer key
	var currentDevKeyID string
	var dev *DevIdentity
	if sb.Lineage != nil {
		id, derr := verifyLineage(sb.Lineage)
		if derr != nil {
			return SignerSet{}, derr
		}
		dev = id
		currentDevKeyID = id.CurrentKeyID
	}

	msg := append(append([]byte{}, manifestSigDomain...), manifestBytes...)
	// The bytes the developer signature covers also bind the lineage (see developerSignMessage); other roles
	// only cover the raw manifest bytes
	devMsg := developerSignMessage(manifestBytes, sb.Lineage)
	set := SignerSet{Trust: identity.TrustOrdinary}

	for _, s := range sb.Signatures {
		sig, derr := base64.StdEncoding.DecodeString(s.Sig)
		if derr != nil {
			return SignerSet{}, fmt.Errorf("pkgregistry: decode sig for %q: %w", s.KeyID, derr)
		}

		var pub ed25519.PublicKey
		verifyMsg := msg
		switch s.Role {
		case RoleDeveloper:
			pub, derr = decodePubKey(s.Key)
			if derr != nil {
				return SignerSet{}, derr
			}
			if keyIDOf(pub) != s.KeyID {
				return SignerSet{}, fmt.Errorf("%w: developer %q", ErrKeyIDMismatch, s.KeyID)
			}
			// With a lineage, the developer signature must be made by the last node of the lineage
			if sb.Lineage != nil && s.KeyID != currentDevKeyID {
				return SignerSet{}, fmt.Errorf("%w: sig key %q, current %q",
					ErrDeveloperKeyNotCurrent, s.KeyID, currentDevKeyID)
			}
			verifyMsg = devMsg // developer covers the manifest + the lineage binding
		default:
			tk, ok := ts.byKeyID[s.KeyID]
			if !ok {
				return SignerSet{}, fmt.Errorf("%w: %q", ErrUntrustedSigner, s.KeyID)
			}
			if _, ok := tk.roles[s.Role]; !ok {
				return SignerSet{}, fmt.Errorf("%w: %q not authorized for %q", ErrUntrustedSigner, s.KeyID, s.Role)
			}
			// The embedded public key (optional) must be byte for byte identical to the public key for that key_id in the trust store
			if s.Key != "" {
				embedded, eerr := decodePubKey(s.Key)
				if eerr != nil {
					return SignerSet{}, eerr
				}
				if !embedded.Equal(tk.pub) {
					return SignerSet{}, fmt.Errorf("%w: embedded key for %q differs from bundle", ErrKeyIDMismatch, s.KeyID)
				}
			}
			pub = tk.pub
		}

		if !ed25519.Verify(pub, verifyMsg, sig) {
			return SignerSet{}, fmt.Errorf("%w: %s by %q", ErrSignatureInvalid, s.Role, s.KeyID)
		}

		set.Roles = append(set.Roles, s.Role)
		set.VerifiedSigners = append(set.VerifiedSigners, VerifiedSigner{Role: s.Role, KeyID: s.KeyID})
		switch s.Role {
		case RoleDeveloper:
			set.HasDeveloper = true
			if dev == nil {
				// No lineage: a single-node identity, root==current==this key
				dev = &DevIdentity{RootKeyID: s.KeyID, CurrentKeyID: s.KeyID, LineageLen: 1, KeyIDs: []string{s.KeyID}}
			}
		case RolePlatformRelease, RolePlatformSystemApp:
			set.HasPlatform = true
		case RoleOEMService, RoleOEMApp:
			// Roles granting OEM trust
			set.HasOEM = true
			set.HasOEMCountersign = true
		case RoleOEMTrustSoftware:
			// Countersignature only (the OEM approves a third-party package for install): meets the countersignature bar but doesn't raise trust,
			// so HasOEM is deliberately not set
			set.HasOEMCountersign = true
		}
	}

	// A lineage without a single developer signature = an unanchored lineage, refused
	if sb.Lineage != nil && !set.HasDeveloper {
		return SignerSet{}, ErrNoDeveloperSignature
	}
	set.Dev = dev

	switch {
	case set.HasPlatform:
		set.Trust = identity.TrustPlatform
	case set.HasOEM:
		set.Trust = identity.TrustOEM
	default:
		set.Trust = identity.TrustOrdinary
	}
	return set, nil
}

// verifyLineage checks the lineage chain hop by hop and returns the identity digest.
//
// Each node's key must match its key_id; every non-first node's signed_by_prev must be the previous
// node's valid signature over (lineageSigDomain || key_id || key), i.e. the previous key authorised
// the next one to take over. The CurrentKeyID returned is the last node
func verifyLineage(l *Lineage) (*DevIdentity, error) {
	keyIDs := make([]string, len(l.Nodes))
	var prevPub ed25519.PublicKey
	for i, n := range l.Nodes {
		pub, err := decodePubKey(n.Key)
		if err != nil {
			return nil, fmt.Errorf("%w: node %d: %v", ErrLineageBroken, i, err)
		}
		if keyIDOf(pub) != n.KeyID {
			return nil, fmt.Errorf("%w: node %d key_id mismatch", ErrLineageBroken, i)
		}
		if i > 0 {
			sig, err := base64.StdEncoding.DecodeString(n.SignedByPrev)
			if err != nil {
				return nil, fmt.Errorf("%w: node %d signed_by_prev decode: %v", ErrLineageBroken, i, err)
			}
			// Message = domain prefix || this node's key_id || this node's raw public key bytes
			msg := append(append(append([]byte{}, lineageSigDomain...), []byte(n.KeyID)...), pub...)
			if !ed25519.Verify(prevPub, msg, sig) {
				return nil, fmt.Errorf("%w: node %d signed_by_prev invalid", ErrLineageBroken, i)
			}
		}
		keyIDs[i] = n.KeyID
		prevPub = pub
	}
	return &DevIdentity{
		RootKeyID:    keyIDs[0],
		CurrentKeyID: keyIDs[len(keyIDs)-1],
		LineageLen:   len(keyIDs),
		KeyIDs:       keyIDs,
	}, nil
}
