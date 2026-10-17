// Package main generates the deterministic Nervus Provider contract for the
// Gazebo simulation system service.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/nervus-os/nervus-gazebo-provider/internal/contract"
	"github.com/nervus-os/nervus-ipc/protocol/ipcv1"
	ipcregistry "github.com/nervus-os/nervus-ipc/registry"
)

const (
	packageID = contract.PackageID

	managerInterface = contract.ManagerInterface
	controlInterface = contract.ControlInterface
	baseInterface    = contract.BaseInterface
	armInterface     = contract.ManipulatorInterface
	interfaceMajor   = contract.InterfaceMajor

	worldResourceType = contract.WorldResourceType
	worldResourceRole = contract.WorldResourceRole
	worldKindLabel    = contract.WorldKindLabel
	baseResourceType  = contract.BaseResourceType
	armResourceType   = contract.ArmResourceType
	baseResourceRole  = contract.BaseResourceRole
	leftArmRole       = contract.LeftArmResourceRole
	rightArmRole      = contract.RightArmResourceRole

	queryPermission     = contract.QueryPermission
	lifecyclePermission = contract.LifecyclePermission
	controlPermission   = contract.ControlPermission

	descriptorFileName = "provider.binpb"
	schemasFileName    = "schemas.binpb"
)

func main() {
	out := flag.String("out", "", "output directory for provider.binpb and schemas.binpb")
	flag.Parse()

	if err := run(*out); err != nil {
		fmt.Fprintf(os.Stderr, "providergen(%s): %v\n", packageID, err)
		os.Exit(1)
	}
}

func run(out string) error {
	if out == "" {
		return fmt.Errorf("-out is required")
	}

	descriptorWire, schemaWire, err := BuildArtifacts()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		return fmt.Errorf("create output directory: %w", err)
	}
	if err := os.WriteFile(filepath.Join(out, descriptorFileName), descriptorWire, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", descriptorFileName, err)
	}
	if err := os.WriteFile(filepath.Join(out, schemasFileName), schemaWire, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", schemasFileName, err)
	}

	fmt.Printf("generated %s Provider contract\n", packageID)
	fmt.Printf("  %-16s %d bytes\n", descriptorFileName, len(descriptorWire))
	fmt.Printf("  %-16s %d bytes\n", schemasFileName, len(schemaWire))
	return nil
}

