package registry

import (
	"bytes"
	"strings"
	"testing"

	ipcv1 "github.com/nervus-os/nervus-ipc/protocol/ipcv1"
	dogv1 "github.com/nervus-os/nervus-ipc/protocol/oem/acme/dog/v1"
)

const (
	metaPackageID   = "com.vendor.cam"
	metaInterfaceID = "com.vendor.cam.interface.stream"
	metaPermission  = "com.vendor.cam.permission.stream"
)

// cameraLikeMethods is a typical metadata interface: start stream + stop stream, zero message types,
// with a Transfer budget on the start method. This is exactly what a camera would look like.
func cameraLikeMethods() []*ipcv1.MethodMeta {
	return []*ipcv1.MethodMeta{
		{
			MethodId:           1,
			RequiredPermission: metaPermission,
			RiskClass:          ipcv1.RiskClass_RISK_CLASS_NORMAL,
			IsReadOnly:         true,
			Transfer: &ipcv1.TransferPolicy{
				Direction:         ipcv1.TransferDirection_TRANSFER_DIRECTION_PROVIDER_TO_CALLER,
				MaxStreams:        1,
				MaxPacketBytes:    4 << 20,
				MaxBytesPerSecond: 64 << 20,
			},
		},
		{
			MethodId:           2,
			RequiredPermission: metaPermission,
			RiskClass:          ipcv1.RiskClass_RISK_CLASS_NORMAL,
		},
	}
}

// cameraLikeEvents is a typical set of metadata events: device state change + stream ended.
func cameraLikeEvents() []*ipcv1.EventMeta {
	return []*ipcv1.EventMeta{
		{
			EventId:            1,
			RequiredPermission: metaPermission,
			RiskClass:          ipcv1.RiskClass_RISK_CLASS_NORMAL,
			DeliveryClass:      ipcv1.DeliveryClass_DELIVERY_CLASS_STATE,
			MaxEventsPerSecond: 10,
		},
		{
			EventId:            2,
			RequiredPermission: metaPermission,
			RiskClass:          ipcv1.RiskClass_RISK_CLASS_NORMAL,
			DeliveryClass:      ipcv1.DeliveryClass_DELIVERY_CLASS_RELIABLE,
		},
	}
}

func metadataDescriptor(t *testing.T, methods []*ipcv1.MethodMeta) *ipcv1.ProviderDescriptor {
	t.Helper()
	hash, err := MethodsHash(methods)
	if err != nil {
		t.Fatalf("MethodsHash: %v", err)
	}
	return &ipcv1.ProviderDescriptor{
		PackageId: metaPackageID,
		Interfaces: []*ipcv1.ProvidedInterface{{
			InterfaceId: metaInterfaceID,
			InterfaceVersions: []*ipcv1.ProvidedInterfaceVersion{{
				Major: 1, SchemaHash: hash, Methods: methods,
			}},
			RequiredPermission: metaPermission,
		}},
		Permissions: []*ipcv1.DefinedPermission{{
			Id:           metaPermission,
			GrantMode:    ipcv1.GrantMode_GRANT_MODE_NORMAL,
			RiskClass:    ipcv1.RiskClass_RISK_CLASS_NORMAL,
			MinimumTrust: ipcv1.PermissionTrustFloor_PERMISSION_TRUST_FLOOR_ORDINARY,
			DisplayName:  &ipcv1.LocalizedText{En: "Stream"},
			Description:  &ipcv1.LocalizedText{En: "Stream"},
		}},
	}
}

// An interface with no .proto messages at all must pass the full Parse flow.
// That's the whole point of this change: adding a capability without writing messages.
func TestMetadataInterfaceParsesWithoutSchemaBundle(t *testing.T) {
	descriptor := metadataDescriptor(t, cameraLikeMethods())

	// **An empty bundle set**, with no schema at all
	descriptorWire, schemaWire, err := MarshalProviderArtifacts(
		descriptor, &ipcv1.InterfaceSchemaBundleSet{})
	if err != nil {
		t.Fatalf("MarshalProviderArtifacts: %v", err)
	}

	artifacts, err := ParseProviderArtifacts(descriptorWire, schemaWire)
	if err != nil {
		t.Fatalf("ParseProviderArtifacts: %v", err)
	}
	schema, ok := artifacts.Schemas.Lookup(metaInterfaceID, 1)
	if !ok {
		t.Fatal("metadata interface not in the SchemaSet")
	}
	if schema.Files() != nil {
		t.Error("metadata interface should not carry descriptor files")
	}
	if len(schema.Methods()) != 2 {
		t.Fatalf("method count = %d, want 2", len(schema.Methods()))
	}
	open, ok := schema.Method(1)
	if !ok || open.GetTransfer().GetMaxPacketBytes() != 4<<20 {
		t.Fatalf("method 1 = %+v", open)
	}
	if open.GetRequestType() != "" || open.GetResponseType() != "" {
		t.Error("metadata interface methods should not have message type names")
	}
}

