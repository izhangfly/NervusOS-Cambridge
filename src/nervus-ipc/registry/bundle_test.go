package registry

import (
	"bytes"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoregistry"

	pkgv1 "github.com/nervus-os/nervus-ipc/protocol/interface/pkgmanagerv1"
	ipcv1 "github.com/nervus-os/nervus-ipc/protocol/ipcv1"
	dogv1 "github.com/nervus-os/nervus-ipc/protocol/oem/acme/dog/v1"
)

func TestSchemaBundleBuildAndParse(t *testing.T) {
	const interfaceID = "nervus.interface.pkg.manager"
	enum := pkgv1.PackageManagerMethod(0).Descriptor()

	bundle, err := BuildSchemaBundle(interfaceID, 1, enum)
	if err != nil {
		t.Fatalf("BuildSchemaBundle: %v", err)
	}
	again, err := BuildSchemaBundle(interfaceID, 1, enum)
	if err != nil {
		t.Fatalf("BuildSchemaBundle again: %v", err)
	}
	if !proto.Equal(bundle, again) {
		t.Fatal("schema bundle construction is not deterministic")
	}
	if len(bundle.GetSchemaHash()) != 32 {
		t.Fatalf("schema hash has %d bytes", len(bundle.GetSchemaHash()))
	}
	if bundle.GetMethodEnumType() != "nervus.interface.pkgmanager.v1.PackageManagerMethod" {
		t.Fatalf("method enum = %q", bundle.GetMethodEnumType())
	}

	schema, err := ParseSchemaBundle(bundle)
	if err != nil {
		t.Fatalf("ParseSchemaBundle: %v", err)
	}
	if schema.InterfaceID() != interfaceID || schema.Major() != 1 || schema.Methods() == nil {
		t.Fatalf("parsed schema identity = %q@%d", schema.InterfaceID(), schema.Major())
	}
	if len(schema.Methods()) != 5 {
		t.Fatalf("parsed %d methods, want 5", len(schema.Methods()))
	}
	install, ok := schema.Method(1)
	if !ok || install.GetRequestType() != "nervus.interface.pkgmanager.v1.InstallRequest" {
		t.Fatalf("install metadata = %+v", install)
	}
	install.MethodId = 99
	installAgain, _ := schema.Method(1)
	if installAgain.GetMethodId() != 1 {
		t.Fatal("Schema.Method exposed mutable registry state")
	}
}

func TestSchemaBundleRejectsTamperingAndWrongMethodType(t *testing.T) {
	bundle, err := BuildSchemaBundle(
		"nervus.interface.pkg.manager",
		1,
		pkgv1.PackageManagerMethod(0).Descriptor(),
	)
	if err != nil {
		t.Fatalf("BuildSchemaBundle: %v", err)
	}

	tampered := proto.Clone(bundle).(*ipcv1.InterfaceSchemaBundle)
	tampered.FileDescriptorSet[0] ^= 0xff
	if _, err := ParseSchemaBundle(tampered); err == nil || !strings.Contains(err.Error(), "hash mismatch") {
		t.Fatalf("tampered bundle error = %v", err)
	}

	wrongType := proto.Clone(bundle).(*ipcv1.InterfaceSchemaBundle)
	wrongType.MethodEnumType = "nervus.interface.pkgmanager.v1.InstallRequest"
	if _, err := ParseSchemaBundle(wrongType); err == nil || !strings.Contains(err.Error(), "is not an enum") {
		t.Fatalf("wrong method_enum_type error = %v", err)
	}

	set := &ipcv1.InterfaceSchemaBundleSet{Bundles: []*ipcv1.InterfaceSchemaBundle{
		bundle,
		proto.Clone(bundle).(*ipcv1.InterfaceSchemaBundle),
	}}
	if _, err := ParseSchemaBundleSet(set); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("duplicate bundle error = %v", err)
	}
}

