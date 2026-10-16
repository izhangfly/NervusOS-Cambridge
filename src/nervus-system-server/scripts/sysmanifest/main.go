package main

import (
	"crypto/ed25519"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"flag"
	"fmt"
	"os"
	"path/filepath"
)

const usage = `sysmanifest: fills in manifest.json and manifest.sig for a system package directory tree

Usage:
  sysmanifest -dir <package dir> -template <template.json> -key <private-key.pem> [-version-code N]

Arguments:
  -dir           the system package directory (containing the built payload, such as bin/pkgmanagerd)
  -template      the manifest template; this tool fills in the digests field
  -key           Ed25519 private key (PKCS#8 PEM). Generate with openssl genpkey -algorithm ed25519
  -version-code  overrides version_code in the template (for CI increments)
  -print         only print the manifest that would be written, without writing it

Output:
  <dir>/manifest.json   the exact bytes that are signed
  <dir>/manifest.sig    Ed25519 signature block for the platform-release role

Note: a system package is an [unpacked directory tree], not a .nspkg. The latter is the format for dynamic install.
`

func b64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

func main() {
	dir := flag.String("dir", "", "system package directory")
	tmpl := flag.String("template", "", "manifest template JSON")
	keyPath := flag.String("key", "", "Ed25519 private key PEM")
	versionCode := flag.Uint64("version-code", 0, "override version_code")
	printOnly := flag.Bool("print", false, "print only, don't write")
	flag.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	flag.Parse()

	if err := run(*dir, *tmpl, *keyPath, *versionCode, *printOnly); err != nil {
		fmt.Fprintf(os.Stderr, "sysmanifest: %v\n", err)
		os.Exit(1)
	}
}

func run(dir, tmplPath, keyPath string, versionCode uint64, printOnly bool) error {
	if dir == "" || tmplPath == "" {
		return fmt.Errorf("-dir and -template are required (-h for usage)")
	}
	if !printOnly && keyPath == "" {
		return fmt.Errorf("-key is required (or use -print to preview)")
	}

	tmplBytes, err := os.ReadFile(tmplPath)
	if err != nil {
		return fmt.Errorf("read template: %w", err)
	}
	var m Manifest
	// The template is parsed with DisallowUnknownFields too: a misspelt field name in the template makes this tool fail at once,
	// instead of being quietly dropped and only noticed when the kernel loads a package whose setting didn't take effect.
	dec := json.NewDecoder(newReader(tmplBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return fmt.Errorf("parse template: %w", err)
	}

	if versionCode != 0 {
		m.VersionCode = versionCode
	}
	if m.Schema == 0 {
		m.Schema = manifestSchemaV1
	}

	// Digests are always computed by this tool and [overwrite] anything in the template: a hand-written digest will
	// go stale, and a stale digest shows up at load time as "corrupt image", a false signal that can send people
	// checking hardware for hours.
	digests, err := computeDigests(dir)
	if err != nil {
		return fmt.Errorf("compute digests: %w", err)
	}
	m.Digests = digests

	if err := validateManifest(m); err != nil {
		return fmt.Errorf("manifest validation failed: %w", err)
	}

	manifestBytes, err := encodeManifest(m)
	if err != nil {
		return err
	}

	if printOnly {
		fmt.Printf("%s", manifestBytes)
		fmt.Fprintf(os.Stderr, "\n--- %d payload files ---\n", len(digests))
		for _, k := range sortedKeys(digests) {
			fmt.Fprintf(os.Stderr, "  %s  %s\n", digests[k][:16], k)
		}
		return nil
	}

	priv, err := loadPrivateKey(keyPath)
	if err != nil {
		return err
	}
	sigBlock := signManifest(manifestBytes, priv)
	sigBytes, err := json.MarshalIndent(sigBlock, "", "  ")
	if err != nil {
		return fmt.Errorf("encode signature block: %w", err)
	}
	sigBytes = append(sigBytes, '\n')

	// Write manifest.json before manifest.sig. The other order could leave an intermediate state of "new signature on disk,
	// old manifest", which is bound to fail verification, and the failure would point at
	// "signature mismatch" rather than "the build was interrupted", which is very misleading.
	if err := os.WriteFile(filepath.Join(dir, manifestFileName), manifestBytes, 0o644); err != nil {
		return fmt.Errorf("write manifest.json: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, signatureFileName), sigBytes, 0o644); err != nil {
		return fmt.Errorf("write manifest.sig: %w", err)
	}

	fmt.Printf("✅ %s\n", m.PackageID)
	fmt.Printf("   version      %s (code %d)\n", m.Version, m.VersionCode)
	fmt.Printf("   components   %d\n", len(m.Components))
	fmt.Printf("   digests      %d files\n", len(digests))
	fmt.Printf("   key_id       %s\n", sigBlock.Signatures[0].KeyID)
	return nil
}

// loadPrivateKey reads an Ed25519 private key from PKCS#8 PEM.
//
// Only Ed25519 is accepted: the kernel's SigAlg only allows ed25519 and refuses unknown algorithms without
// any "best-effort compatibility". Refusing RSA/ECDSA here beats producing a package the kernel refuses at load time.
func loadPrivateKey(path string) (ed25519.PrivateKey, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read private key: %w", err)
	}
	block, _ := pem.Decode(raw)
	if block == nil {
		return nil, fmt.Errorf("%s is not valid PEM", path)
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse PKCS#8 private key: %w", err)
	}
	priv, ok := key.(ed25519.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("private key type is %T; only Ed25519 is accepted", key)
	}
	return priv, nil
}
