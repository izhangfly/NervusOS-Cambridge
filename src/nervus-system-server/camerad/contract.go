//go:build linux

// This file holds the service's contract constants and its schema hashes.
//
// # Why the hash is [computed] rather than passed in at build time
//
// RegisterEndpoint carries a schema_hash, and nervud compares it byte for byte with the one registered in the Catalog
// and refuses on a mismatch. Both values come from the same generated code:
//
//	providergen   BuildSchemaBundle(...) → written into ProviderArtifacts → into the Catalog
//	this service  BuildSchemaBundle(...) → sent when registering
//
// Writing the hash as a constant or reading it from a file would add one more copy that can drift from the generated code; after drifting,
// the service's registration is refused, and the error only says "schema hash mismatch", not who is out of date.
//
// Now both sides compute it from camerav1's enum descriptors, tied to the same proto at compile time.
package main

import (
	"fmt"

	camerav1 "github.com/nervus-os/nervus-ipc/protocol/interface/camerav1"
	ipcregistry "github.com/nervus-os/nervus-ipc/registry"
)

// The service's identity and interfaces. componentID must match components[].id in manifest.json:
// nervud checks it via the peer's PID → cgroup → systemd unit and refuses the handshake on a mismatch.
const (
	packageID   = "nervus.camerad"
	componentID = "main"

	interfaceCamera       = "nervus.interface.camera"
	interfaceCameraConfig = "nervus.interface.camera.config"

	interfaceMajor = 1
)

// Platform resource type and permissions. Must match what providergen declares.
const (
	resourceCamera       = "nervus.resource.camera"
	resourceCameraConfig = "nervus.resource.camera.config"

	permissionCapture   = "perm.camera.capture"
	permissionConfigure = "perm.camera.configure"
)

// schemaHashes computes the schema hashes of the two interfaces.
func schemaHashes() (capture, config []byte, err error) {
	// It uses [the same constructor and the same enums] as providergen. The event enum doesn't affect
	// schema_hash (it's in the same file as the method enum, so it's in that descriptor set anyway),
	// but keeping both places consistent stops someone changing one side later and forgetting the other.
	captureBundle, err := ipcregistry.BuildSchemaBundleWithEvents(
		interfaceCamera, interfaceMajor,
		camerav1.CameraMethod(0).Descriptor(),
		camerav1.CameraEvent(0).Descriptor())
	if err != nil {
		return nil, nil, fmt.Errorf("camerad: build %s schema bundle: %w", interfaceCamera, err)
	}
	configBundle, err := ipcregistry.BuildSchemaBundleWithEvents(
		interfaceCameraConfig, interfaceMajor,
		camerav1.CameraConfigMethod(0).Descriptor(),
		camerav1.CameraConfigEvent(0).Descriptor())
	if err != nil {
		return nil, nil, fmt.Errorf("camerad: build %s schema bundle: %w", interfaceCameraConfig, err)
	}
	return captureBundle.GetSchemaHash(), configBundle.GetSchemaHash(), nil
}
