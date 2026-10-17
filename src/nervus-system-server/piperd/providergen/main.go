// Package main is piperd's providergen: it produces the package's Provider contract
// (provider.binpb + schemas.binpb) for sysmanifest to put into the digests and the manifest.
//
// # It must match the kernel's definition exactly
//
// nervud's catalog bootstrap defines nervus.interface.manipulator.arm@1 itself, bound to the
// resource type nervus.resource.manipulator.arm with arm.main as its default. When piperd's package
// brings the same interface, the catalog Builder compares the two with sameInterfaceContract, and
// any field that differs fails the whole catalog with "interface ... conflicts with definition
// owned by ...". So every field below replicates bootstrapInterface(InterfaceManipulatorArm, ...)
// in nervud's internal/catalog/bootstrap.go; they aren't settings.
//
// The resource arm.main is declared again, identically: a provider's descriptor must manage every
// resource type its interfaces bind (the registry refuses "unmanaged resource type"), and the catalog
// accepts a second declaration only when it matches the kernel's field for field (sameResourceContract),
// making piperd the resource's manager.
// No Permissions: perm.manipulator.control is a platform permission the kernel defines.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	manipulatorv1 "github.com/nervus-os/nervus-ipc/protocol/interface/manipulatorv1"
	"github.com/nervus-os/nervus-ipc/protocol/ipcv1"
	ipcregistry "github.com/nervus-os/nervus-ipc/registry"
)

const (
	packageID          = "nervus.piperd"
	interfaceID        = "nervus.interface.manipulator.arm"
	major              = 1
	requiredPermission = "perm.manipulator.control"
	resourceType       = "nervus.resource.manipulator.arm"
	defaultRole        = "arm.main"

	descriptorFileName = "provider.binpb"
	schemasFileName    = "schemas.binpb"
)

func main() {
	out := flag.String("out", "", "output directory (provider.binpb and schemas.binpb are written there)")
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
	descriptorWire, schemaWire, err := buildArtifacts()
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(out, descriptorFileName), descriptorWire, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", descriptorFileName, err)
	}
	if err := os.WriteFile(filepath.Join(out, schemasFileName), schemaWire, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", schemasFileName, err)
	}
	fmt.Printf("✅ %s provider contract\n", packageID)
	fmt.Printf("   %-16s %d bytes\n", descriptorFileName, len(descriptorWire))
	fmt.Printf("   %-16s %d bytes\n", schemasFileName, len(schemaWire))
	return nil
}

func buildArtifacts() (descriptorWire, schemaWire []byte, err error) {
	bundle, err := ipcregistry.BuildSchemaBundle(
		interfaceID, major, manipulatorv1.ManipulatorMethod(0).Descriptor())
	if err != nil {
		return nil, nil, fmt.Errorf("build schema bundle: %w", err)
	}
	descriptor := &ipcv1.ProviderDescriptor{
		PackageId: packageID,
		Interfaces: []*ipcv1.ProvidedInterface{{
			InterfaceId: interfaceID,
			InterfaceVersions: []*ipcv1.ProvidedInterfaceVersion{{
				Major:      major,
				SchemaHash: append([]byte(nil), bundle.GetSchemaHash()...),
			}},
			RequiredPermission:      requiredPermission,
			ResourceRiskFloor:       ipcv1.RiskClass_RISK_CLASS_PHYSICAL_CONTROL,
			CompatibleResourceTypes: []string{resourceType},
			DefaultResourceType:     resourceType,
			DefaultResourceRole:     defaultRole,
		}},
		Resources: []*ipcv1.ManagedResource{{
			StableRole:   defaultRole,
			ResourceType: resourceType,
			AccessMode:   ipcv1.ResourceAccessMode_RESOURCE_ACCESS_MODE_EXCLUSIVE_CONTROL,
			RiskClass:    ipcv1.RiskClass_RISK_CLASS_PHYSICAL_CONTROL,
		}},
	}
	set := &ipcv1.InterfaceSchemaBundleSet{Bundles: []*ipcv1.InterfaceSchemaBundle{bundle}}
	return ipcregistry.MarshalProviderArtifacts(descriptor, set)
}
