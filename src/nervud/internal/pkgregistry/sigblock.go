// This file is the wire format of manifest.sig: the data model and structural validation of multi-role parallel signatures
// and key lineage. It only parses and checks structure, with no
// cryptographic verification; the real signature checks, root-of-trust resolution and lineage chain checks are in signature.go
package pkgregistry

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

const (
	// sigBlockFormatV1 is the only top-level manifest.sig format number this version knows
	sigBlockFormatV1 = 1
	// lineageFormatV1 is the only lineage format number this version knows
	lineageFormatV1 = 1

	// maxSignatures is the limit on signature entries in one manifest.sig
	maxSignatures = 8
	// maxLineageNodes is the limit on nodes in one lineage chain, so overlong chains can't burn the CPU
	maxLineageNodes = 16
)

// Signature domain separation prefixes: preventing cross-protocol signature reuse
//
//   - manifestSigDomain covers the raw manifest.json bytes
//   - lineageSigDomain covers the key_id||key of the next node authorised to take over
//   - lineageBindDomain binds the whole lineage into the developer signature (see developerSignMessage):
//     otherwise the developer only signs the raw manifest bytes and the lineage is replaceable; a man in the middle could put
//     any A_evil -> B lineage on a package genuinely signed by B, poisoning the root with their own key, after which legitimate
//     B upgrades would be refused because the root differs, a persistent upgrade DoS / identity hijack
//
// These three constants are protocol sources of truth that signer and verifier must match byte for byte; changing them breaks compatibility.
// const string (not var []byte): a true constant that can't be rewritten at run time, which also avoids
// gochecknoglobals. When used as append([]byte{}, domain...), Go allows appending a string into a []byte
const (
	manifestSigDomain = "nervus-pkg-manifest-v1\x00"
	lineageSigDomain  = "nervus-lineage-v1\x00"
	lineageBindDomain = "nervus-lineage-bind-v1\x00"
)

// SignerRole is the signer's role, deciding the highest trust it can prove.
//
// Roles are divided by blast radius rather than component type: whether a component is an app or a service is expressed by the manifest's
// component.type, and expressing it again with keys would make two sources of truth
type SignerRole string

const (
	RoleDeveloper         SignerRole = "developer"          // developer self-signed
	RolePlatformRelease   SignerRole = "platform-release"   // platform: nervud + core system services
	RolePlatformSystemApp SignerRole = "platform-systemapp" // platform: system software
	RoleOEMService        SignerRole = "oem-service"        // OEM: hardware Provider Services
	RoleOEMApp            SignerRole = "oem-app"            // OEM: system software
	// RoleOEMTrustSoftware: the OEM endorses (countersigns) ordinary software/services; it still only issues Ordinary trust,
	// and means "the OEM approves this third-party package being installed on my device"
	RoleOEMTrustSoftware SignerRole = "oem-trust-software"
)

func (r SignerRole) valid() bool {
	switch r {
	case RoleDeveloper, RolePlatformRelease, RolePlatformSystemApp,
		RoleOEMService, RoleOEMApp, RoleOEMTrustSoftware:
		return true
	default:
		return false
	}
}

// SigAlg is the signature algorithm. v1 only allows ed25519; the field is kept for future algorithm agility,
// and unknown values are always refused, with no best-effort compatibility
type SigAlg string

const SigAlgEd25519 SigAlg = "ed25519"

var (
	// ErrSigBlockMalformed: manifest.sig has an illegal structure (format number, empty fields, too many entries and so on)
	ErrSigBlockMalformed = errors.New("pkgregistry: malformed signature block")
	// ErrUnknownSigAlg: a signature entry uses an unknown algorithm
	ErrUnknownSigAlg = errors.New("pkgregistry: unknown signature algorithm")
	// ErrUnknownSignerRole: a signature entry uses an unknown role
	ErrUnknownSignerRole = errors.New("pkgregistry: unknown signer role")
	// ErrDuplicateKeyID: the same key_id appears twice in one manifest.sig
	ErrDuplicateKeyID = errors.New("pkgregistry: duplicate key_id in signature block")
	// ErrLineageMalformed: the lineage has an illegal structure
	ErrLineageMalformed = errors.New("pkgregistry: malformed lineage")
)

