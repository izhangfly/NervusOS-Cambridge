package stdinterface_test

import (
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/nervus-os/nervus-ipc/registry"
	"github.com/nervus-os/nervus-ipc/registry/stdinterface"
)

// -update rewrites the committed Kotlin constants file. Normally (in CI) the flag is absent and only the assertions run;
// with the flag, the hashes computed on the Go side are written to disk as the JVM side's single source of truth.
//
// The same shape as the golden vectors' -update, for the same reason: cross-language agreement is guaranteed by "reading one
// file Go produced", not by "each side implementing the algorithm".
var update = flag.Bool("update", false, "rewrite the committed Kotlin schema-hash table")

// repoRoot walks up to the directory containing buf.yaml, i.e. the repository root.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, statErr := os.Stat(filepath.Join(dir, "buf.yaml")); statErr == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("repo root (buf.yaml) not found above CWD")
		}
		dir = parent
	}
}

// TestUpdateCommittedTable writes to disk under -update; otherwise it skips.
func TestUpdateCommittedTable(t *testing.T) {
	if !*update {
		t.Skip("run with -update to regenerate the Kotlin schema-hash table")
	}
	rendered, err := stdinterface.RenderKotlin()
	if err != nil {
		t.Fatalf("RenderKotlin: %v", err)
	}
	path := filepath.Join(repoRoot(t), stdinterface.KotlinTableFile)
	if mkErr := os.MkdirAll(filepath.Dir(path), 0o755); mkErr != nil {
		t.Fatal(mkErr)
	}
	if wErr := os.WriteFile(path, rendered, 0o644); wErr != nil {
		t.Fatal(wErr)
	}
	t.Logf("wrote %d interfaces to %s", len(stdinterface.All()), path)
}

// TestCommittedTableMatches is the core gate: the committed Kotlin file must equal byte for byte
// what this package computes now.
//
// Changing any standard interface's .proto without rerunning generation turns this red. Without this gate, the drift shows up
// as a JVM Provider's registration being refused on the real machine, with an error that only says "schema hash mismatch",
// not who is stale.
func TestCommittedTableMatches(t *testing.T) {
	if *update {
		t.Skip("skip assertions during -update")
	}
	want, err := os.ReadFile(filepath.Join(repoRoot(t), stdinterface.KotlinTableFile))
	if err != nil {
		t.Fatalf("read committed table (run `go test ./registry/stdinterface -run TestUpdateCommittedTable -update`): %v", err)
	}
	got, err := stdinterface.RenderKotlin()
	if err != nil {
		t.Fatalf("RenderKotlin: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("committed Kotlin table is stale; rerun with -update\n--- got ---\n%s\n--- want ---\n%s",
			got, want)
	}
}

// TestHashesMatchBuildSchemaBundle asserts every interface's hash in the list really is
// the output of registry.BuildSchemaBundleWithEvents.
//
// This differs from the one above: that pins "what's on disk equals what's computed now", this pins "what's computed now uses
// the same constructor as nervud's bootstrap and every providergen". Both are needed: with only the former,
// if this package computed hashes its own way, disk and computation would still agree, but not match the kernel.
func TestHashesMatchBuildSchemaBundle(t *testing.T) {
	entries, err := stdinterface.Entries()
	if err != nil {
		t.Fatalf("Entries: %v", err)
	}
	if len(entries) != len(stdinterface.All()) {
		t.Fatalf("got %d entries, want %d", len(entries), len(stdinterface.All()))
	}
	for i, iface := range stdinterface.All() {
		bundle, berr := registry.BuildSchemaBundleWithEvents(
			iface.ID, iface.Major, iface.MethodEnum, iface.EventEnum)
		if berr != nil {
			t.Fatalf("%s: BuildSchemaBundleWithEvents: %v", iface.ID, berr)
		}
		if entries[i].ID != iface.ID || entries[i].Major != iface.Major {
			t.Errorf("entry %d = %s@%d, want %s@%d",
				i, entries[i].ID, entries[i].Major, iface.ID, iface.Major)
			continue
		}
		if string(entries[i].SchemaHash) != string(bundle.GetSchemaHash()) {
			t.Errorf("%s hash mismatch\n got=%x\nwant=%x",
				iface.ID, entries[i].SchemaHash, bundle.GetSchemaHash())
		}
		// The sha256 length is an invariant ParseSchemaBundle checks explicitly, so pin it while here:
		// a hash of the wrong length would be refused by the kernel at registration for a different reason, harder to track down
		if len(entries[i].SchemaHash) != 32 {
			t.Errorf("%s hash has %d bytes, want 32", iface.ID, len(entries[i].SchemaHash))
		}
	}
}

// TestNoDuplicateInterfaces asserts the list has no duplicate (id, major).
//
// A duplicate would put two constants of the same name in the committed table. The Kotlin side wouldn't compile, but the error
// would point at the generated output rather than the list, so stopping it here is easier.
func TestNoDuplicateInterfaces(t *testing.T) {
	seen := make(map[string]struct{})
	for _, iface := range stdinterface.All() {
		key := iface.ID
		if _, dup := seen[key]; dup {
			t.Errorf("duplicate interface %s", key)
		}
		seen[key] = struct{}{}
	}
}