// The contract identity must not depend on method order: there's no reason the packager and the kernel, or two Providers,
// agree on method order, and if the hash depended on it the symptom would be "two vendors wrote the same interface and got judged conflicting".
func TestMethodsHashIsOrderIndependent(t *testing.T) {
	methods := cameraLikeMethods()
	forward, err := MethodsHash(methods)
	if err != nil {
		t.Fatalf("MethodsHash: %v", err)
	}
	reversed := []*ipcv1.MethodMeta{methods[1], methods[0]}
	backward, err := MethodsHash(reversed)
	if err != nil {
		t.Fatalf("MethodsHash reversed: %v", err)
	}
	if !bytes.Equal(forward, backward) {
		t.Fatal("MethodsHash depends on method order")
	}
}

// Changing any item of metadata must change the contract identity; that's what sameInterfaceContract relies on.
// When two Providers declare different permissions or Transfer budgets, the kernel must be able to tell.
func TestMethodsHashDetectsMetadataChange(t *testing.T) {
	base, err := MethodsHash(cameraLikeMethods())
	if err != nil {
		t.Fatalf("MethodsHash: %v", err)
	}

	for _, tc := range []struct {
		name   string
		mutate func([]*ipcv1.MethodMeta)
	}{
		{"change permission", func(m []*ipcv1.MethodMeta) { m[0].RequiredPermission = "com.vendor.cam.permission.other" }},
		{"change risk class", func(m []*ipcv1.MethodMeta) { m[0].RiskClass = ipcv1.RiskClass_RISK_CLASS_PHYSICAL_CONTROL }},
		{"loosen rate budget", func(m []*ipcv1.MethodMeta) { m[0].Transfer.MaxBytesPerSecond = 1 << 30 }},
		{"change method id", func(m []*ipcv1.MethodMeta) { m[1].MethodId = 3 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			methods := cameraLikeMethods()
			tc.mutate(methods)
			got, err := MethodsHash(methods)
			if err != nil {
				t.Fatalf("MethodsHash: %v", err)
			}
			if bytes.Equal(got, base) {
				t.Fatalf("contract hash unchanged after %s", tc.name)
			}
		})
	}
}

// A declared schema_hash that doesn't match the actual methods must be refused: otherwise a Provider could
// register its own methods under someone else's contract identity.
func TestMetadataInterfaceRejectsHashMismatch(t *testing.T) {
	descriptor := metadataDescriptor(t, cameraLikeMethods())
	descriptor.Interfaces[0].InterfaceVersions[0].SchemaHash = bytes.Repeat([]byte{0xAB}, 32)

	if _, _, err := MarshalProviderArtifacts(
		descriptor, &ipcv1.InterfaceSchemaBundleSet{},
	); err == nil {
		t.Fatal("schema_hash mismatching the methods was accepted")
	}
}

// A metadata interface may not declare message type names: there's no bundle to resolve them, and letting them through would only carry
// a type name that can never resolve all the way to dispatch before failing.
func TestMetadataInterfaceRejectsMessageTypes(t *testing.T) {
	methods := cameraLikeMethods()
	methods[0].RequestType = "com.vendor.cam.v1.OpenRequest"
	descriptor := metadataDescriptor(t, methods)

	_, _, err := MarshalProviderArtifacts(descriptor, &ipcv1.InterfaceSchemaBundleSet{})
	if err == nil || !strings.Contains(err.Error(), "carries no schema bundle") {
		t.Fatalf("err = %v, want message type names refused", err)
	}
}

// An unbounded Transfer budget has to be stopped at packaging time. nervud's Begin would refuse with
// "method transfer policy is unbounded", but by then the error appears at the first stream start.
func TestMetadataInterfaceRejectsUnboundedTransfer(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*ipcv1.TransferPolicy)
	}{
		{"max_streams is 0", func(p *ipcv1.TransferPolicy) { p.MaxStreams = 0 }},
		{"max_packet_bytes is 0", func(p *ipcv1.TransferPolicy) { p.MaxPacketBytes = 0 }},
		{"max_bytes_per_second is 0", func(p *ipcv1.TransferPolicy) { p.MaxBytesPerSecond = 0 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			methods := cameraLikeMethods()
			tc.mutate(methods[0].Transfer)
			descriptor := metadataDescriptor(t, methods)
			if _, _, err := MarshalProviderArtifacts(
				descriptor, &ipcv1.InterfaceSchemaBundleSet{},
			); err == nil {
				t.Fatalf("%s was accepted", tc.name)
			}
		})
	}
}

// The same (interface, major) both inlining methods and carrying a bundle: two contracts, no way to tell which is authoritative.
func TestMetadataInterfaceRejectsDoubleDeclaration(t *testing.T) {
	bundle, err := BuildSchemaBundle(metaInterfaceID, 1, dogv1.RawGaitMethod(0).Descriptor())
	if err != nil {
		t.Fatalf("BuildSchemaBundle: %v", err)
	}
	descriptor := metadataDescriptor(t, cameraLikeMethods())

	_, _, err = MarshalProviderArtifacts(descriptor, &ipcv1.InterfaceSchemaBundleSet{
		Bundles: []*ipcv1.InterfaceSchemaBundle{bundle},
	})
	if err == nil || !strings.Contains(err.Error(), "inline methods and a schema bundle") {
		t.Fatalf("err = %v, want the double declaration refused", err)
	}
}
