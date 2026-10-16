// This file plugs permission into the kernel.Module lifecycle
package permission

import "context"

// Module plugs permission into the kernel.Module lifecycle.
//
// The Registry's full state is pushed by pkgregistry after install / uninstall / the boot scan
//
// (Registry.Replace); the Module doesn't pull it in Start. That's the same way
//
// identity.Registry receives pkgregistry's projection (see projectIdentity in
// pkgregistry/module.go). So the Module itself has no initial state to
// set up: main.go builds the Registry during wiring and shares it with both
// pkgregistry and permission. Start/Stop only bring permission formally into
// the Kernel's start/stop sequence, for later extensions (such as Fatal reporting).
//
// It's registered after pkgregistry (its Grant projection comes from pkgregistry's push)
// and before ipc (ipc relies on it for permission decisions before dispatching a Request).
type Module struct {
	registry *Registry
}

// New constructs the permission Module.
//
// The caller builds registry explicitly with NewRegistry(cat) during wiring and passes it in, rather than this
// function creating it internally: pkgregistry has to share the same *Registry with the Module to push
// Install/Scan projections into it, and the two can't each hold a copy the other doesn't know about.
func New(registry *Registry) *Module {
	return &Module{registry: registry}
}

func (m *Module) Name() string { return "permission" }

// Start has no initialisation to do: the Registry is usable from construction (empty grant set,
// Allowed refuses everything), and the first real data is pushed in by pkgregistry's own Start.
func (m *Module) Start(_ context.Context) error { return nil }

// Stop: purely in-memory state, nothing to release
func (m *Module) Stop(_ context.Context) error { return nil }
