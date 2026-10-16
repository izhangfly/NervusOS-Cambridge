// This file only serves development builds: on a binary without an embedded platform root, it lets system image packages still get
// the trust their signatures claim.
//
// # Why it's needed
//
// Production binaries inject embeddedPlatformRootB64 with -ldflags, and LoadTrustStore uses it to verify the
// trust bundle. Development builds don't inject it, LoadTrustStore fails outright, and scanSystemImage
// fails every system package closed to Ordinary. But perm.service.register's bar is
// MinimumTrust=OEM + GRANT_MODE_PRIVILEGED, so on a development machine no system service exporting a public interface
// could register, and even pkgmanagerd's compatibility bridge didn't hold because it required TrustPlatform.
//
// # What it relaxes and what it doesn't
//
// It relaxes only one thing: whether this signing key was authorised by the embedded platform root.
//
// Every signature is still really verified with Ed25519, key_id must still equal the public key's sha256,
// roles must still be known roles, and digests are still a hard check. In other words:
//
//   - change the binary -> the digest doesn't match, still refused
//   - change the manifest -> the signature fails, still refused
//   - try to pose as a key_id without the private key -> you can't sign, still refused
//
// So key_ids are real, and the same-signer comparison of GRANT_MODE_SIGNATURE means the same in development and production.
// That's deliberate: if this invented a constant key_id, every development package would become
// "the same signer", and the difference would only surface once some feature relying on SameIdentity shipped.
//
// Where the public key comes from: sysmanifest embeds the public key for non-developer roles too (signManifest's
// Key field), with a comment saying "including it lets development (with no trust store) check itself". This file is what
// makes good on that.
package pkgregistry

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
)

// LoadDevTrustStore builds a development TrustStore from the public keys embedded in system image packages.
//
// It only scans manifest.sig under systemPackagesDir. Dynamically installed packages take no part, and needn't:
// Arbitrate(SourceDynamicInstall,...) only ever issues Ordinary whatever the signature,
// so this store gives the dynamic install path no extra power at all.
//
// The returned store's policy is the zero value (RequireOEMCountersign=false). Development doesn't simulate
// device-level countersignature policy: that belongs to the trust bundle, and there's no bundle here at all.
func LoadDevTrustStore(systemPackagesDir string, log *slog.Logger) (TrustStore, error) {
	matches, err := filepath.Glob(filepath.Join(systemPackagesDir, "*", SignatureFileName))
	if err != nil {
		return TrustStore{}, fmt.Errorf("pkgregistry: scan dev signatures: %w", err)
	}

	byKeyID := make(map[string]trustedKey)
	for _, sigPath := range matches {
		sigBytes, rerr := os.ReadFile(sigPath)
		if rerr != nil {
			continue
		}
		sb, perr := ParseSignatureBlock(sigBytes)
		if perr != nil {
			// A bad signature block here only means "can't anchor a key". The real refusal happens at
			// scanSystemImage's signature check, where the error message is closer to the cause too
			continue
		}
		for _, s := range sb.Signatures {
			// The developer role anchors itself: its public key is embedded in the signature block, and verifiers never consult the trust store for it.
			// Putting it in the store would be redundant, and would blur the line of "who needs to be authorised"
			if s.Role == RoleDeveloper || s.Key == "" {
				continue
			}
			pub, derr := decodePubKey(s.Key)
			if derr != nil {
				continue
			}
			// key_id must really be this public key's sha256. Without checking, a package could use
			// its own key to occupy another key_id's authorisation slot
			if keyIDOf(pub) != s.KeyID {
				continue
			}
			tk, ok := byKeyID[s.KeyID]
			if !ok {
				tk = trustedKey{pub: pub, roles: make(map[SignerRole]struct{})}
			}
			tk.roles[s.Role] = struct{}{}
			byKeyID[s.KeyID] = tk
		}
	}

	if len(byKeyID) == 0 {
		return TrustStore{}, fmt.Errorf(
			"%w: no embedded signer keys found under %s", ErrTrustBundleInvalid, systemPackagesDir)
	}
	if log != nil {
		for keyID, tk := range byKeyID {
			roles := make([]string, 0, len(tk.roles))
			for role := range tk.roles {
				roles = append(roles, string(role))
			}
			log.Warn("pkgregistry: DEV trust anchor accepted without platform root",
				"key_id", keyID, "roles", roles)
		}
	}
	return TrustStore{byKeyID: byKeyID}, nil
}