// Signature is one signature in manifest.sig
type Signature struct {
	Role  SignerRole `json:"role"`
	Alg   SigAlg     `json:"alg"`
	KeyID string     `json:"key_id"`
	// Key is the base64 raw public key. The developer role must embed it (self-signed); other roles may,
	// and if embedded it must be byte for byte identical to the public key for that key_id in the trust store (checked in signature.go)
	Key string `json:"key,omitempty"`
	Sig string `json:"sig"`
}

// LineageNode is one node of a lineage chain
type LineageNode struct {
	KeyID string `json:"key_id"`
	Key   string `json:"key"`
	// SignedByPrev = signed with nodes[i-1]'s private key over (lineageSigDomain || key_id || key).
	// The first node doesn't have it; every other node must
	SignedByPrev string `json:"signed_by_prev,omitempty"`
}

// Lineage is the key lineage chain: nodes[0] is the root, and the last node is the currently valid signing key
type Lineage struct {
	Format int           `json:"format"`
	Nodes  []LineageNode `json:"nodes"`
}

// SignatureBlock is the parsed result of manifest.sig
type SignatureBlock struct {
	Format     int         `json:"format"`
	Signatures []Signature `json:"signatures"`
	Lineage    *Lineage    `json:"lineage,omitempty"`
}

// ParseSignatureBlock deserialises manifest.sig and does structural validation.
//
// It only checks the shape is legal: the format number, empty fields, the entry limit, key_id de-duplication, recognisable algorithm/role,
// the lineage node limit and the first node having no signed_by_prev. Cryptographic verification is in signature.go
func ParseSignatureBlock(data []byte) (SignatureBlock, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()

	var sb SignatureBlock
	if err := dec.Decode(&sb); err != nil {
		return SignatureBlock{}, fmt.Errorf("pkgregistry: decode signature block: %w", err)
	}
	if err := sb.validate(); err != nil {
		return SignatureBlock{}, err
	}
	return sb, nil
}

func (sb SignatureBlock) validate() error {
	if sb.Format != sigBlockFormatV1 {
		return fmt.Errorf("%w: format %d", ErrSigBlockMalformed, sb.Format)
	}
	if len(sb.Signatures) == 0 {
		return fmt.Errorf("%w: no signatures", ErrSigBlockMalformed)
	}
	if len(sb.Signatures) > maxSignatures {
		return fmt.Errorf("%w: %d signatures exceed cap %d", ErrSigBlockMalformed, len(sb.Signatures), maxSignatures)
	}

	seenKeyID := make(map[string]struct{}, len(sb.Signatures))
	for _, s := range sb.Signatures {
		if !s.Role.valid() {
			return fmt.Errorf("%w: %q", ErrUnknownSignerRole, s.Role)
		}
		if s.Alg != SigAlgEd25519 {
			return fmt.Errorf("%w: %q", ErrUnknownSigAlg, s.Alg)
		}
		if s.KeyID == "" || s.Sig == "" {
			return fmt.Errorf("%w: signature with empty key_id or sig", ErrSigBlockMalformed)
		}
		if s.Role == RoleDeveloper && s.Key == "" {
			return fmt.Errorf("%w: developer signature must embed its public key", ErrSigBlockMalformed)
		}
		if _, dup := seenKeyID[s.KeyID]; dup {
			return fmt.Errorf("%w: %q", ErrDuplicateKeyID, s.KeyID)
		}
		seenKeyID[s.KeyID] = struct{}{}
	}

	if sb.Lineage != nil {
		if err := sb.Lineage.validate(); err != nil {
			return err
		}
	}
	return nil
}

func (l Lineage) validate() error {
	if l.Format != lineageFormatV1 {
		return fmt.Errorf("%w: format %d", ErrLineageMalformed, l.Format)
	}
	if len(l.Nodes) == 0 {
		return fmt.Errorf("%w: no nodes", ErrLineageMalformed)
	}
	if len(l.Nodes) > maxLineageNodes {
		return fmt.Errorf("%w: %d nodes exceed cap %d", ErrLineageMalformed, len(l.Nodes), maxLineageNodes)
	}
	for i, n := range l.Nodes {
		if n.KeyID == "" || n.Key == "" {
			return fmt.Errorf("%w: node %d has empty key_id or key", ErrLineageMalformed, i)
		}
		if i == 0 && n.SignedByPrev != "" {
			return fmt.Errorf("%w: root node must not carry signed_by_prev", ErrLineageMalformed)
		}
		if i > 0 && n.SignedByPrev == "" {
			return fmt.Errorf("%w: node %d missing signed_by_prev", ErrLineageMalformed, i)
		}
	}
	return nil
}
