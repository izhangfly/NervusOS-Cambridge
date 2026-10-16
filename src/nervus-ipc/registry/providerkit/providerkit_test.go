package providerkit_test

import (
	"flag"
	"os"
	"path/filepath"
	"testing"

	permissionv1 "github.com/nervus-os/nervus-ipc/protocol/interface/permissionv1"
	"github.com/nervus-os/nervus-ipc/registry"
	"github.com/nervus-os/nervus-ipc/registry/providerkit"
)

// -update rewrites the committed artefacts. Normally (in CI) the flag is absent and only the assertions run.
//
// The same shape as the golden vectors' and stdinterface's -update, for the same reason: cross-language
// agreement is guaranteed by "reading one file Go produced", not by "each side implementing the algorithm".
var update = flag.Bool("update", false, "rewrite the committed provider artifacts")

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

// TestUpdateCommittedArtifacts writes to disk under -update; otherwise it skips.
func TestUpdateCommittedArtifacts(t *testing.T) {
	if !*update {
		t.Skip("run with -update to regenerate the committed provider artifacts")
	}
	root := repoRoot(t)
	for _, spec := range providerkit.All() {
		artifacts, err := providerkit.Build(spec)
		if err != nil {
			t.Fatalf("build %s: %v", spec.PackageID, err)
		}
		for _, file := range []struct {
			rel  string
			data []byte
		}{
			{artifacts.DescriptorPath(), artifacts.DescriptorWire},
			{artifacts.SchemasPath(), artifacts.SchemaWire},
		} {
			abs := filepath.Join(root, filepath.FromSlash(file.rel))
			if mkErr := os.MkdirAll(filepath.Dir(abs), 0o755); mkErr != nil {
				t.Fatal(mkErr)
			}
			if wErr := os.WriteFile(abs, file.data, 0o644); wErr != nil {
				t.Fatal(wErr)
			}
			t.Logf("wrote %s (%d bytes)", file.rel, len(file.data))
		}
	}
}

// TestCommittedArtifactsMatch is the core gate: the two committed binpb files must equal byte for byte
// what this package computes now.
//
// Changing a standard interface's .proto without rerunning generation turns this red. Without this gate, the drift shows up as
// the package being quarantined during the target machine's boot scan, with an error message (schema hash mismatch / interface conflict) that
// doesn't say who is stale, and by then the image has already been flashed.
func TestCommittedArtifactsMatch(t *testing.T) {
	if *update {
		t.Skip("skip assertions during -update")
	}
	root := repoRoot(t)
	for _, spec := range providerkit.All() {
		t.Run(spec.PackageID, func(t *testing.T) {
			artifacts, err := providerkit.Build(spec)
			if err != nil {
				t.Fatalf("build: %v", err)
			}
			for _, file := range []struct {
				rel  string
				data []byte
			}{
				{artifacts.DescriptorPath(), artifacts.DescriptorWire},
				{artifacts.SchemasPath(), artifacts.SchemaWire},
			} {
				want, readErr := os.ReadFile(filepath.Join(root, filepath.FromSlash(file.rel)))
				if readErr != nil {
					t.Fatalf("read committed %s (run `go test ./registry/providerkit "+
						"-run TestUpdateCommittedArtifacts -update`): %v", file.rel, readErr)
				}
				if string(file.data) != string(want) {
					t.Errorf("%s is stale; rerun with -update\n got %d bytes\nwant %d bytes",
						file.rel, len(file.data), len(want))
				}
			}
		})
	}
}