func TestParseProviderArtifactsOEM(t *testing.T) {
	const (
		packageID   = "com.acme.dog"
		interfaceID = "com.acme.dog.interface.raw_gait"
		resourceID  = "com.acme.dog.resource.base"
		permission  = "com.acme.dog.permission.raw_gait"
	)
	bundle, err := BuildSchemaBundle(interfaceID, 1, dogv1.RawGaitMethod(0).Descriptor())
	if err != nil {
		t.Fatalf("BuildSchemaBundle: %v", err)
	}
	descriptor := &ipcv1.ProviderDescriptor{
		PackageId: packageID,
		Interfaces: []*ipcv1.ProvidedInterface{{
			InterfaceId: interfaceID,
			InterfaceVersions: []*ipcv1.ProvidedInterfaceVersion{{
				Major: 1, SchemaHash: append([]byte(nil), bundle.GetSchemaHash()...),
			}},
			RequiredPermission:      permission,
			ResourceRiskFloor:       ipcv1.RiskClass_RISK_CLASS_PHYSICAL_CONTROL,
			CompatibleResourceTypes: []string{resourceID},
			DefaultResourceType:     resourceID,
			DefaultResourceRole:     "base.main",
		}},
		Resources: []*ipcv1.ManagedResource{{
			StableRole:   "base.main",
			ResourceType: resourceID,
			AccessMode:   ipcv1.ResourceAccessMode_RESOURCE_ACCESS_MODE_EXCLUSIVE_CONTROL,
			RiskClass:    ipcv1.RiskClass_RISK_CLASS_PHYSICAL_CONTROL,
		}},
		Permissions: []*ipcv1.DefinedPermission{{
			Id:           permission,
			GrantMode:    ipcv1.GrantMode_GRANT_MODE_USER_CONSENT,
			RiskClass:    ipcv1.RiskClass_RISK_CLASS_PHYSICAL_CONTROL,
			MinimumTrust: ipcv1.PermissionTrustFloor_PERMISSION_TRUST_FLOOR_OEM,
			DisplayName:  &ipcv1.LocalizedText{En: "Raw gait control"},
			Description:  &ipcv1.LocalizedText{En: "Controls raw gait parameters"},
		}},
	}
	set := &ipcv1.InterfaceSchemaBundleSet{Bundles: []*ipcv1.InterfaceSchemaBundle{bundle}}
	descriptorWire, err := proto.MarshalOptions{Deterministic: true}.Marshal(descriptor)
	if err != nil {
		t.Fatalf("marshal descriptor: %v", err)
	}
	schemaWire, err := proto.MarshalOptions{Deterministic: true}.Marshal(set)
	if err != nil {
		t.Fatalf("marshal schema set: %v", err)
	}

	artifacts, err := ParseProviderArtifacts(descriptorWire, schemaWire)
	if err != nil {
		t.Fatalf("ParseProviderArtifacts: %v", err)
	}
	if artifacts.Descriptor.GetPackageId() != packageID || artifacts.Schemas.Len() != 1 {
		t.Fatalf("artifacts = %+v", artifacts)
	}
	schema, ok := artifacts.Schemas.Lookup(interfaceID, 1)
	if !ok || !bytes.Equal(schema.Hash(), bundle.GetSchemaHash()) {
		t.Fatal("validated schema not indexed by interface and major")
	}

	descriptor.Permissions = nil
	if err := ValidateProviderArtifacts(descriptor, artifacts.Schemas); err == nil ||
		!strings.Contains(err.Error(), "undefined custom permission") {
		t.Fatalf("missing custom permission error = %v", err)
	}
	descriptor.Permissions = artifacts.Descriptor.GetPermissions()
	descriptor.Interfaces[0].RequiredPermission = "com.evil.permission.camera"
	if err := ValidateProviderArtifacts(descriptor, artifacts.Schemas); err == nil ||
		!strings.Contains(err.Error(), "neither platform-owned nor under package namespace") {
		t.Fatalf("foreign permission error = %v", err)
	}
	descriptor.Interfaces[0].RequiredPermission = permission
	descriptor.Interfaces[0].DefaultResourceRole = "base.missing"
	if err := ValidateProviderArtifacts(descriptor, artifacts.Schemas); err == nil ||
		!strings.Contains(err.Error(), "is not managed") {
		t.Fatalf("missing default resource error = %v", err)
	}
}

func TestValidateMethodMetaRequiresBoundedTransfer(t *testing.T) {
	meta := &ipcv1.MethodMeta{
		MethodId:  1,
		RiskClass: ipcv1.RiskClass_RISK_CLASS_PRIVACY_SENSITIVE,
		Transfer: &ipcv1.TransferPolicy{
			Direction:         ipcv1.TransferDirection_TRANSFER_DIRECTION_PROVIDER_TO_CALLER,
			MaxStreams:        1,
			MaxPacketBytes:    1 << 20,
			MaxBytesPerSecond: 8 << 20,
			AllowedModes:      []ipcv1.TransferMode{ipcv1.TransferMode_TRANSFER_MODE_FRAMED_RELAY},
		},
	}
	if err := validateMethodMeta(protoregistry.GlobalFiles, meta); err != nil {
		t.Fatalf("valid transfer metadata rejected: %v", err)
	}
	meta.Transfer.MaxBytesPerSecond = 0
	if err := validateMethodMeta(protoregistry.GlobalFiles, meta); err == nil ||
		!strings.Contains(err.Error(), "zero max_bytes_per_second") {
		t.Fatalf("unbounded transfer error = %v", err)
	}
}

func TestValidateMethodMetaChecksErrorDetailType(t *testing.T) {
	meta := &ipcv1.MethodMeta{
		MethodId:        1,
		RiskClass:       ipcv1.RiskClass_RISK_CLASS_NORMAL,
		ErrorDetailType: "nervus.interface.pkgmanager.v1.PackageManagerErrorDetail",
	}
	if err := validateMethodMeta(protoregistry.GlobalFiles, meta); err != nil {
		t.Fatalf("known error detail type rejected: %v", err)
	}
	meta.ErrorDetailType = "nervus.interface.pkgmanager.v1.MissingErrorDetail"
	if err := validateMethodMeta(protoregistry.GlobalFiles, meta); err == nil ||
		!strings.Contains(err.Error(), "error detail type") {
		t.Fatalf("missing error detail type error = %v", err)
	}
}
