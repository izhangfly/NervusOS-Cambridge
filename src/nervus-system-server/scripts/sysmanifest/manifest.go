// Package main is sysmanifest: it completes an already-built system package directory tree into the shape nervud can
// load: compute digests, fill in manifest.json, produce the Ed25519-signed manifest.sig.
//
// # Why Go instead of reusing nervus-packaging's Kotlin signing-lib
//
// This repository is pure Go, and manifest.sig is just JSON plus an Ed25519 signature, which the standard library's
// crypto/ed25519 produces directly. Dragging a JVM toolchain into the build for signing isn't worth it.
//
// The cost is that the struct definitions here and in the kernel's pkgregistry are **two copies** that can drift. The mitigation is
// the golden test in manifest_test.go: any field change makes it fail, forcing people to look at both sides.
//
// # It doesn't produce .nspkg
//
// System packages are **unpacked directory trees** in the image (the kernel's scan.go globs */manifest.json),
// not zstd+tar .nspkg files. The latter is the format for dynamic install, and the two paths must not be mixed.
package main

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// manifestSchemaV1 must match the kernel's pkgregistry.manifestSchemaV1.
// The kernel uses it in the first of its two parsing passes to stop schemas it doesn't know.
const manifestSchemaV1 = 1

// manifestSigDomain is the signature's domain separation prefix and must match the kernel's sigblock.go byte for byte.
//
// Why domain separation: the same private key may sign other things elsewhere (lineage nodes, the trust bundle),
// and without a prefix a signature from one context could be moved to another and misused.
const manifestSigDomain = "nervus-pkg-manifest-v1\x00"

// rolePlatformRelease is the signing role of core system services.
// The kernel's sigblock.go says: "platform: nervud + core system services".
const rolePlatformRelease = "platform-release"

// Component corresponds to the kernel's pkgregistry.Component.
//
// Field names and json tags must match exactly: the kernel parses with DisallowUnknownFields,
// so one extra field or a misspelt tag means a vague decode failure at load time.
type Component struct {
	ID           string          `json:"id"`
	Type         string          `json:"type"`    // app | service
	Runtime      string          `json:"runtime"` // native | jvm
	Entry        string          `json:"entry"`
	NativeLibDir string          `json:"native_lib_dir,omitempty"`
	LaunchMode   string          `json:"launch_mode"` // always-on | on-demand | manual
	Criticality  string          `json:"criticality,omitempty"`
	Disableable  bool            `json:"disableable,omitempty"`
	Exports      []Export        `json:"exports,omitempty"`
	Interfaces   []string        `json:"interfaces,omitempty"`
	IdleTimeout  int             `json:"idle_timeout_sec,omitempty"`
	Limits       ComponentLimits `json:"limits,omitempty"`

	// Privileged / Capabilities / AddressFamilies are the Linux-level privileges a component requests.
	//
	// This tool [passes them through without judging]: whether they're valid is decided by the kernel's allowlist (pkgregistry/
	// privilege.go), and whether they're actually granted by the kernel based on package source (only system image packages get them).
	// The fields are here only so strict JSON decoding recognises them; without them, a manifest that fills in
	// privileged fails at packaging with "unknown field",
	// and that error looks like a mistake in the template.
	Privileged      bool     `json:"privileged,omitempty"`
	Capabilities    []string `json:"capabilities,omitempty"`
	AddressFamilies []string `json:"address_families,omitempty"`
}

// Export declares an interface this component provides.
type Export struct {
	Interface  string `json:"interface"`
	Visibility string `json:"visibility,omitempty"`
}

// ProviderArtifactsRef points at the data-driven Provider contract inside the package; it corresponds to the kernel's
// pkgregistry.ProviderArtifactsRef.
//
// Descriptor is the deterministic protobuf bytes of a ProviderDescriptor, Schemas the deterministic protobuf bytes of an
// InterfaceSchemaBundleSet. Both files are produced by the service's own
// providergen (see <service dir>/providergen/); this tool only includes them in the
// digests and writes them into the manifest.
//
// [Packages that export interfaces must have it]: for "exports without provider" the kernel's
// loadRequiredProviderArtifacts returns ErrProviderArtifactsRequired.
// The only exception was the compatibility bridge for nervus.pkgmanagerd, and that bridge is being removed.
type ProviderArtifactsRef struct {
	Descriptor string `json:"descriptor"`
	Schemas    string `json:"schemas"`
}

// ComponentLimits are a component's resource limits, translated to systemd's
// MemoryMax / CPUQuotaPerSecUSec / TasksMax. A zero value means that item isn't set.
type ComponentLimits struct {
	MemoryMaxMB     uint64 `json:"memory_max_mb,omitempty"`
	CPUQuotaPercent uint32 `json:"cpu_quota_percent,omitempty"`
	TasksMax        uint32 `json:"tasks_max,omitempty"`
}

