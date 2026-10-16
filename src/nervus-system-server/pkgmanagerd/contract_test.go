package main

import (
	"bytes"
	"crypto/sha256"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	ipcregistry "github.com/nervus-os/nervus-ipc/registry"
)

// The hash used to register at run time must equal the one providergen writes out and that ends up in the Catalog.
//
// The two values come from two independent main packages and stay consistent only by the convention that "both call BuildSchemaBundle",
// which nothing enforces at compile time; this test is where it breaks. main.go once left SchemaHash empty altogether,
// and pkgmanagerd handshook, got refused at registration and exited every two seconds, while callers saw
// RESOURCE_NOT_FOUND from ResolveEndpoint, unrelated to the real cause.
func TestSchemaHashMatchesProvidergenArtifacts(t *testing.T) {
	runtimeHash, err := schemaHash()
	if err != nil {
		t.Fatalf("schemaHash: %v", err)
	}
	if len(runtimeHash) != sha256.Size {
		t.Fatalf("runtime hash has %d bytes, want %d", len(runtimeHash), sha256.Size)
	}

	// Run providergen itself rather than copying its construction logic here: a copy would be changed
	// along with it, and then this test could never go red.
	dir := t.TempDir()
	cmd := exec.Command("go", "run", "./providergen", "-out", dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go run ./providergen: %v\n%s", err, out)
	}

	artifacts, err := ipcregistry.ParseProviderArtifacts(
		readArtifact(t, dir, "provider.binpb"),
		readArtifact(t, dir, "schemas.binpb"))
	if err != nil {
		t.Fatalf("ParseProviderArtifacts: %v", err)
	}

	schema, ok := artifacts.Schemas.Lookup(interfaceID, interfaceMajor)
	if !ok {
		t.Fatalf("no %s@%d in the providergen artifacts", interfaceID, interfaceMajor)
	}
	if !bytes.Equal(runtimeHash, schema.Hash()) {
		t.Errorf("runtime hash doesn't match the providergen artifact\n runtime = %x\n artifact = %x",
			runtimeHash, schema.Hash())
	}
}

func readArtifact(t *testing.T, dir, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return b
}
