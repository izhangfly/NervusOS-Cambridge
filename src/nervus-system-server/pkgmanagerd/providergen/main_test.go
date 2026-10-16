package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	ipcv1 "github.com/nervus-os/nervus-ipc/protocol/ipcv1"
	ipcregistry "github.com/nervus-os/nervus-ipc/registry"
)

// The contract this file produces must match, field by field, the one in nervud's internal/catalog/bootstrap.go,
// bootstrapInterface(InterfacePackageManager, ..., "perm.pkg.query", UNSPECIFIED, nil, "", "")
// or the kernel Builder's sameInterfaceContract makes the whole Catalog build fail.
//
// These assertions anchor that "agreement made elsewhere" in this repository: changing any of them goes red here,
// instead of waiting for the target to start and report "conflicts with definition owned by ...".
func TestBuildArtifacts_MatchesKernelBootstrapContract(t *testing.T) {
	descriptorWire, schemaWire, err := buildArtifacts()
	if err != nil {
		t.Fatalf("buildArtifacts: %v", err)
	}

	artifacts, err := ipcregistry.ParseProviderArtifacts(descriptorWire, schemaWire)
	if err != nil {
		t.Fatalf("ParseProviderArtifacts: %v", err)
	}

	if got := artifacts.Descriptor.GetPackageId(); got != packageID {
		t.Errorf("package_id = %q, want %q", got, packageID)
	}
	ifaces := artifacts.Descriptor.GetInterfaces()
	if len(ifaces) != 1 {
		t.Fatalf("interface count = %d, want 1", len(ifaces))
	}
	iface := ifaces[0]

	if iface.GetInterfaceId() != interfaceID {
		t.Errorf("interface_id = %q, want %q", iface.GetInterfaceId(), interfaceID)
	}
	if iface.GetRequiredPermission() != requiredPermission {
		t.Errorf("required_permission = %q, want %q (the kernel bootstrap uses this)",
			iface.GetRequiredPermission(), requiredPermission)
	}
	// The install interface isn't bound to a physical resource. If these three were non-empty they'd disagree with the kernel bootstrap
	if iface.GetResourceRiskFloor() != ipcv1.RiskClass_RISK_CLASS_UNSPECIFIED {
		t.Errorf("resource_risk_floor = %v, want UNSPECIFIED", iface.GetResourceRiskFloor())
	}
	if len(iface.GetCompatibleResourceTypes()) != 0 {
		t.Errorf("compatible_resource_types = %v, want empty", iface.GetCompatibleResourceTypes())
	}
	if iface.GetDefaultResourceType() != "" || iface.GetDefaultResourceRole() != "" {
		t.Errorf("default resource = (%q, %q), want both empty",
			iface.GetDefaultResourceType(), iface.GetDefaultResourceRole())
	}

	// Don't define perm.* here: those are platform permissions owned by nervud's bootstrap,
	// and defining them again hits "conflicts with definition owned by"
	if n := len(artifacts.Descriptor.GetPermissions()); n != 0 {
		t.Errorf("permission count = %d, want 0 (platform permissions are defined by the kernel)", n)
	}
	if n := len(artifacts.Descriptor.GetResources()); n != 0 {
		t.Errorf("resource count = %d, want 0 (installing uses no ManagedResource)", n)
	}

	// The declared schema_hash must really be the bundle's hash
	versions := iface.GetInterfaceVersions()
	if len(versions) != 1 || versions[0].GetMajor() != major {
		t.Fatalf("interface_versions = %+v, want a single major=%d", versions, major)
	}
	schema, ok := artifacts.Schemas.Lookup(interfaceID, major)
	if !ok {
		t.Fatalf("%s@%d not found in the schema set", interfaceID, major)
	}
	if !bytes.Equal(schema.Hash(), versions[0].GetSchemaHash()) {
		t.Error("the declared schema_hash doesn't match the bundle's actual hash")
	}
}

// Digests are computed at build time and can't be recomputed on the target, so the same input must produce the same bytes every time.
func TestBuildArtifacts_IsDeterministic(t *testing.T) {
	firstDesc, firstSchema, err := buildArtifacts()
	if err != nil {
		t.Fatalf("buildArtifacts: %v", err)
	}
	secondDesc, secondSchema, err := buildArtifacts()
	if err != nil {
		t.Fatalf("buildArtifacts again: %v", err)
	}
	if !bytes.Equal(firstDesc, secondDesc) || !bytes.Equal(firstSchema, secondSchema) {
		t.Fatal("the provider contract encoding isn't deterministic; the digest would drift between builds")
	}
}

// File names must match the provider section of manifest.json.in; on a mismatch sysmanifest fails with
// "not covered by digests", and that error doesn't say the file name was wrong.
func TestRun_WritesBothArtifactFiles(t *testing.T) {
	dir := t.TempDir()
	if err := run(dir); err != nil {
		t.Fatalf("run: %v", err)
	}
	for _, name := range []string{descriptorFileName, schemasFileName} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("missing %s: %v", name, err)
		}
		if info.Size() == 0 {
			t.Errorf("%s is an empty file", name)
		}
	}
}

func TestRun_RequiresOutDir(t *testing.T) {
	if err := run(""); err == nil {
		t.Fatal("an empty -out was accepted; it should be an error")
	}
}