// Manifest corresponds to the kernel's pkgregistry.Manifest.
type Manifest struct {
	Schema          int               `json:"schema"`
	PackageID       string            `json:"package_id"`
	Label           string            `json:"label"`
	Labels          map[string]string `json:"labels,omitempty"`
	Icon            string            `json:"icon,omitempty"`
	Version         string            `json:"version"`
	VersionCode     uint64            `json:"version_code"`
	MinNervusAPI    uint32            `json:"min_nervus_api"`
	TargetNervusAPI uint32            `json:"target_nervus_api"`
	SupportedABIs   []string          `json:"supported_abis"`
	Permissions     []string          `json:"permissions,omitempty"`
	Components      []Component       `json:"components"`
	// Provider's json tag must match the kernel pkgregistry.Manifest's `provider,omitempty`
	// exactly. The kernel parses with DisallowUnknownFields, and one wrong letter means a
	// vague decode failure
	Provider *ProviderArtifactsRef `json:"provider,omitempty"`
	Digests  map[string]string     `json:"digests"`
}

// Signature and SignatureBlock correspond to the kernel pkgregistry types of the same names.
type Signature struct {
	Role  string `json:"role"`
	Alg   string `json:"alg"`
	KeyID string `json:"key_id"`
	Key   string `json:"key,omitempty"`
	Sig   string `json:"sig"`
}

// SignatureBlock is the content of manifest.sig.
//
// No lineage: system packages ship with the image, and signing key rotation follows whole-image OTA, so there's no need for
// a per-package lineage chain. Lineage is for dynamically installed third-party packages, for "the same developer changed keys",
// a case that doesn't exist here.
type SignatureBlock struct {
	Format     int         `json:"format"`
	Signatures []Signature `json:"signatures"`
}

// keyIDOf computes a public key's key_id, in the same format as the kernel's keyIDOf: "sha256:" + hex.
func keyIDOf(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// computeDigests walks every regular file under root and produces relative path -> sha256 hex.
//
// manifest.json and manifest.sig are deliberately excluded: they can't hash themselves (writing the hash in changes
// their own hash). The kernel's VerifyDigests exempts the same two files, but that exemption left a gap of "verify A,
// write B", which the kernel closes in install.go with verifyStagingMetadata's byte-for-byte comparison.
//
// Symlinks are refused: digests must cover real content. Whether the digest of a symlink pointing at /etc/shadow
// is "the target's content" or "the link itself" depends on implementation details,
// an ambiguity that has no place in an integrity check.
func computeDigests(root string) (map[string]string, error) {
	digests := make(map[string]string)

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)

		if rel == manifestFileName || rel == signatureFileName {
			return nil
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("%s: only regular files are allowed, found %s (symlinks, devices and the like are always refused)", rel, d.Type())
		}

		sum, err := sha256File(path)
		if err != nil {
			return fmt.Errorf("%s: %w", rel, err)
		}
		digests[rel] = sum
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(digests) == 0 {
		// The kernel's validate refuses with ErrNoDigests; give a clearer message here first
		return nil, fmt.Errorf("there are no payload files under %s", root)
	}
	return digests, nil
}

func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

const (
	manifestFileName  = "manifest.json"
	signatureFileName = "manifest.sig"
)

// encodeManifest serialises the manifest.
//
// [These bytes are exactly what's signed], so they must be stable: MarshalIndent fixes the indentation,
// and encoding/json guarantees map keys come out sorted. Any re-serialisation must give
// the same bytes, or the signature won't verify.
//
// What's written to disk is these same bytes, not the result of "marshalling once more": that's exactly the "verify A, write B"
// the kernel's verifyStagingMetadata guards against.
func encodeManifest(m Manifest) ([]byte, error) {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode manifest: %w", err)
	}
	return append(b, '\n'), nil
}

// signManifest produces a signature block for the platform-release role over manifestBytes with priv.
func signManifest(manifestBytes []byte, priv ed25519.PrivateKey) SignatureBlock {
	pub := priv.Public().(ed25519.PublicKey)

	msg := append([]byte(manifestSigDomain), manifestBytes...)
	sig := ed25519.Sign(priv, msg)

	return SignatureBlock{
		Format: 1,
		Signatures: []Signature{{
			Role:  rolePlatformRelease,
			Alg:   "ed25519",
			KeyID: keyIDOf(pub),
			// Embed the public key: the kernel allows embedding for non-developer roles and compares it byte for byte with the public key
			// the trust store has for that key_id. Including it lets development (with no trust store) check itself,
			// and adds one more consistency check in production.
			Key: b64(pub),
			Sig: b64(sig),
		}},
	}
}

