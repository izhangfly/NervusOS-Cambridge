package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newPkgDir builds a minimal system package directory tree.
func newPkgDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "bin"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "bin", "svc"), []byte("#!/bin/true\n"), 0o755); err != nil {
		t.Fatalf("write payload: %v", err)
	}
	return dir
}

func minimalManifest() Manifest {
	return Manifest{
		Schema:          manifestSchemaV1,
		PackageID:       "nervus.example",
		Label:           "Example",
		Version:         "1.0.0",
		VersionCode:     1,
		MinNervusAPI:    1,
		TargetNervusAPI: 1,
		SupportedABIs:   []string{"linux-arm64"},
		Components: []Component{{
			ID:         "main",
			Type:       "service",
			Runtime:    "native",
			Entry:      "bin/svc",
			LaunchMode: "always-on",
		}},
	}
}

func TestComputeDigests_ExcludesSelfDescribingFiles(t *testing.T) {
	dir := newPkgDir(t)
	// manifest.json and manifest.sig can't hash themselves: writing them into digests changes their own hash.
	// The kernel's VerifyDigests exempts the same two files.
	for _, name := range []string{manifestFileName, signatureFileName} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("{}"), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	d, err := computeDigests(dir)
	if err != nil {
		t.Fatalf("computeDigests: %v", err)
	}
	if _, ok := d[manifestFileName]; ok {
		t.Error("manifest.json shouldn't appear in digests")
	}
	if _, ok := d[signatureFileName]; ok {
		t.Error("manifest.sig shouldn't appear in digests")
	}
	if _, ok := d["bin/svc"]; !ok {
		t.Error("payload files should be covered")
	}
}

func TestComputeDigests_MatchesSHA256(t *testing.T) {
	dir := newPkgDir(t)
	d, err := computeDigests(dir)
	if err != nil {
		t.Fatalf("computeDigests: %v", err)
	}
	want := sha256.Sum256([]byte("#!/bin/true\n"))
	if got := d["bin/svc"]; got != hex.EncodeToString(want[:]) {
		t.Fatalf("digest = %s, want %s", got, hex.EncodeToString(want[:]))
	}
}

func TestComputeDigests_UsesForwardSlashes(t *testing.T) {
	// The kernel joins these keys directly into paths inside the package. Building on Windows, filepath.Rel gives
	// backslashes, and without normalising we'd produce a digest list that never matches on Linux.
	dir := newPkgDir(t)
	d, err := computeDigests(dir)
	if err != nil {
		t.Fatalf("computeDigests: %v", err)
	}
	for k := range d {
		if strings.Contains(k, "\\") {
			t.Fatalf("digest key %q contains a backslash; it must be normalised to /", k)
		}
	}
}

func TestComputeDigests_RejectsSymlink(t *testing.T) {
	// Digests must cover real content. Whether a symlink's digest is "the target's content"
	// or "the link itself" depends on implementation details, an ambiguity that has no place in an integrity check.
	dir := newPkgDir(t)
	target := filepath.Join(dir, "bin", "svc")
	link := filepath.Join(dir, "bin", "link")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("this platform doesn't support symlinks: %v", err)
	}
	if _, err := computeDigests(dir); err == nil {
		t.Fatal("symlinks must be refused")
	}
}

func TestEncodeManifest_IsStable(t *testing.T) {
	// These bytes [are what's signed]. Two encodings must be identical, or after re-serialising
	// the signature won't verify.
	m := minimalManifest()
	m.Digests = map[string]string{"b": "2", "a": "1", "c": "3"}

	first, err := encodeManifest(m)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	for i := 0; i < 5; i++ {
		again, err := encodeManifest(m)
		if err != nil {
			t.Fatalf("encode #%d: %v", i, err)
		}
		if string(again) != string(first) {
			t.Fatal("encoding twice produced different bytes; the signature couldn't be reproduced")
		}
	}
	// Maps must come out in sorted order (encoding/json's guarantee), otherwise "stable" is just luck
	if !strings.Contains(string(first), `"a": "1"`) ||
		strings.Index(string(first), `"a"`) > strings.Index(string(first), `"b"`) {
		t.Fatalf("digests aren't in sorted order:\n%s", first)
	}
}

