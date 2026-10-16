// This file holds the service's contract constants and its schema hash.
//
// # Why the hash is [computed] rather than a constant
//
// RegisterEndpoint carries a schema_hash, and nervud compares it byte for byte with the one registered in the Catalog
// and refuses on a mismatch. For this interface the Catalog copy has two sources, both using exactly the same algorithm:
//
//	nervud   bootstrap.go buildBootstrapArtifacts → BuildSchemaBundle(...) → Catalog
//	providergen                                   → BuildSchemaBundle(...) → provider.binpb
//	this service                                  → BuildSchemaBundle(...) → sent when registering
//
// Writing the hash as a constant or reading it from a file would add one more copy that can drift from the generated code; after drifting,
// the service's registration is refused, and the error only says "schema hash mismatch", not who is out of date.
//
// # The hash only depends on the descriptor set
//
// BuildSchemaBundle takes the sha256 of the FileDescriptorSet; interfaceID and major don't
// take part. So even if the constants here and in providergen disagreed, it wouldn't show up as a hash mismatch; that
// error surfaces as a sameInterfaceContract conflict when the Catalog is built. What really has to
// match is that [all three repositories pin the same nervus-ipc commit]: if the descriptor changes, the hash changes.
package main

import (
	"fmt"

	pkgv1 "github.com/nervus-os/nervus-ipc/protocol/interface/pkgmanagerv1"
	ipcregistry "github.com/nervus-os/nervus-ipc/registry"
)

// interfaceMajor must match the major providergen declares and the interface the manifest exports.
const interfaceMajor = 1

// schemaHash computes this interface's schema hash.
//
// It uses [the same constructor and the same method enum] as providergen; that's the only guarantee
// the two don't drift, and either side switching to a different enum shows up at once as a refused registration.
func schemaHash() ([]byte, error) {
	bundle, err := ipcregistry.BuildSchemaBundle(
		interfaceID, interfaceMajor, pkgv1.PackageManagerMethod(0).Descriptor())
	if err != nil {
		return nil, fmt.Errorf("pkgmanagerd: build %s schema bundle: %w", interfaceID, err)
	}
	return bundle.GetSchemaHash(), nil
}
