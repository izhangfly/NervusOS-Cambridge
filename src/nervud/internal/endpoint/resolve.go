// This file implements ResolveEndpoint: a Caller resolves an interface_id into this connection's scoped
// endpoint_id, with the permission decision done before binding
package endpoint

import (
	"bytes"
	"fmt"

	ipcv1 "github.com/nervus-os/nervus-ipc/protocol/ipcv1"
	"google.golang.org/protobuf/proto"

	"github.com/nervus-os/nervud/internal/catalog"
	"github.com/nervus-os/nervud/internal/identity"
)

// ResolveEndpoint handles one Caller's discovery and returns a connection-scoped binding
func (m *Module) ResolveEndpoint(conn ConnHandle, caller identity.Caller, req *ipcv1.ResolveEndpoint) *ipcv1.ResolveEndpointResult {
	reqID := req.GetRequestId()
	if m == nil {
		return resolveFailure(reqID, ipcv1.StatusCode_STATUS_CODE_FAILED_PRECONDITION,
			ipcv1.ResolveEndpointReason_RESOLVE_ENDPOINT_REASON_UNSPECIFIED)
	}

	interfaceID := req.GetInterfaceId()
	snapshot := m.snapshot()
	if snapshot == nil {
		m.audit(caller, "endpoint.ResolveEndpoint", true, errInterfaceNotFound, interfaceID)
		return resolveFailure(reqID, ipcv1.StatusCode_STATUS_CODE_FAILED_PRECONDITION,
			ipcv1.ResolveEndpointReason_RESOLVE_ENDPOINT_REASON_INTERFACE_NOT_FOUND)
	}

	// explicit_component is always PERMISSION_DENIED in v1, because
	// the manifest has no field yet to declare "bind a specific Component", so a non-empty one can never find a matching
	// declaration. Refusing directly is the right behaviour, not technical debt
	if req.GetExplicitComponent() != "" {
		m.audit(caller, "endpoint.ResolveEndpoint", true, errExplicitComponent, interfaceID)
		return resolveFailure(reqID, ipcv1.StatusCode_STATUS_CODE_PERMISSION_DENIED,
			ipcv1.ResolveEndpointReason_RESOLVE_ENDPOINT_REASON_UNSPECIFIED)
	}

	if req.GetMaxInterfaceMajor() < req.GetMinInterfaceMajor() {
		m.audit(caller, "endpoint.ResolveEndpoint", true, errVersionMismatch, interfaceID)
		return resolveFailure(reqID, ipcv1.StatusCode_STATUS_CODE_FAILED_PRECONDITION,
			ipcv1.ResolveEndpointReason_RESOLVE_ENDPOINT_REASON_VERSION_MISMATCH)
	}

	attemptedStart := false
	for {
		m.mu.Lock()
		visible := m.visibleCandidatesLocked(caller.PackageID, interfaceID)
		var (
			candidates       []resolvedCandidate
			versionCandidate bool
			resourceMismatch bool
		)
		for _, reg := range visible {
			if reg.ifaceMajor < req.GetMinInterfaceMajor() ||
				reg.ifaceMajor > req.GetMaxInterfaceMajor() {
				continue
			}
			versionCandidate = true
			def, provider, valid := registrationInSnapshot(snapshot, reg)
			if !valid {
				continue
			}
			resourceHandle, resourceGeneration, valid :=
				resolveInterfaceResource(snapshot, def, req.GetSelector())
			if !valid || resourceHandle != reg.resourceHandle ||
				resourceGeneration != reg.resourceGeneration {
				resourceMismatch = true
				continue
			}
			candidates = append(candidates, resolvedCandidate{
				registration:       reg,
				definition:         def,
				provider:           provider,
				resourceHandle:     resourceHandle,
				resourceGeneration: resourceGeneration,
			})
		}

		switch len(candidates) {
		case 1:
			candidate := candidates[0]
			cand := candidate.registration
			requiredPermission := candidate.definition.RequiredPermission

			// Step 5: query the current authorisation before binding, so Resolve doesn't rely only on the install-time decision
			if !m.allowedAt(snapshot, caller.PackageID, requiredPermission) {
				m.mu.Unlock()
				m.audit(caller, "endpoint.ResolveEndpoint", true, errPermissionDenied, requiredPermission)
				return resolveFailure(reqID, ipcv1.StatusCode_STATUS_CODE_PERMISSION_DENIED,
					ipcv1.ResolveEndpointReason_RESOLVE_ENDPOINT_REASON_UNSPECIFIED)
			}

			// Step 7: allocate this connection's nextEndpointID and create the binding
			cs := m.connStateLocked(conn)
			cs.nextEndpointID++
			id := cs.nextEndpointID
			cs.bindings[id] = &binding{
				id:                   id,
				conn:                 conn,
				callerPackageID:      caller.PackageID,
				target:               cand,
				targetGeneration:     cand.generation,
				interfaceID:          interfaceID,
				interfaceMajor:       cand.ifaceMajor,
				interfaceMinor:       cand.ifaceMinor,
				requiredPermission:   requiredPermission,
				resourceHandle:       candidate.resourceHandle,
				definitionGeneration: candidate.definition.DefinitionGeneration,
				providerGeneration:   candidate.provider.DefinitionGeneration,
				resourceGeneration:   candidate.resourceGeneration,
			}
			m.mu.Unlock()

			m.audit(caller, "endpoint.ResolveEndpoint", false, nil, interfaceID)
			return &ipcv1.ResolveEndpointResult{RequestId: reqID, Outcome: &ipcv1.ResolveEndpointResult_Success{
				Success: &ipcv1.ResolveEndpointSuccess{
					EndpointId:          id,
					InterfaceMajor:      cand.ifaceMajor,
					InterfaceMinor:      cand.ifaceMinor,
					InterfaceSchemaHash: append([]byte(nil), candidate.definition.SchemaHash...),
					ResourceHandle:      candidate.resourceHandle,
					CatalogRevision:     snapshot.Revision(),
				},
			}}

		case 0:
			m.mu.Unlock()

			if len(visible) != 0 {
				if !versionCandidate {
					m.audit(caller, "endpoint.ResolveEndpoint", true, errVersionMismatch, interfaceID)
					return resolveFailure(reqID, ipcv1.StatusCode_STATUS_CODE_FAILED_PRECONDITION,
						ipcv1.ResolveEndpointReason_RESOLVE_ENDPOINT_REASON_VERSION_MISMATCH)
				}
				if resourceMismatch {
					m.audit(caller, "endpoint.ResolveEndpoint", true, errResourceNotFound, interfaceID)
					return resolveFailure(reqID, ipcv1.StatusCode_STATUS_CODE_FAILED_PRECONDITION,
						ipcv1.ResolveEndpointReason_RESOLVE_ENDPOINT_REASON_RESOURCE_NOT_FOUND)
				}
				m.audit(caller, "endpoint.ResolveEndpoint", true, errInterfaceNotFound, interfaceID)
				return resolveFailure(reqID, ipcv1.StatusCode_STATUS_CODE_FAILED_PRECONDITION,
					ipcv1.ResolveEndpointReason_RESOLVE_ENDPOINT_REASON_INTERFACE_NOT_FOUND)
			}

			// Step 6: try an on-demand start. A rejected or stale
			// registration may wake the waiter, but it never grants a second
			// unbounded start attempt.
			if attemptedStart {
				m.audit(caller, "endpoint.ResolveEndpoint", true, errInterfaceNotFound, interfaceID)
				return resolveFailure(reqID, ipcv1.StatusCode_STATUS_CODE_FAILED_PRECONDITION,
					ipcv1.ResolveEndpointReason_RESOLVE_ENDPOINT_REASON_INTERFACE_NOT_FOUND)
			}
			candidate, failure := m.authorizeOnDemandStart(snapshot, caller.PackageID, req)
			switch failure {
			case onDemandPermissionDenied:
				m.audit(caller, "endpoint.ResolveEndpoint", true, errPermissionDenied, interfaceID)
				return resolveFailure(reqID, ipcv1.StatusCode_STATUS_CODE_PERMISSION_DENIED,
					ipcv1.ResolveEndpointReason_RESOLVE_ENDPOINT_REASON_UNSPECIFIED)
			case onDemandVersionMismatch:
				m.audit(caller, "endpoint.ResolveEndpoint", true, errVersionMismatch, interfaceID)
				return resolveFailure(reqID, ipcv1.StatusCode_STATUS_CODE_FAILED_PRECONDITION,
					ipcv1.ResolveEndpointReason_RESOLVE_ENDPOINT_REASON_VERSION_MISMATCH)
			case onDemandResourceMismatch:
				m.audit(caller, "endpoint.ResolveEndpoint", true, errResourceNotFound, interfaceID)
				return resolveFailure(reqID, ipcv1.StatusCode_STATUS_CODE_FAILED_PRECONDITION,
					ipcv1.ResolveEndpointReason_RESOLVE_ENDPOINT_REASON_RESOURCE_NOT_FOUND)
			case onDemandAmbiguous:
				m.audit(caller, "endpoint.ResolveEndpoint", true, errAmbiguous, interfaceID)
				return resolveFailure(reqID, ipcv1.StatusCode_STATUS_CODE_FAILED_PRECONDITION,
					ipcv1.ResolveEndpointReason_RESOLVE_ENDPOINT_REASON_RESOURCE_AMBIGUOUS)
			case onDemandNotFound:
				m.audit(caller, "endpoint.ResolveEndpoint", true, errInterfaceNotFound, interfaceID)
				return resolveFailure(reqID, ipcv1.StatusCode_STATUS_CODE_FAILED_PRECONDITION,
					ipcv1.ResolveEndpointReason_RESOLVE_ENDPOINT_REASON_INTERFACE_NOT_FOUND)
			case onDemandAuthorized:
			}

			attemptedStart = true
			started, rejected, err := m.tryOnDemandStart(candidate.pkg, candidate.comp)
			if err != nil {
				// EnsureStarted failed (such as the component being disabled): map it unchanged to FAILED_PRECONDITION
				m.audit(caller, "endpoint.ResolveEndpoint", true, err, interfaceID)
				return resolveFailure(reqID, ipcv1.StatusCode_STATUS_CODE_FAILED_PRECONDITION,
					ipcv1.ResolveEndpointReason_RESOLVE_ENDPOINT_REASON_UNSPECIFIED)
			}
			if !started {
				// Getting here means the selector already resolved successfully in authorizeOnDemandStart
				// (otherwise it would get onDemandResourceMismatch), so this is NOT a resource problem.
				// This used to return RESOURCE_NOT_FOUND, so a Provider refused when reporting in
				// (schema hash mismatch) sent the caller off to check its own selector, where
				// there's nothing to change.
				//
				// INTERFACE_NOT_FOUND is the truth: no endpoint registered this interface.
				// And UNAVAILABLE says it's about timing: the provider exists and was allowed to start,
				// it just couldn't get ready right now, so retrying makes sense.
				failure := error(errOnDemandTimeout)
				if rejected != "" {
					failure = fmt.Errorf("%w: %s/%s: %s",
						errOnDemandRejected, candidate.pkg, candidate.comp, rejected)
				}
				m.audit(caller, "endpoint.ResolveEndpoint", true, failure, interfaceID)
				return resolveFailure(reqID, ipcv1.StatusCode_STATUS_CODE_UNAVAILABLE,
					ipcv1.ResolveEndpointReason_RESOLVE_ENDPOINT_REASON_INTERFACE_NOT_FOUND)
			}

			// Woken: go back to checking candidates, without assuming "broadcast means success"
			continue

		default:
			// In theory this can't happen under v1's single-actuator model: if it does, it signals a wiring contradiction
			m.mu.Unlock()
			m.audit(caller, "endpoint.ResolveEndpoint", true, errAmbiguous, interfaceID)
			return resolveFailure(reqID, ipcv1.StatusCode_STATUS_CODE_FAILED_PRECONDITION,
				ipcv1.ResolveEndpointReason_RESOLVE_ENDPOINT_REASON_RESOURCE_AMBIGUOUS)
		}
	}
}