func TestSignManifest_VerifiesWithKernelAlgorithm(t *testing.T) {
	// Reproduces the kernel's signature.go verification path in full:
	//   msg = manifestSigDomain || manifestBytes
	// one wrong byte in the domain separation prefix and the kernel can't verify, and the error only says
	// "invalid signature", giving no hint that the domain separation is the problem.
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("gen key: %v", err)
	}
	manifestBytes := []byte(`{"schema":1,"package_id":"nervus.example"}`)

	sb := signManifest(manifestBytes, priv)
	if len(sb.Signatures) != 1 {
		t.Fatalf("signature count = %d, want 1", len(sb.Signatures))
	}
	s := sb.Signatures[0]

	if s.Role != rolePlatformRelease {
		t.Errorf("role = %q, want %q", s.Role, rolePlatformRelease)
	}
	if s.Alg != "ed25519" {
		t.Errorf("alg = %q, want ed25519", s.Alg)
	}

	sig, err := base64.StdEncoding.DecodeString(s.Sig)
	if err != nil {
		t.Fatalf("sig isn't valid base64: %v", err)
	}
	msg := append([]byte(manifestSigDomain), manifestBytes...)
	if !ed25519.Verify(pub, msg, sig) {
		t.Fatal("verification with the kernel's algorithm failed")
	}

	// Without the domain separation prefix it must fail, or the prefix means nothing
	if ed25519.Verify(pub, manifestBytes, sig) {
		t.Fatal("the bare manifest bytes verified too; domain separation isn't working")
	}
}

func TestSignManifest_EmbeddedKeyMatchesKeyID(t *testing.T) {
	// The kernel compares the embedded public key byte for byte with the trust store's key for the key_id.
	// This asserts that the two we produce agree with each other.
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	sb := signManifest([]byte("{}"), priv)
	s := sb.Signatures[0]

	gotKey, err := base64.StdEncoding.DecodeString(s.Key)
	if err != nil {
		t.Fatalf("key isn't valid base64: %v", err)
	}
	if string(gotKey) != string(pub) {
		t.Error("the embedded public key doesn't match the private key's public key")
	}
	if s.KeyID != keyIDOf(pub) {
		t.Errorf("key_id = %q, want %q", s.KeyID, keyIDOf(pub))
	}
	if !strings.HasPrefix(s.KeyID, "sha256:") {
		t.Errorf("key_id must have the sha256: prefix, got %q", s.KeyID)
	}
}

func TestSignatureBlock_JSONShapeMatchesKernel(t *testing.T) {
	// The kernel parses manifest.sig with DisallowUnknownFields. One extra field means a load failure,
	// and the error only says "unknown field", not which side should change.
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	b, err := json.Marshal(signManifest([]byte("{}"), priv))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var generic map[string]any
	if err := json.Unmarshal(b, &generic); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for k := range generic {
		switch k {
		case "format", "signatures", "lineage":
		default:
			t.Errorf("the top level has a field the kernel doesn't know: %q", k)
		}
	}
}

func TestValidateManifest_AcceptsProviderArtifacts(t *testing.T) {
	m := minimalManifest()
	m.Components[0].Exports = []Export{{
		Interface: "nervus.interface.example", Visibility: "public",
	}}
	m.Provider = &ProviderArtifactsRef{
		Descriptor: "provider.binpb",
		Schemas:    "schemas.binpb",
	}
	m.Digests = map[string]string{
		"bin/svc":        "deadbeef",
		"provider.binpb": "cafe",
		"schemas.binpb":  "f00d",
	}
	if err := validateManifest(m); err != nil {
		t.Fatalf("a valid provider section was refused: %v", err)
	}
}

