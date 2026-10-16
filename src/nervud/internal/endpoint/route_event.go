package endpoint

import (
	ipcv1 "github.com/nervus-os/nervus-ipc/protocol/ipcv1"

	"github.com/nervus-os/nervud/internal/catalog"
)

// EventRoute is the result of one subscription admission.
//
// The difference from RouteInfo is the target: a call is forwarded to a Provider, while a subscription is only registered,
// so what this gives is "the event source's coordinates", for subscription.Registry to index.
type EventRoute struct {
	// ProviderConn / ProviderEndpointID are the event source's coordinates. When the Provider reports
	// PublishEvent it uses its own endpoint_id, and the two must match.
	ProviderConn       ConnHandle
	ProviderEndpointID uint64

	InterfaceID    string
	InterfaceMajor uint32

	// Event carries the authoritative EventMeta: delivery class, rate limit, permission.
	Event catalog.EventDefinition

	// When Admit is non-nil, this subscription must first pass its instance-scope decision (see
	// BuiltinSubscribeAdmitter). Only built-in endpoints provide it.
	Admit BuiltinSubscribeAdmitter
}

// RouteEvent checks one subscription and gives the event source's coordinates.
//
// The admission chain has exactly the same source as Route (method calls), reusing the same invariants one by one: the binding is still alive,
// the generation hasn't drifted, the resource still matches, interface-level permission passes. The only difference is that the last step checks the event instead of the method.
//
// Checking permission once at subscription doesn't mean permanent rights: the subscriber's authorisation may be revoked afterwards.
// That path is triggered by permission's revoker through cleanups like CloseProviderEndpoint,
// and isn't this function's job; this only guarantees "it was legal at the moment it was set up".
func (m *Module) RouteEvent(
	conn ConnHandle,
	endpointID uint64,
	eventID uint32,
) (EventRoute, RouteError) {
	if m == nil {
		return EventRoute{}, RouteError{Code: ipcv1.StatusCode_STATUS_CODE_NOT_FOUND}
	}
	snapshot := m.snapshot()
	if snapshot == nil {
		return EventRoute{}, RouteError{Code: ipcv1.StatusCode_STATUS_CODE_FAILED_PRECONDITION}
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	cs, ok := m.byConn[conn]
	if !ok {
		return EventRoute{}, RouteError{Code: ipcv1.StatusCode_STATUS_CODE_NOT_FOUND}
	}
	b, ok := cs.bindings[endpointID]
	if !ok || b.target == nil || !b.target.live ||
		b.targetGeneration != b.target.generation {
		return EventRoute{}, RouteError{Code: ipcv1.StatusCode_STATUS_CODE_NOT_FOUND}
	}
	def, provider, valid := registrationInSnapshot(snapshot, b.target)
	if !valid ||
		def.DefinitionGeneration != b.definitionGeneration ||
		provider.DefinitionGeneration != b.providerGeneration ||
		b.interfaceID != b.target.interfaceID ||
		b.interfaceMajor != b.target.ifaceMajor {
		return EventRoute{}, RouteError{Code: ipcv1.StatusCode_STATUS_CODE_NOT_FOUND}
	}

	// Interface-level bar, the same as Resolve/Route
	if !m.allowedAt(snapshot, b.callerPackageID, def.RequiredPermission) {
		return EventRoute{}, RouteError{Code: ipcv1.StatusCode_STATUS_CODE_PERMISSION_DENIED}
	}

	event, ok := snapshot.ProviderEvent(
		b.target.packageID, b.interfaceID, b.interfaceMajor, eventID)
	if !ok || event.Meta == nil ||
		event.DefinitionGeneration != b.definitionGeneration ||
		event.ProviderGeneration != b.providerGeneration {
		// event_ids not declared in the contract are always refused. A Provider can't push an event outside the contract,
		// and a subscriber can't subscribe to one
		return EventRoute{}, RouteError{Code: ipcv1.StatusCode_STATUS_CODE_NOT_FOUND}
	}
	// The event-level bar stacks on the interface level: an interface may let anyone read its state
	// but only let privileged parties subscribe to raw telemetry
	if !m.allowedAt(snapshot, b.callerPackageID, event.Meta.GetRequiredPermission()) {
		return EventRoute{}, RouteError{Code: ipcv1.StatusCode_STATUS_CODE_PERMISSION_DENIED}
	}

	return EventRoute{
		ProviderConn:       b.target.conn,
		ProviderEndpointID: b.target.id,
		InterfaceID:        b.interfaceID,
		InterfaceMajor:     b.interfaceMajor,
		Event:              event,
		// Only built-ins can have admission. External registrations' subscribeAdmit is always nil,
		// and their events are endpoint-scoped.
		Admit: b.target.subscribeAdmit,
	}, RouteError{}
}

// LookupProviderEvent checks one PublishEvent: does this connection really own the endpoint,
// and was the event_id declared in the contract.
//
// It looks up by the Provider's own endpoint handle: a Provider can't push events for another endpoint;
// the endpoint_id it gives must be the one it registered itself.
func (m *Module) LookupProviderEvent(
	conn ConnHandle,
	serviceEndpointID uint64,
	eventID uint32,
) (catalog.EventDefinition, RouteError) {
	if m == nil {
		return catalog.EventDefinition{}, RouteError{Code: ipcv1.StatusCode_STATUS_CODE_NOT_FOUND}
	}
	snapshot := m.snapshot()
	if snapshot == nil {
		return catalog.EventDefinition{}, RouteError{
			Code: ipcv1.StatusCode_STATUS_CODE_FAILED_PRECONDITION,
		}
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	cs, ok := m.byConn[conn]
	if !ok {
		return catalog.EventDefinition{}, RouteError{Code: ipcv1.StatusCode_STATUS_CODE_NOT_FOUND}
	}
	reg, ok := cs.registrations[serviceEndpointID]
	if !ok || !reg.live {
		return catalog.EventDefinition{}, RouteError{Code: ipcv1.StatusCode_STATUS_CODE_NOT_FOUND}
	}
	event, ok := snapshot.ProviderEvent(
		reg.packageID, reg.interfaceID, reg.ifaceMajor, eventID)
	if !ok || event.Meta == nil {
		return catalog.EventDefinition{}, RouteError{Code: ipcv1.StatusCode_STATUS_CODE_NOT_FOUND}
	}
	return event, RouteError{}
}

// OwnsEndpoint answers "does this connection own this registration".
//
// BindEventScope goes through it: without this check, any system service could register instance ownership for
// another endpoint, and so insert itself into someone else's event stream.
//
// It looks only at registrations, not bindings: ownership is registered by the provider, using the handle from its own
// RegisterEndpoint, which is a different namespace from the caller side's bindings.
func (m *Module) OwnsEndpoint(conn ConnHandle, serviceEndpointID uint64) bool {
	if m == nil {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	cs, ok := m.byConn[conn]
	if !ok {
		return false
	}
	reg, ok := cs.registrations[serviceEndpointID]
	return ok && reg.live
}
