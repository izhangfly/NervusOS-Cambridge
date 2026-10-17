package main

import (
	"fmt"

	manipulatorv1 "github.com/nervus-os/nervus-ipc/protocol/interface/manipulatorv1"
	ipcregistry "github.com/nervus-os/nervus-ipc/registry"
)

// schemaHash is what RegisterEndpoint must report. It's built from the generated method enum, the
// same way nervud's catalog bootstrap and providergen build it, so the three can't disagree.
func schemaHash() ([]byte, error) {
	bundle, err := ipcregistry.BuildSchemaBundle(
		interfaceID, interfaceMajor, manipulatorv1.ManipulatorMethod(0).Descriptor())
	if err != nil {
		return nil, fmt.Errorf("piperd: build %s schema bundle: %w", interfaceID, err)
	}
	return bundle.GetSchemaHash(), nil
}
