// Package main is pkgmanagerd's providergen: it produces this service's data-driven Provider contract
// (provider.binpb + schemas.binpb), for sysmanifest to include in the digests and write into the manifest.
//
// # Why each service has its own, instead of the packager generating them all
//
// Building a schema bundle needs to import the interface's [generated code]. If sysmanifest did it for everyone,
// it would have to import every interface's generated package, turning "add a capability" into "change the packaging tool",
// exactly the coupling the data-driven Catalog is meant to remove. nervus-ipc's BuildSchemaBundle comment
// says "packagers just use this function"; the job belongs on the service side.
//
// # How it relates to the kernel bootstrap
//
// nervud's catalog bootstrap also defines nervus.interface.pkg.manager@1. When the same
// interface already exists in the Catalog, the Builder compares them field by field with sameInterfaceContract,
// so [the contract this file produces must match the kernel's exactly]. Any one field differing makes the whole
// Catalog build fail with "interface ... conflicts with definition owned by ...".
//
// So requiredPermission / the risk class / the resource fields below aren't configuration to change on a whim;
// they replicate that bootstrapInterface call in nervud's internal/catalog/bootstrap.go.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	pkgv1 "github.com/nervus-os/nervus-ipc/protocol/interface/pkgmanagerv1"
	"github.com/nervus-os/nervus-ipc/protocol/ipcv1"
	ipcregistry "github.com/nervus-os/nervus-ipc/registry"
)

const (
	packageID   = "nervus.pkgmanagerd"
	interfaceID = "nervus.interface.pkg.manager"
	major       = 1

	// requiredPermission must match nervud's catalog bootstrap declaration for the same interface.
	// On the kernel side it is bootstrapInterface(InterfacePackageManager, ..., "perm.pkg.query", ...)
	requiredPermission = "perm.pkg.query"

	// File names must match the provider section of manifest.json.in
	descriptorFileName = "provider.binpb"
	schemasFileName    = "schemas.binpb"
)

func main() {
	out := flag.String("out", "", "output directory (provider.binpb and schemas.binpb are written there)")
	flag.Parse()

	if err := run(*out); err != nil {
		_, err := fmt.Fprintf(os.Stderr, "providergen(%s): %v\n", packageID, err)
		if err != nil {
			return
		}
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

	// 0o644: these two files are covered by digests and ship read-only in the image; no executable bit needed
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

// buildArtifacts is split out so tests can assert the contract's shape without writing to disk.
func buildArtifacts() (descriptorWire, schemaWire []byte, err error) {
	// The method enum comes from the generated code: method_id and MethodMeta hang off its enum values,
	// and the cost of copying it locally isn't duplication, it's that the copy quietly goes stale
	bundle, err := ipcregistry.BuildSchemaBundle(
		interfaceID, major, pkgv1.PackageManagerMethod(0).Descriptor())
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
			RequiredPermission: requiredPermission,
			// The install interface isn't bound to a physical resource: the risk class stays UNSPECIFIED and the resource fields stay empty.
			// These three are also compared by sameInterfaceContract, and match the kernel bootstrap
		}},
		// No Resources: installing occupies no ManagedResource
		// No Permissions: perm.pkg.query / perm.pkg.install are platform permissions
		// defined by nervud's bootstrap. A package that isn't platform-release declaring perm.* is refused by
		// authorizePermissionNamespace; this package is platform-release, but defining the same permission
		// again would conflict with the kernel's copy
	}

	set := &ipcv1.InterfaceSchemaBundleSet{
		Bundles: []*ipcv1.InterfaceSchemaBundle{bundle},
	}
	return ipcregistry.MarshalProviderArtifacts(descriptor, set)
}