type resolvedCandidate struct {
	registration       *serviceRegistration
	definition         catalog.InterfaceDefinition
	provider           catalog.ProviderInterface
	resourceHandle     string
	resourceGeneration uint64
}

type onDemandFailure uint8

const (
	onDemandAuthorized onDemandFailure = iota
	onDemandNotFound
	onDemandVersionMismatch
	onDemandPermissionDenied
	onDemandResourceMismatch
	onDemandAmbiguous
)

// authorizeOnDemandStart proves the side effect itself is authorized. A
// manifest export alone is not enough: the exact package/component must be a
// catalog-approved Provider in the requested major range, the interface
// permission must already be granted, and the selector must resolve against
// that same immutable snapshot.
func (m *Module) authorizeOnDemandStart(
	snapshot *catalog.Snapshot,
	callerPackageID string,
	req *ipcv1.ResolveEndpoint,
) (componentKey, onDemandFailure) {
	if snapshot == nil || req == nil {
		return componentKey{}, onDemandNotFound
	}
	manifestCandidates := m.manifestOnDemandCandidates(
		callerPackageID, req.GetInterfaceId())
	if len(manifestCandidates) == 0 {
		return componentKey{}, onDemandNotFound
	}

	allMatch := false
	for _, provider := range snapshot.ProviderInterfaces(
		req.GetInterfaceId(), 0, ^uint32(0)) {
		key := componentKey{pkg: provider.PackageID, comp: provider.ComponentID}
		if _, ok := manifestCandidates[key]; ok {
			allMatch = true
			break
		}
	}
	if !allMatch {
		return componentKey{}, onDemandNotFound
	}

	rangeMatch := false
	permissionDenied := false
	resourceMismatch := false
	authorized := make(map[componentKey]struct{})
	var selected componentKey
	for _, provider := range snapshot.ProviderInterfaces(
		req.GetInterfaceId(), req.GetMinInterfaceMajor(), req.GetMaxInterfaceMajor()) {
		key := componentKey{pkg: provider.PackageID, comp: provider.ComponentID}
		if _, ok := manifestCandidates[key]; !ok {
			continue
		}
		rangeMatch = true
		if !m.allowedAt(snapshot, callerPackageID, provider.Definition.RequiredPermission) {
			permissionDenied = true
			continue
		}
		if _, _, ok := resolveInterfaceResource(
			snapshot, provider.Definition, req.GetSelector()); !ok {
			resourceMismatch = true
			continue
		}
		if _, duplicate := authorized[key]; !duplicate {
			authorized[key] = struct{}{}
			selected = key
		}
	}

	switch {
	case len(authorized) > 1:
		return componentKey{}, onDemandAmbiguous
	case len(authorized) == 1:
		return selected, onDemandAuthorized
	case !rangeMatch:
		return componentKey{}, onDemandVersionMismatch
	case permissionDenied:
		return componentKey{}, onDemandPermissionDenied
	case resourceMismatch:
		return componentKey{}, onDemandResourceMismatch
	default:
		return componentKey{}, onDemandNotFound
	}
}