// BuildArtifacts derives method IDs, event IDs, message types and schema hashes
// from generated protobuf descriptors. Nothing in this function duplicates the
// method or event number tables from the .proto files.
func BuildArtifacts() (descriptorWire, schemaWire []byte, err error) {
	managerBundle, controlBundle, err := contract.SchemaBundles()
	if err != nil {
		return nil, nil, err
	}
	baseBundle, armBundle, err := contract.StandardSchemaBundles()
	if err != nil {
		return nil, nil, err
	}

	descriptor := &ipcv1.ProviderDescriptor{
		PackageId: packageID,
		Interfaces: []*ipcv1.ProvidedInterface{
			{
				InterfaceId: managerInterface,
				InterfaceVersions: []*ipcv1.ProvidedInterfaceVersion{{
					Major:      interfaceMajor,
					SchemaHash: append([]byte(nil), managerBundle.GetSchemaHash()...),
				}},
				// Query is the Resolve gate. Lifecycle methods add their own
				// stronger permission at method dispatch.
				RequiredPermission: queryPermission,
			},
			{
				InterfaceId: controlInterface,
				InterfaceVersions: []*ipcv1.ProvidedInterfaceVersion{{
					Major:      interfaceMajor,
					SchemaHash: append([]byte(nil), controlBundle.GetSchemaHash()...),
				}},
				// Query-only callers may resolve the endpoint and subscribe to
				// state. Mutations still require controlPermission and a Lease.
				RequiredPermission:      queryPermission,
				ResourceRiskFloor:       ipcv1.RiskClass_RISK_CLASS_NORMAL,
				CompatibleResourceTypes: []string{worldResourceType},
				DefaultResourceType:     worldResourceType,
				DefaultResourceRole:     worldResourceRole,
			},
			{
				InterfaceId: baseInterface,
				InterfaceVersions: []*ipcv1.ProvidedInterfaceVersion{{
					Major:      interfaceMajor,
					SchemaHash: append([]byte(nil), baseBundle.GetSchemaHash()...),
				}},
				RequiredPermission:      "perm.motion.control",
				ResourceRiskFloor:       ipcv1.RiskClass_RISK_CLASS_PHYSICAL_CONTROL,
				CompatibleResourceTypes: []string{baseResourceType},
				DefaultResourceType:     baseResourceType,
				DefaultResourceRole:     baseResourceRole,
			},
			{
				InterfaceId: armInterface,
				InterfaceVersions: []*ipcv1.ProvidedInterfaceVersion{{
					Major:      interfaceMajor,
					SchemaHash: append([]byte(nil), armBundle.GetSchemaHash()...),
				}},
				RequiredPermission:      "perm.manipulator.control",
				ResourceRiskFloor:       ipcv1.RiskClass_RISK_CLASS_PHYSICAL_CONTROL,
				CompatibleResourceTypes: []string{armResourceType},
			},
		},
		Resources: []*ipcv1.ManagedResource{
			{
				StableRole:   worldResourceRole,
				ResourceType: worldResourceType,
				AccessMode:   ipcv1.ResourceAccessMode_RESOURCE_ACCESS_MODE_EXCLUSIVE_CONTROL,
				RiskClass:    ipcv1.RiskClass_RISK_CLASS_NORMAL,
				Labels: map[string]string{
					worldKindLabel: "world",
				},
			},
			{
				StableRole:   baseResourceRole,
				ResourceType: baseResourceType,
				AccessMode:   ipcv1.ResourceAccessMode_RESOURCE_ACCESS_MODE_EXCLUSIVE_CONTROL,
				RiskClass:    ipcv1.RiskClass_RISK_CLASS_PHYSICAL_CONTROL,
				Labels:       map[string]string{worldKindLabel: "base"},
			},
			{
				StableRole:   leftArmRole,
				ResourceType: armResourceType,
				AccessMode:   ipcv1.ResourceAccessMode_RESOURCE_ACCESS_MODE_EXCLUSIVE_CONTROL,
				RiskClass:    ipcv1.RiskClass_RISK_CLASS_PHYSICAL_CONTROL,
				Labels:       map[string]string{worldKindLabel: "arm"},
			},
			{
				StableRole:   rightArmRole,
				ResourceType: armResourceType,
				AccessMode:   ipcv1.ResourceAccessMode_RESOURCE_ACCESS_MODE_EXCLUSIVE_CONTROL,
				RiskClass:    ipcv1.RiskClass_RISK_CLASS_PHYSICAL_CONTROL,
				Labels:       map[string]string{worldKindLabel: "arm"},
			},
		},
		Permissions: permissions(),
	}

	set := &ipcv1.InterfaceSchemaBundleSet{
		Bundles: []*ipcv1.InterfaceSchemaBundle{
			managerBundle, controlBundle, baseBundle, armBundle,
		},
	}
	return ipcregistry.MarshalProviderArtifacts(descriptor, set)
}

func permissions() []*ipcv1.DefinedPermission {
	return []*ipcv1.DefinedPermission{
		{
			Id:           queryPermission,
			GrantMode:    ipcv1.GrantMode_GRANT_MODE_NORMAL,
			RiskClass:    ipcv1.RiskClass_RISK_CLASS_NORMAL,
			MinimumTrust: ipcv1.PermissionTrustFloor_PERMISSION_TRUST_FLOOR_ORDINARY,
			Group:        "simulation",
			DisplayName: &ipcv1.LocalizedText{
				En:   "View Gazebo simulation",
			},
			Description: &ipcv1.LocalizedText{
				En:   "View simulation status, models, joints, and capabilities",
			},
		},
		{
			Id:           lifecyclePermission,
			GrantMode:    ipcv1.GrantMode_GRANT_MODE_USER_CONSENT,
			RiskClass:    ipcv1.RiskClass_RISK_CLASS_NORMAL,
			MinimumTrust: ipcv1.PermissionTrustFloor_PERMISSION_TRUST_FLOOR_ORDINARY,
			Group:        "simulation",
			DisplayName: &ipcv1.LocalizedText{
				En:   "Manage Gazebo simulation",
			},
			Description: &ipcv1.LocalizedText{
				En:   "Start and stop simulation and load allowed worlds",
			},
		},
		{
			Id:           controlPermission,
			GrantMode:    ipcv1.GrantMode_GRANT_MODE_USER_CONSENT,
			RiskClass:    ipcv1.RiskClass_RISK_CLASS_NORMAL,
			MinimumTrust: ipcv1.PermissionTrustFloor_PERMISSION_TRUST_FLOOR_ORDINARY,
			Group:        "simulation",
			DisplayName: &ipcv1.LocalizedText{
				En:   "Control Gazebo simulation",
			},
			Description: &ipcv1.LocalizedText{
				En:   "Pause, step, or reset simulation and control virtual robot joints",
			},
		},
	}
}
