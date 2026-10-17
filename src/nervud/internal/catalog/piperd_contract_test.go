package catalog

import (
	"testing"

	manipulatorv1 "github.com/nervus-os/nervus-ipc/protocol/interface/manipulatorv1"
	ipcv1 "github.com/nervus-os/nervus-ipc/protocol/ipcv1"
	ipcregistry "github.com/nervus-os/nervus-ipc/registry"

	"github.com/nervus-os/nervud/internal/identity"
)

// piperdArtifacts mirrors nervus-system-server/piperd/providergen: Manipulator@1 bound to the
// kernel's arm.main, with that resource declared again identically so piperd becomes its manager.
func piperdArtifacts(t *testing.T) *ipcregistry.ProviderArtifacts {
	t.Helper()
	bundle, err := ipcregistry.BuildSchemaBundle(
		InterfaceManipulatorArm, 1, manipulatorv1.ManipulatorMethod(0).Descriptor())
	if err != nil {
		t.Fatalf("BuildSchemaBundle: %v", err)
	}
	descriptor := &ipcv1.ProviderDescriptor{
		PackageId: "nervus.piperd",
		Interfaces: []*ipcv1.ProvidedInterface{{
			InterfaceId: InterfaceManipulatorArm,
			InterfaceVersions: []*ipcv1.ProvidedInterfaceVersion{{
				Major: 1, SchemaHash: append([]byte(nil), bundle.GetSchemaHash()...),
			}},
			RequiredPermission:      "perm.manipulator.control",
			ResourceRiskFloor:       ipcv1.RiskClass_RISK_CLASS_PHYSICAL_CONTROL,
			CompatibleResourceTypes: []string{ResourceManipulatorArm},
			DefaultResourceType:     ResourceManipulatorArm,
			DefaultResourceRole:     "arm.main",
		}},
		Resources: []*ipcv1.ManagedResource{{
			StableRole:   "arm.main",
			ResourceType: ResourceManipulatorArm,
			AccessMode:   ipcv1.ResourceAccessMode_RESOURCE_ACCESS_MODE_EXCLUSIVE_CONTROL,
			RiskClass:    ipcv1.RiskClass_RISK_CLASS_PHYSICAL_CONTROL,
		}},
	}
	descriptorWire, schemaWire, err := ipcregistry.MarshalProviderArtifacts(
		descriptor, &ipcv1.InterfaceSchemaBundleSet{Bundles: []*ipcv1.InterfaceSchemaBundle{bundle}})
	if err != nil {
		t.Fatalf("MarshalProviderArtifacts: %v", err)
	}
	artifacts, err := ipcregistry.ParseProviderArtifacts(descriptorWire, schemaWire)
	if err != nil {
		t.Fatalf("ParseProviderArtifacts: %v", err)
	}
	return artifacts
}

func TestPiperdArtifactsMatchBootstrapContract(t *testing.T) {
	registry := mustDefaultRegistry(t)
	candidate, err := registry.Prepare([]Source{{
		PackageID: "nervus.piperd",
		Kind:      SourceKindSystemImage,
		Trust:     identity.TrustPlatform,
		Signers: SignerEvidence{
			Roles: []string{rolePlatformRelease},
			VerifiedSigners: []VerifiedSigner{{
				Role: rolePlatformRelease, KeyID: "platform-release-key",
			}},
		},
		Exports:   []ExportBinding{{ComponentID: "main", InterfaceID: InterfaceManipulatorArm}},
		Artifacts: piperdArtifacts(t),
	}})
	if err != nil {
		t.Fatalf("Prepare with piperd's artifacts: %v", err)
	}
	snap := candidate.Snapshot()

	servo := uint32(manipulatorv1.ManipulatorMethod_MANIPULATOR_METHOD_SERVO_TO_POSE)
	method, ok := snap.ProviderMethod("nervus.piperd", InterfaceManipulatorArm, 1, servo)
	if !ok || method.Request == nil ||
		string(method.Request.FullName()) != "nervus.interface.manipulator.v1.ServoToPoseRequest" {
		t.Fatalf("ServoToPose = %+v, %v", method, ok)
	}
	// Teleoperation must be possible for an ordinary app: a lease, but no confirmation dialog.
	if !method.Meta.GetRequiresControlLease() || method.Meta.GetNeedsUserConfirmation() || method.Meta.GetReturnsOperation() {
		t.Fatalf("ServoToPose meta = %+v; want lease-gated, no confirmation, not an operation", method.Meta)
	}
	res, ok := snap.ResourceByHandle("arm.main")
	if !ok || res.ManagerPackageID != "nervus.piperd" {
		t.Fatalf("arm.main = %+v, %v; want managed by nervus.piperd", res, ok)
	}
}