// validateManifest runs checks equivalent to the kernel's validate.
//
// Why repeat them here: the kernel finds problems [at load time], with just one decode/validate failure
// in the log, when the image has already been flashed. Stopping it at build time keeps the error closest to its cause.
func validateManifest(m Manifest) error {
	if m.Schema != manifestSchemaV1 {
		return fmt.Errorf("schema must be %d", manifestSchemaV1)
	}
	if m.PackageID == "" {
		return fmt.Errorf("package_id can't be empty")
	}
	if m.Version == "" {
		return fmt.Errorf("version can't be empty")
	}
	if m.VersionCode == 0 {
		// Upgrade decisions only look at version_code; without it, downgrade protection can't decide
		return fmt.Errorf("version_code can't be 0 (upgrade downgrade-protection depends on it)")
	}
	if len(m.SupportedABIs) == 0 {
		return fmt.Errorf("supported_abis can't be empty")
	}
	for _, abi := range m.SupportedABIs {
		switch abi {
		case "linux-arm64", "linux-armv7", "linux-x86_64":
		default:
			// The kernel only accepts these three canonical tokens, not Android NDK names (arm64-v8a)
			// or bare CPU names (aarch64), and doesn't normalise them
			return fmt.Errorf("supported_abis has an invalid value %q (only linux-arm64 / linux-armv7 / linux-x86_64)", abi)
		}
	}
	if len(m.Components) == 0 {
		return fmt.Errorf("at least one component must be declared")
	}

	seen := make(map[string]struct{}, len(m.Components))
	for _, c := range m.Components {
		if c.ID == "" {
			return fmt.Errorf("component id can't be empty")
		}
		if _, dup := seen[c.ID]; dup {
			return fmt.Errorf("component id %q is duplicated", c.ID)
		}
		seen[c.ID] = struct{}{}

		if c.Type != "app" && c.Type != "service" {
			return fmt.Errorf("component %q: type must be app or service", c.ID)
		}
		if c.Runtime != "native" && c.Runtime != "jvm" {
			return fmt.Errorf("component %q: runtime must be native or jvm", c.ID)
		}
		switch c.LaunchMode {
		case "always-on", "on-demand", "manual":
		default:
			return fmt.Errorf("component %q: invalid launch_mode %q", c.ID, c.LaunchMode)
		}
		// The kernel has the same hard check (ErrLaunchModeTypeMismatch)
		if c.Type == "app" && c.LaunchMode == "always-on" {
			return fmt.Errorf("component %q: an app can't be always-on", c.ID)
		}
		if c.Type == "service" && c.LaunchMode == "manual" {
			return fmt.Errorf("component %q: a service can't be manual", c.ID)
		}
		if c.Entry == "" {
			return fmt.Errorf("component %q: entry can't be empty", c.ID)
		}
		if strings.HasPrefix(c.Entry, "/") || strings.Contains(c.Entry, "..") {
			return fmt.Errorf("component %q: entry must be a relative path inside the package without ..", c.ID)
		}
		// The entry must be covered by the integrity check, or it would allow running an unverified executable
		if _, ok := m.Digests[c.Entry]; !ok {
			return fmt.Errorf("component %q: entry %q isn't covered by digests", c.ID, c.Entry)
		}
	}

	return validateProvider(m)
}

// validateProvider re-checks the shape of the Provider contract, matching the kernel pkgregistry's two checks:
// the path/digest checks in manifest.go, and "exports means there must be a provider" in
// provider_artifacts.go.
//
// Stopping it here matters for the same reason as the other checks in this file: the kernel finds the same problem at load time,
// after the image is flashed, and its error (ErrProviderArtifactsRequired) is far from the real cause,
// "the template is missing a section".
func validateProvider(m Manifest) error {
	if m.Provider == nil {
		for _, c := range m.Components {
			if len(c.Exports) != 0 {
				return fmt.Errorf(
					"component %q exports interfaces but the manifest has no provider section"+
						" (the kernel would refuse to load it with ErrProviderArtifactsRequired)", c.ID)
			}
		}
		return nil
	}

	for _, ref := range []struct {
		label string
		rel   string
	}{
		{"provider.descriptor", m.Provider.Descriptor},
		{"provider.schemas", m.Provider.Schemas},
	} {
		if ref.rel == "" {
			return fmt.Errorf("%s can't be empty", ref.label)
		}
		if strings.HasPrefix(ref.rel, "/") || strings.Contains(ref.rel, "..") {
			return fmt.Errorf("%s %q must be a relative path inside the package without ..", ref.label, ref.rel)
		}
		// As with a component's entry: the Provider contract decides what goes into the kernel's Catalog,
		// and not being covered by digests would let an unverified contract into the kernel
		if _, ok := m.Digests[ref.rel]; !ok {
			return fmt.Errorf("%s %q isn't covered by digests", ref.label, ref.rel)
		}
	}
	if m.Provider.Descriptor == m.Provider.Schemas {
		return fmt.Errorf("provider.descriptor and provider.schemas can't be the same file")
	}
	return nil
}

// sortedKeys is only for stable ordering in diagnostic output.
func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
