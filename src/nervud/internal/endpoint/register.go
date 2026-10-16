// This file implements RegisterEndpoint: a Service reports to nervud an Interface it has implemented
package endpoint

import (
	"bytes"
	"errors"

	ipcv1 "github.com/nervus-os/nervus-ipc/protocol/ipcv1"

	"github.com/nervus-os/nervud/internal/catalog"
	"github.com/nervus-os/nervud/internal/identity"
	"github.com/nervus-os/nervud/internal/pkgregistry"
)

// RegisterEndpoint handles one Service report
func (m *Module) RegisterEndpoint(conn ConnHandle, caller identity.Caller, req *ipcv1.RegisterEndpoint) *ipcv1.RegisterEndpointResult {
	reqID := req.GetRequestId()
	if m == nil {
		return registerFailure(reqID, ipcv1.StatusCode_STATUS_CODE_FAILED_PRECONDITION)
	}

	interfaceID := req.GetInterfaceId()

	fail := func(code ipcv1.StatusCode, detail string) *ipcv1.RegisterEndpointResult {
		m.audit(caller, "endpoint.RegisterEndpoint", true, errors.New(detail), interfaceID)
		// Also left for any Resolve waiting for this component to start. Without this step, that side could only report
		// "wait timed out", and a timeout doesn't distinguish "never came up" from "came up but was refused"
		m.recordRejection(caller.PackageID, caller.ComponentID, detail)
		return registerFailure(reqID, code)
	}

	// Step 1: the connection must have completed its handshake (caller.ComponentID non-empty; see ipc's component check)
	if caller.ComponentID == "" {
		return fail(ipcv1.StatusCode_STATUS_CODE_FAILED_PRECONDITION, "handshake not complete: no component id")
	}
	snapshot := m.snapshot()
	if snapshot == nil {
		return fail(ipcv1.StatusCode_STATUS_CODE_FAILED_PRECONDITION,
			"authoritative catalog is unavailable")
	}

	// Step 2: a Service may only register endpoints its manifest declared
	if m.pkgs == nil {
		return fail(ipcv1.StatusCode_STATUS_CODE_FAILED_PRECONDITION,
			"package registry is unavailable")
	}
	entry, ok := m.pkgs.Lookup(caller.PackageID)
	if !ok {
		return fail(ipcv1.StatusCode_STATUS_CODE_PERMISSION_DENIED, "unknown package")
	}
	comp, ok := entry.Manifest.Component(caller.ComponentID)
	if !ok {
		return fail(ipcv1.StatusCode_STATUS_CODE_PERMISSION_DENIED, "unknown component")
	}
	if entry.ComponentDisabled(caller.ComponentID) {
		return fail(ipcv1.StatusCode_STATUS_CODE_PERMISSION_DENIED, "component disabled")
	}
	// Apps can too: a component with a UI must be invokable by interface from other packages (the permission confirmation screen is invoked by Settings and
	// returns an answer). The authorisation criteria haven't changed at all; see ComponentType.CanProvideInterfaces.
	if !comp.Type.CanProvideInterfaces() {
		return fail(ipcv1.StatusCode_STATUS_CODE_PERMISSION_DENIED,
			"this component type may not register endpoints")
	}

	var (
		visibility pkgregistry.Visibility
		exported   bool
	)
	for _, e := range comp.Exports {
		if e.Interface == interfaceID {
			visibility = e.Visibility
			exported = true
			break
		}
	}
	if !exported {
		return fail(ipcv1.StatusCode_STATUS_CODE_PERMISSION_DENIED, "interface not declared in manifest exports")
	}

	// The signed catalog, not the wire request or manifest alone, decides which
	// package/component implements this exact interface major.
	provider, ok := snapshot.ProviderInterface(
		caller.PackageID, interfaceID, req.GetInterfaceMajor())
	if !ok ||
		provider.PackageID != caller.PackageID ||
		provider.ComponentID != caller.ComponentID ||
		provider.Definition.InterfaceID != interfaceID ||
		provider.Definition.Major != req.GetInterfaceMajor() {
		return fail(ipcv1.StatusCode_STATUS_CODE_PERMISSION_DENIED,
			"component is not a catalog-approved provider for the interface major")
	}

	// Step 3: choose the permission ID by the Export's Visibility and decide
	permID := permServiceRegister
	if visibility == pkgregistry.VisibilityPackage {
		permID = permServiceRegisterPrivate
	}
	if !m.allowedAt(snapshot, caller.PackageID, permID) {
		return fail(ipcv1.StatusCode_STATUS_CODE_PERMISSION_DENIED, "missing permission "+permID)
	}

	resourceHandle := req.GetResourceHandle()
	resourceGeneration, resourceOK := registrationResource(
		snapshot, provider.Definition, resourceHandle)
	if !resourceOK {
		return fail(ipcv1.StatusCode_STATUS_CODE_INVALID_ARGUMENT,
			"resource_handle is absent, unknown, or incompatible with the interface")
	}

	// The schema hash must equal the Catalog's copy byte for byte. There used to be a compatibility bridge that let only
	// nervus.pkgmanagerd through with an empty schema; it was removed once the packaging chain could produce ProviderArtifacts,
	// and now every Provider is treated the same.
	reportedSchema := req.GetInterfaceSchemaHash()
	if !bytes.Equal(reportedSchema, provider.Definition.SchemaHash) {
		return fail(ipcv1.StatusCode_STATUS_CODE_FAILED_PRECONDITION,
			"interface schema hash does not match the catalog")
	}
	schemaHash := append([]byte(nil), provider.Definition.SchemaHash...)

	m.mu.Lock()

	// Step 6: allocate the conn's own nextRegID and register the serviceRegistration
	regKey := registrationKey{pkg: caller.PackageID, comp: caller.ComponentID, iface: interfaceID}
	m.generations[regKey]++

	cs := m.connStateLocked(conn)
	cs.nextRegID++
	reg := &serviceRegistration{
		id:                   cs.nextRegID,
		conn:                 conn,
		packageID:            caller.PackageID,
		componentID:          caller.ComponentID,
		interfaceID:          interfaceID,
		ifaceMajor:           req.GetInterfaceMajor(),
		ifaceMinor:           req.GetInterfaceMinor(),
		schemaHash:           schemaHash,
		resourceHandle:       resourceHandle,
		visibility:           visibility,
		generation:           m.generations[regKey],
		definitionGeneration: provider.Definition.DefinitionGeneration,
		providerGeneration:   provider.DefinitionGeneration,
		resourceGeneration:   resourceGeneration,
		live:                 true,
	}
	cs.registrations[reg.id] = reg
	m.byInterface[interfaceID] = append(m.byInterface[interfaceID], reg)

	// Step 7: if a Resolve is waiting for this (pkg, comp) to finish starting, broadcast to wake it
	key := componentKey{pkg: caller.PackageID, comp: caller.ComponentID}
	waiters := m.pendingStarts[key]
	delete(m.pendingStarts, key)

	m.mu.Unlock()

	for _, ch := range waiters {
		close(ch)
	}

	m.audit(caller, "endpoint.RegisterEndpoint", false, nil, interfaceID)
	return &ipcv1.RegisterEndpointResult{RequestId: reqID, Outcome: &ipcv1.RegisterEndpointResult_Success{
		Success: &ipcv1.RegisterEndpointSuccess{EndpointId: reg.id},
	}}
}

func registrationResource(
	snapshot *catalog.Snapshot,
	def catalog.InterfaceDefinition,
	handle string,
) (uint64, bool) {
	if len(def.CompatibleResourceTypes) == 0 {
		return 0, handle == ""
	}
	if handle == "" {
		return 0, false
	}
	resource, ok := snapshot.ResourceByHandle(handle)
	if !ok || !compatibleResource(def, resource.ResourceType) {
		return 0, false
	}
	return resource.DefinitionGeneration, true
}

func compatibleResource(def catalog.InterfaceDefinition, resourceType string) bool {
	for _, compatible := range def.CompatibleResourceTypes {
		if compatible == resourceType {
			return true
		}
	}
	return false
}

func registerFailure(reqID uint64, code ipcv1.StatusCode) *ipcv1.RegisterEndpointResult {
	return &ipcv1.RegisterEndpointResult{RequestId: reqID, Outcome: &ipcv1.RegisterEndpointResult_Failure{
		Failure: &ipcv1.Failure{Code: code},
	}}
}
