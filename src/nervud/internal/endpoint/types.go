// This file is the package's core data model: the two independent endpoint_id namespaces
// (service-side registration / caller-side binding) must be generated and stored separately;
// the same number on two connections means nothing in common
package endpoint

import (
	ipcv1 "github.com/nervus-os/nervus-ipc/protocol/ipcv1"

	"github.com/nervus-os/nervud/internal/catalog"
	"github.com/nervus-os/nervud/internal/pkgregistry"
)

// ConnHandle is the opaque identity of an IPC connection, satisfied by internal/ipc's *conn.
//
// This package doesn't know or need to know conn's concrete shape, avoiding a reverse dependency on ipc; it only needs to
// be a comparable map key that uniquely identifies a connection.
// "The same number on another connection isn't the same binding" relies on exactly this pointer identity,
// not on the number itself
type ConnHandle interface{}

// RouteInfo is the forwarding target handed to ipc after a successful Route lookup.
//
// route_id allocation, Dispatch/DispatchResult correlation and execution Lane scheduling are outside this package;
// ipc handles them itself once it has the RouteInfo
type RouteInfo struct {
	TargetConn          ConnHandle
	ServiceEndpointID   uint64
	ProviderPackageID   string
	ProviderComponentID string
	InterfaceID         string
	InterfaceMajor      uint32
	// InterfaceMinor / InterfaceSchemaHash are the type binding: callers use them to resume
	// decoding typed payloads. Long tasks need it most: after an SDK disconnects and reconnects it only has an
	// operation_id, and without this binding it can't decode progress or results.
	InterfaceMinor      uint32
	InterfaceSchemaHash []byte
	ResourceHandle      string

	Method                 catalog.MethodDefinition
	RegistrationGeneration uint64
	DefinitionGeneration   uint64
	ProviderGeneration     uint64
	ResourceGeneration     uint64
	RequiredPermissions    []string

	// When Builtin is non-nil, the target is a built-in endpoint implemented by nervud itself (see builtin.go):
	// the caller should run it in place rather than forward a Dispatch to some Service connection.
	//
	// Mutually exclusive with TargetConn: when Builtin is non-nil, TargetConn is always nil (built-ins have no connection).
	// Callers check Builtin before TargetConn; the other order would treat a built-in as "routed successfully but
	// with no forwarding target" and return UNAVAILABLE.
	Builtin BuiltinHandler
}

// RouteError is the result of a failed Route lookup.
//
// A zero Code (StatusCode_STATUS_CODE_UNSPECIFIED) means it didn't fail, and callers test for that,
// following the existing ipcv1.Failure convention for zero codes
type RouteError struct {
	Code ipcv1.StatusCode
	// Reason is a short phrase for the kernel log, saying exactly which step this failure got stuck at.
	//
	// # Why it's needed
	//
	// Route has nearly ten places that return NOT_FOUND (unknown connection, missing binding, target not live,
	// generation drift, resource mismatch, method not in the Catalog...), and on the wire they look identical:
	// the caller only sees STATUS_CODE_NOT_FOUND. Route also used to log nothing at all, so
	// "the call failed" had no clue pointing at the real cause; you could only read the code and guess, and a wrong guess
	// cost a whole round of compile, flash and reproduce.
	//
	// It doesn't go on the wire; it's only written to the kernel log. These phrases describe kernel-internal state (generation numbers, the binding
	// table), which would neither help the caller act nor avoid leaking implementation details.
	Reason string
}

// componentKey uniquely identifies a component instance, for the on-demand start wait queue
// (the same key shape as internal/service's)
type componentKey struct {
	pkg  string
	comp string
}

// registrationKey uniquely identifies a (Package, Component, Interface) triple,
// used to generate increasing generation numbers (for bindings to check staleness; see serviceRegistration)
type registrationKey struct {
	pkg   string
	comp  string
	iface string
}

// serviceRegistration is one Interface implementation a Service reported through
// RegisterEndpoint
type serviceRegistration struct {
	id             uint64 // service-side namespace, RegisterEndpointSuccess.endpoint_id
	conn           ConnHandle
	packageID      string
	componentID    string
	interfaceID    string
	ifaceMajor     uint32
	ifaceMinor     uint32
	schemaHash     []byte
	resourceHandle string // always "base.main" or empty in v1
	visibility     pkgregistry.Visibility
	generation     uint64 // incremented each time the same (pkg,comp,interface) registers again

	definitionGeneration uint64
	providerGeneration   uint64
	resourceGeneration   uint64

	// When builtin is non-nil, this is a built-in endpoint implemented by nervud itself (see builtin.go):
	// no conn, always live, and Route returns it rather than a forwarding target.
	builtin BuiltinHandler

	// When subscribeAdmit is non-nil, this built-in interface's events need instance-scoped admission
	//  (see BuiltinSubscribeAdmitter). Always nil for external registrations.
	subscribeAdmit BuiltinSubscribeAdmitter

	// live false means the registration has lapsed (connection dropped / UnregisterEndpoint).
	// Bindings referring to it are judged invalid at their next Route; that's where v1's passive invalidation lands.
	// Active EndpointDied pushes depend on an upgrade of ipc's write path,
	// which this package doesn't depend on yet
	live bool
}

// binding is the routing handle one ResolveEndpoint creates on a caller connection
type binding struct {
	id                 uint64 // caller-side namespace, ResolveEndpointSuccess.endpoint_id
	conn               ConnHandle
	callerPackageID    string
	target             *serviceRegistration
	targetGeneration   uint64 // snapshot of serviceRegistration.generation at creation, for diagnostics
	interfaceID        string
	interfaceMajor     uint32
	interfaceMinor     uint32
	requiredPermission string // from the central catalog; empty means no extra interface-level decision
	resourceHandle     string

	definitionGeneration uint64
	providerGeneration   uint64
	resourceGeneration   uint64
}

// connState is all the registrations and bindings under one connection.
//
// The service-side endpoint_id (nextRegID) and caller-side endpoint_id (nextEndpointID)
// are two independent namespaces; in principle one connection can both register and resolve
type connState struct {
	nextEndpointID uint64
	nextRegID      uint64
	bindings       map[uint64]*binding
	registrations  map[uint64]*serviceRegistration
}

func newConnState() *connState {
	return &connState{
		bindings:      make(map[uint64]*binding),
		registrations: make(map[uint64]*serviceRegistration),
	}
}