func registrationInSnapshot(
	snapshot *catalog.Snapshot,
	reg *serviceRegistration,
) (catalog.InterfaceDefinition, catalog.ProviderInterface, bool) {
	if snapshot == nil || reg == nil || !reg.live {
		return catalog.InterfaceDefinition{}, catalog.ProviderInterface{}, false
	}
	def, ok := snapshot.Interface(reg.interfaceID, reg.ifaceMajor)
	if !ok || def.DefinitionGeneration != reg.definitionGeneration ||
		!bytes.Equal(def.SchemaHash, reg.schemaHash) {
		return catalog.InterfaceDefinition{}, catalog.ProviderInterface{}, false
	}
	provider, ok := snapshot.ProviderInterface(
		reg.packageID, reg.interfaceID, reg.ifaceMajor)
	if !ok || provider.ComponentID != reg.componentID ||
		provider.DefinitionGeneration != reg.providerGeneration ||
		provider.Definition.DefinitionGeneration != reg.definitionGeneration {
		return catalog.InterfaceDefinition{}, catalog.ProviderInterface{}, false
	}
	return def, provider, true
}

// resolveInterfaceResource applies the interface's catalog-owned default and
// compatibility set. Interfaces without resources resolve to the empty handle;
// they cannot be rebound to an arbitrary resource.
func resolveInterfaceResource(
	snapshot *catalog.Snapshot,
	def catalog.InterfaceDefinition,
	sel *ipcv1.ResourceSelector,
) (string, uint64, bool) {
	if snapshot == nil {
		return "", 0, false
	}
	// An empty selector falls back to the default resource the interface itself declares.
	//
	// That's not the same as v1's implicit default: v1 had the kernel hard-code {motion.base, main},
	// so any interface left empty got the base; here the default is declared by the interface's ProviderDescriptor,
	// and no declaration means no default and nothing resolves. The semantics come from the interface's data, not hard-coded in the kernel.
	if catalog.SelectorIsEmpty(sel) {
		resourceType, role := def.DefaultResourceType, def.DefaultResourceRole
		if resourceType == "" && role == "" {
			// Interfaces that bind no resource: empty is correct. Interfaces that bind a resource but declare no default: a selector must be given explicitly
			return "", 0, len(def.CompatibleResourceTypes) == 0
		}
		if len(def.CompatibleResourceTypes) == 0 {
			return "", 0, false
		}
		resource, ok := snapshot.ResolveResource(resourceType, role)
		if !ok || !compatibleResource(def, resource.ResourceType) {
			return "", 0, false
		}
		return resource.Handle, resource.DefinitionGeneration, true
	}

	// A non-empty selector goes through full selection: exact type / role match + labels AND filter +
	// the multi-candidate policy (unspecified fails closed as REQUIRE_UNIQUE)
	if len(def.CompatibleResourceTypes) == 0 {
		return "", 0, false
	}
	matched, ok := catalog.SelectResources(snapshot, sel)
	if !ok {
		// Zero matches, or several when the policy requires uniqueness. Both are things the caller must change,
		// expressed uniformly as "can't resolve"; ResolveEndpoint returns FAILED_PRECONDITION
		return "", 0, false
	}
	resource := matched[0]
	if !compatibleResource(def, resource.ResourceType) {
		return "", 0, false
	}
	return resource.Handle, resource.DefinitionGeneration, true
}

// resolveFailure builds a failure result carrying a ResolveEndpointErrorDetail.
// When reason is UNSPECIFIED, ErrorDetail is omitted (PERMISSION_DENIED, for example, doesn't need it)
func resolveFailure(reqID uint64, code ipcv1.StatusCode, reason ipcv1.ResolveEndpointReason) *ipcv1.ResolveEndpointResult {
	f := &ipcv1.Failure{Code: code}
	if reason != ipcv1.ResolveEndpointReason_RESOLVE_ENDPOINT_REASON_UNSPECIFIED {
		if b, err := proto.Marshal(&ipcv1.ResolveEndpointErrorDetail{Reason: reason}); err == nil {
			f.ErrorDetail = b
		}
	}
	return &ipcv1.ResolveEndpointResult{RequestId: reqID, Outcome: &ipcv1.ResolveEndpointResult_Failure{Failure: f}}
}