func TestManifest_ProviderJSONShapeMatchesKernel(t *testing.T) {
	// The kernel parses manifest.json with DisallowUnknownFields. The provider section's field names and
	// omitempty behaviour must match pkgregistry.ProviderArtifactsRef exactly.
	m := minimalManifest()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(b), "provider") {
		t.Error("a nil Provider shouldn't appear in the JSON (the kernel relies on that to tell \"no provider\")")
	}

	m.Provider = &ProviderArtifactsRef{Descriptor: "provider.binpb", Schemas: "schemas.binpb"}
	b, err = json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal with provider: %v", err)
	}
	var generic struct {
		Provider map[string]any `json:"provider"`
	}
	if err := json.Unmarshal(b, &generic); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for k := range generic.Provider {
		switch k {
		case "descriptor", "schemas":
		default:
			t.Errorf("the provider section has a field the kernel doesn't know: %q", k)
		}
	}
	if generic.Provider["descriptor"] != "provider.binpb" ||
		generic.Provider["schemas"] != "schemas.binpb" {
		t.Errorf("wrong provider section content: %+v", generic.Provider)
	}
}

func TestValidateManifest_RejectsBadInput(t *testing.T) {
	digests := map[string]string{"bin/svc": "deadbeef"}

	tests := []struct {
		name  string
		mutex func(*Manifest)
		want  string
	}{
		{"version_code is 0", func(m *Manifest) { m.VersionCode = 0 }, "version_code"},
		{"ABI uses an Android NDK name", func(m *Manifest) { m.SupportedABIs = []string{"arm64-v8a"} }, "supported_abis"},
		{"ABI uses a bare CPU name", func(m *Manifest) { m.SupportedABIs = []string{"aarch64"} }, "supported_abis"},
		{"a service can't be manual", func(m *Manifest) { m.Components[0].LaunchMode = "manual" }, "manual"},
		{"an app can't be always-on", func(m *Manifest) { m.Components[0].Type = "app" }, "always-on"},
		{"entry is an absolute path", func(m *Manifest) { m.Components[0].Entry = "/etc/shadow" }, "relative path"},
		{"entry escapes", func(m *Manifest) { m.Components[0].Entry = "../../etc/shadow" }, "relative path"},
		{"entry not covered by digests", func(m *Manifest) { m.Components[0].Entry = "bin/other" }, "digests"},
		{"invalid runtime", func(m *Manifest) { m.Components[0].Runtime = "python" }, "runtime"},
		{"duplicate component id", func(m *Manifest) {
			m.Components = append(m.Components, m.Components[0])
		}, "duplicated"},
		{"exports interfaces without a provider section", func(m *Manifest) {
			m.Components[0].Exports = []Export{{
				Interface: "nervus.interface.example", Visibility: "public",
			}}
		}, "provider"},
		{"provider.descriptor is empty", func(m *Manifest) {
			m.Provider = &ProviderArtifactsRef{Descriptor: "", Schemas: "schemas.binpb"}
		}, "provider.descriptor"},
		{"provider.schemas is empty", func(m *Manifest) {
			// descriptor must pass the digest coverage check first before the empty schemas check can be reached
			m.Digests = map[string]string{"bin/svc": "deadbeef", "provider.binpb": "cafe"}
			m.Provider = &ProviderArtifactsRef{Descriptor: "provider.binpb", Schemas: ""}
		}, "provider.schemas"},
		{"provider path escapes", func(m *Manifest) {
			m.Provider = &ProviderArtifactsRef{
				Descriptor: "../../etc/shadow", Schemas: "schemas.binpb",
			}
		}, "relative path"},
		{"provider not covered by digests", func(m *Manifest) {
			m.Provider = &ProviderArtifactsRef{
				Descriptor: "provider.binpb", Schemas: "schemas.binpb",
			}
		}, "digests"},
		{"descriptor and schemas are the same file", func(m *Manifest) {
			// Give it its own digests: this case has to get past the coverage check to reach the same-file check
			m.Digests = map[string]string{"bin/svc": "deadbeef", "provider.binpb": "cafe"}
			m.Provider = &ProviderArtifactsRef{
				Descriptor: "provider.binpb", Schemas: "provider.binpb",
			}
		}, "the same file"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := minimalManifest()
			m.Digests = digests
			tc.mutex(&m)
			err := validateManifest(m)
			if err == nil {
				t.Fatalf("should have been refused")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q doesn't mention %q", err, tc.want)
			}
		})
	}
}

func TestValidateManifest_AcceptsMinimal(t *testing.T) {
	m := minimalManifest()
	m.Digests = map[string]string{"bin/svc": "deadbeef"}
	if err := validateManifest(m); err != nil {
		t.Fatalf("a minimal valid manifest was refused: %v", err)
	}
}