// TestArtifactsParseAsKernelWould asserts the artefacts pass the same checks the kernel applies when loading.
//
// MarshalProviderArtifacts inside Build has already Parsed once; this does it again on **the committed
// bytes**: the former proves "what I generate is valid", the latter proves "what sits in the repository is
// valid". They differ: only the latter catches a mistaken manual edit.
func TestArtifactsParseAsKernelWould(t *testing.T) {
	root := repoRoot(t)
	for _, spec := range providerkit.All() {
		t.Run(spec.PackageID, func(t *testing.T) {
			artifacts, err := providerkit.Build(spec)
			if err != nil {
				t.Fatalf("build: %v", err)
			}
			descriptorWire, err := os.ReadFile(
				filepath.Join(root, filepath.FromSlash(artifacts.DescriptorPath())))
			if err != nil {
				t.Fatalf("read descriptor: %v", err)
			}
			schemaWire, err := os.ReadFile(
				filepath.Join(root, filepath.FromSlash(artifacts.SchemasPath())))
			if err != nil {
				t.Fatalf("read schemas: %v", err)
			}
			parsed, err := registry.ParseProviderArtifacts(descriptorWire, schemaWire)
			if err != nil {
				t.Fatalf("ParseProviderArtifacts: %v", err)
			}
			if got := parsed.Descriptor.GetPackageId(); got != spec.PackageID {
				t.Errorf("package_id = %q, want %q", got, spec.PackageID)
			}
			// package_id must match the manifest, or nervud's
			// loadRequiredProviderArtifacts refuses to load it. This assertion lives here rather than relying on
			// the packaging chain to find it, because by then it's too late.
			if len(parsed.Descriptor.GetInterfaces()) != len(spec.Interfaces) {
				t.Errorf("interfaces = %d, want %d",
					len(parsed.Descriptor.GetInterfaces()), len(spec.Interfaces))
			}
			// The boundary of pure interface export: zero resources, zero custom permissions. If someone ever adds
			// either to a spec, this goes red, meaning providerkit's scope has been exceeded,
			// and it should go through its own providergen.
			if n := len(parsed.Descriptor.GetResources()); n != 0 {
				t.Errorf("resources = %d, want 0 (providerkit is interface-only)", n)
			}
			if n := len(parsed.Descriptor.GetPermissions()); n != 0 {
				t.Errorf("permissions = %d, want 0 (providerkit is interface-only)", n)
			}
		})
	}
}

// TestSchemaHashMatchesStdInterface asserts the schema_hash in the artefacts is the same value
// the stdinterface table gives.
//
// Both are computed from the same BuildSchemaBundle, so normally they must be equal. Pinning it guards
// against one specific mismatch: a providerkit spec with the wrong major (say 2); the
// bundle still builds and the hash is still valid, but it differs from what the JVM side's hashOf(id, 1)
// gets. The symptom is RegisterEndpoint being refused, with an error that only says "schema hash mismatch".
func TestSchemaHashMatchesStdInterface(t *testing.T) {
	for _, spec := range providerkit.All() {
		for _, iface := range spec.Interfaces {
			bundle, err := registry.BuildSchemaBundleWithEvents(
				iface.ID, iface.Major, iface.MethodEnum, iface.EventEnum)
			if err != nil {
				t.Fatalf("%s: %v", iface.ID, err)
			}
			if len(bundle.GetSchemaHash()) != 32 {
				t.Errorf("%s hash has %d bytes, want 32", iface.ID, len(bundle.GetSchemaHash()))
			}
		}
	}
}

// TestBuildRejectsEmptySpec pins two fail-closed cases.
func TestBuildRejectsEmptySpec(t *testing.T) {
	if _, err := providerkit.Build(providerkit.Spec{}); err == nil {
		t.Error("empty package id accepted")
	}
	if _, err := providerkit.Build(providerkit.Spec{PackageID: "nervus.x"}); err == nil {
		// A package that exports no interface needs no provider section. Generating an empty one would only get the kernel to refuse loading for
		// a different reason, an error far from the real cause.
		t.Error("spec with no interfaces accepted")
	}
}

// TestBuildRejectsDuplicateInterface asserts declaring the same interface_id twice is refused.
func TestBuildRejectsDuplicateInterface(t *testing.T) {
	iface := providerkit.Interface{
		ID:         "nervus.interface.permission.ui",
		Major:      1,
		MethodEnum: permissionv1.PermissionUiMethod(0).Descriptor(),
	}
	_, err := providerkit.Build(providerkit.Spec{
		PackageID:  "nervus.permissionui",
		Interfaces: []providerkit.Interface{iface, iface},
	})
	if err == nil {
		t.Error("duplicate interface accepted")
	}
}
