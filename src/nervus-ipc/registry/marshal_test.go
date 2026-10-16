package registry

import (
	"bytes"
	"testing"

	"google.golang.org/protobuf/proto"

	pkgv1 "github.com/nervus-os/nervus-ipc/protocol/interface/pkgmanagerv1"
	ipcv1 "github.com/nervus-os/nervus-ipc/protocol/ipcv1"
)

func pkgManagerArtifacts(t *testing.T) (*ipcv1.ProviderDescriptor, *ipcv1.InterfaceSchemaBundleSet) {
	t.Helper()
	const interfaceID = "nervus.interface.pkg.manager"
	bundle, err := BuildSchemaBundle(interfaceID, 1, pkgv1.PackageManagerMethod(0).Descriptor())
	if err != nil {
		t.Fatalf("BuildSchemaBundle: %v", err)
	}
	descriptor := &ipcv1.ProviderDescriptor{
		PackageId: "nervus.pkgmanagerd",
		Interfaces: []*ipcv1.ProvidedInterface{{
			InterfaceId: interfaceID,
			InterfaceVersions: []*ipcv1.ProvidedInterfaceVersion{{
				Major: 1, SchemaHash: append([]byte(nil), bundle.GetSchemaHash()...),
			}},
			RequiredPermission: "perm.pkg.query",
		}},
	}
	return descriptor, &ipcv1.InterfaceSchemaBundleSet{
		Bundles: []*ipcv1.InterfaceSchemaBundle{bundle},
	}
}

// The digest covers exactly these two byte strings, so the same input must encode to exactly the same result every time.
// If that didn't hold, "reproducible builds" and "digests computed at build time" would both collapse.
func TestMarshalProviderArtifactsIsDeterministic(t *testing.T) {
	descriptor, bundles := pkgManagerArtifacts(t)

	firstDesc, firstSchema, err := MarshalProviderArtifacts(descriptor, bundles)
	if err != nil {
		t.Fatalf("MarshalProviderArtifacts: %v", err)
	}
	secondDesc, secondSchema, err := MarshalProviderArtifacts(descriptor, bundles)
	if err != nil {
		t.Fatalf("MarshalProviderArtifacts again: %v", err)
	}
	if !bytes.Equal(firstDesc, secondDesc) || !bytes.Equal(firstSchema, secondSchema) {
		t.Fatal("provider artifact encoding is not deterministic")
	}
}

// The encoded bytes must read back unchanged through ParseProviderArtifacts: what the packager writes and what the kernel reads
// are one contract, with no second encoding in between.
func TestMarshalProviderArtifactsRoundTrips(t *testing.T) {
	descriptor, bundles := pkgManagerArtifacts(t)

	descriptorWire, schemaWire, err := MarshalProviderArtifacts(descriptor, bundles)
	if err != nil {
		t.Fatalf("MarshalProviderArtifacts: %v", err)
	}
	artifacts, err := ParseProviderArtifacts(descriptorWire, schemaWire)
	if err != nil {
		t.Fatalf("ParseProviderArtifacts: %v", err)
	}
	if !proto.Equal(artifacts.Descriptor, descriptor) {
		t.Fatal("round-tripped descriptor differs from input")
	}
	if artifacts.Schemas.Len() != 1 {
		t.Fatalf("schema count = %d, want 1", artifacts.Schemas.Len())
	}
}

// Content the kernel would refuse must be stopped at packaging time. "An interface's declared schema_hash not matching the bundle"
// stands for it here: discovering it at load time on the target machine means the image has already been flashed.
func TestMarshalProviderArtifactsRejectsInvalidInput(t *testing.T) {
	descriptor, bundles := pkgManagerArtifacts(t)
	descriptor.Interfaces[0].InterfaceVersions[0].SchemaHash = []byte("not-the-real-hash")

	if _, _, err := MarshalProviderArtifacts(descriptor, bundles); err == nil {
		t.Fatal("MarshalProviderArtifacts accepted a mismatched schema hash, want failure")
	}
}

func TestMarshalProviderArtifactsRejectsNil(t *testing.T) {
	descriptor, bundles := pkgManagerArtifacts(t)

	if _, _, err := MarshalProviderArtifacts(nil, bundles); err == nil {
		t.Fatal("nil descriptor accepted, want failure")
	}
	if _, _, err := MarshalProviderArtifacts(descriptor, nil); err == nil {
		t.Fatal("nil bundle set accepted, want failure")
	}
}
