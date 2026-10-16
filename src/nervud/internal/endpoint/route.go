// This file implements Route, UnregisterEndpoint and lifecycle invalidation when a connection closes
package endpoint

import (
	ipcv1 "github.com/nervus-os/nervus-ipc/protocol/ipcv1"
)

// Route is used by ipc's handleRequest: given an endpoint_id, one lookup answers "who to forward to,
// and is this call still legal"
//
// Every call re-checks permissions (rather than trusting only the one check at Resolve). That's the literal
// requirement "check permissions once when resolving an endpoint, and still do a quick permission and liveness
// re-check on every call, to support dynamic revocation", and it's why even purely passive checks guarantee "the next call
// after a permission is revoked must fail"
func (m *Module) Route(
	conn ConnHandle,
	endpointID uint64,
	methodID uint32,
) (RouteInfo, RouteError) {
	if m == nil {
		return RouteInfo{}, RouteError{
			Code: ipcv1.StatusCode_STATUS_CODE_NOT_FOUND, Reason: "endpoint module is nil"}
	}
	snapshot := m.snapshot()
	if snapshot == nil {
		return RouteInfo{}, RouteError{
			Code: ipcv1.StatusCode_STATUS_CODE_FAILED_PRECONDITION, Reason: "catalog snapshot unavailable"}
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	cs, ok := m.byConn[conn]
	if !ok {
		return RouteInfo{}, RouteError{
			Code: ipcv1.StatusCode_STATUS_CODE_NOT_FOUND, Reason: "caller connection has no endpoint state"}
	}
	b, ok := cs.bindings[endpointID]
	if !ok {
		return RouteInfo{}, RouteError{
			Code: ipcv1.StatusCode_STATUS_CODE_NOT_FOUND, Reason: "endpoint_id not bound on this connection"}
	}
	if b.target == nil || !b.target.live {
		// The Provider's registration has lapsed (process exited / connection dropped), while the caller still holds the endpoint_id
		// from its last Resolve. For on-demand components that's bound to happen after they exit when idle.
		return RouteInfo{}, RouteError{
			Code: ipcv1.StatusCode_STATUS_CODE_NOT_FOUND, Reason: "provider registration is no longer live"}
	}
	if b.targetGeneration != b.target.generation {
		// The Provider restarted: the same triple registered again and the generation number went up. The old binding must lapse,
		// or calls would hit the new process while the caller thinks it's still the old one.
		return RouteInfo{}, RouteError{
			Code:   ipcv1.StatusCode_STATUS_CODE_NOT_FOUND,
			Reason: "provider restarted (registration generation moved)"}
	}
	def, provider, valid := registrationInSnapshot(snapshot, b.target)
	if !valid {
		return RouteInfo{}, RouteError{
			Code: ipcv1.StatusCode_STATUS_CODE_NOT_FOUND, Reason: "provider registration not in current catalog"}
	}
	if def.DefinitionGeneration != b.definitionGeneration ||
		provider.DefinitionGeneration != b.providerGeneration {
		// The Catalog has a new revision (installs/uninstalls rebuild it), and the definition generation at binding differs from now.
		return RouteInfo{}, RouteError{
			Code:   ipcv1.StatusCode_STATUS_CODE_NOT_FOUND,
			Reason: "catalog definition generation moved since bind"}
	}
	if b.interfaceID != b.target.interfaceID ||
		b.interfaceMajor != b.target.ifaceMajor {
		return RouteInfo{}, RouteError{
			Code: ipcv1.StatusCode_STATUS_CODE_NOT_FOUND, Reason: "binding interface does not match registration"}
	}
	if b.resourceHandle == "" {
		if b.resourceGeneration != 0 || len(def.CompatibleResourceTypes) != 0 {
			return RouteInfo{}, RouteError{
				Code:   ipcv1.StatusCode_STATUS_CODE_NOT_FOUND,
				Reason: "interface needs a resource but binding has none"}
		}
	} else {
		resource, exists := snapshot.ResourceByHandle(b.resourceHandle)
		if !exists ||
			resource.DefinitionGeneration != b.resourceGeneration ||
			!compatibleResource(def, resource.ResourceType) ||
			b.target.resourceHandle != b.resourceHandle {
			return RouteInfo{}, RouteError{
				Code: ipcv1.StatusCode_STATUS_CODE_NOT_FOUND, Reason: "bound resource is gone or incompatible"}
		}
	}

	if !m.allowedAt(snapshot, b.callerPackageID, def.RequiredPermission) {
		return RouteInfo{}, RouteError{
			Code:   ipcv1.StatusCode_STATUS_CODE_PERMISSION_DENIED,
			Reason: "caller lacks interface permission " + def.RequiredPermission}
	}

	method, ok := snapshot.ProviderMethod(
		b.target.packageID, b.interfaceID, b.interfaceMajor, methodID)
	if !ok || method.Meta == nil {
		// This Provider's interface has no such method_id: the caller got the number wrong, or
		// the Provider's schema isn't the same revision as the Catalog's.
		return RouteInfo{}, RouteError{
			Code:   ipcv1.StatusCode_STATUS_CODE_NOT_FOUND,
			Reason: "method_id not declared by this provider's interface"}
	}
	if method.DefinitionGeneration != b.definitionGeneration ||
		method.ProviderGeneration != b.providerGeneration {
		return RouteInfo{}, RouteError{
			Code:   ipcv1.StatusCode_STATUS_CODE_NOT_FOUND,
			Reason: "method generation moved since bind"}
	}
	methodPermission := method.Meta.GetRequiredPermission()
	if !m.allowedAt(snapshot, b.callerPackageID, methodPermission) {
		return RouteInfo{}, RouteError{
			Code:   ipcv1.StatusCode_STATUS_CODE_PERMISSION_DENIED,
			Reason: "caller lacks method permission " + methodPermission}
	}
	requiredPermissions := uniquePermissions(def.RequiredPermission, methodPermission)

	return RouteInfo{
		TargetConn:             b.target.conn,
		ServiceEndpointID:      b.target.id,
		ProviderPackageID:      b.target.packageID,
		ProviderComponentID:    b.target.componentID,
		InterfaceID:            b.interfaceID,
		InterfaceMajor:         b.interfaceMajor,
		InterfaceMinor:         b.target.ifaceMinor,
		InterfaceSchemaHash:    append([]byte(nil), b.target.schemaHash...),
		ResourceHandle:         b.resourceHandle,
		Method:                 method,
		RegistrationGeneration: b.targetGeneration,
		DefinitionGeneration:   b.definitionGeneration,
		ProviderGeneration:     b.providerGeneration,
		ResourceGeneration:     b.resourceGeneration,
		RequiredPermissions:    requiredPermissions,
		Builtin:                b.target.builtin,
	}, RouteError{}
}

func uniquePermissions(interfacePermission, methodPermission string) []string {
	switch {
	case interfacePermission == "" && methodPermission == "":
		return nil
	case interfacePermission == "":
		return []string{methodPermission}
	case methodPermission == "" || methodPermission == interfacePermission:
		return []string{interfacePermission}
	default:
		return []string{interfacePermission, methodPermission}
	}
}

// UnregisterEndpoint withdraws a Service-side registration.
//
// req.Drain isn't wired in v1: the execution layer doesn't track in-flight Dispatches, so drain=true degrades to
// immediate invalidation; v1 keeps no two-version registration that could wait for old calls to finish
func (m *Module) UnregisterEndpoint(conn ConnHandle, req *ipcv1.UnregisterEndpoint) *ipcv1.UnregisterEndpointResult {
	reqID := req.GetRequestId()
	if m == nil {
		return unregisterFailure(reqID, ipcv1.StatusCode_STATUS_CODE_FAILED_PRECONDITION)
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	cs, ok := m.byConn[conn]
	if !ok {
		return unregisterFailure(reqID, ipcv1.StatusCode_STATUS_CODE_NOT_FOUND)
	}
	reg, ok := cs.registrations[req.GetEndpointId()]
	if !ok {
		return unregisterFailure(reqID, ipcv1.StatusCode_STATUS_CODE_NOT_FOUND)
	}

	delete(cs.registrations, req.GetEndpointId())
	reg.live = false
	m.removeFromInterfaceIndexLocked(reg)

	return &ipcv1.UnregisterEndpointResult{RequestId: reqID, Outcome: &ipcv1.UnregisterEndpointResult_Success{
		Success: &ipcv1.UnregisterEndpointSuccess{},
	}}
}

func unregisterFailure(reqID uint64, code ipcv1.StatusCode) *ipcv1.UnregisterEndpointResult {
	return &ipcv1.UnregisterEndpointResult{RequestId: reqID, Outcome: &ipcv1.UnregisterEndpointResult_Failure{
		Failure: &ipcv1.Failure{Code: code},
	}}
}

// ConnClosed is called once by ipc when a connection's serve loop exits, cleaning up every
// registration/binding under that connection
//
//   - Service connection: all its serviceRegistrations are marked invalid and removed from byInterface,
//     and bindings referring to them are judged invalid on their next Route
//   - Caller connection: all its bindings are simply dropped, with nobody to notify: after a UDS disconnect
//     every endpoint is invalid, and a reconnect must Resolve again
func (m *Module) ConnClosed(conn ConnHandle) {
	if m == nil {
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	cs, ok := m.byConn[conn]
	if !ok {
		return
	}
	delete(m.byConn, conn)

	for _, reg := range cs.registrations {
		reg.live = false
		m.removeFromInterfaceIndexLocked(reg)
	}
}
