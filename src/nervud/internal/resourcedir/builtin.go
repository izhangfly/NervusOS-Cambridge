// This package wraps "which resources this machine has" as a built-in endpoint, for nervud to register
// during setup (endpoint.RegisterBuiltin). Read-only.
//
// # It fills a real gap
//
// ResourceSelector can answer "connect me to that front camera", but not "how many cameras does this machine
// actually have". Without the latter, an App could only probe with repeated Resolves until it hit NOT_FOUND,
// which is slow and unreliable: a failed Resolve can't tell "there's no such device" from
// "there is one but I'm not allowed".
//
// # Why the kernel implements it rather than some system service
//
// The resource directory is the Catalog itself. Having a service answer would mean it keeps a copy of the
// Catalog, and there's always a window in which copy and original can drift: the install has taken effect, the copy
// hasn't refreshed, and the App gets a stale device list. This package reads catalog.Registry's
// current Snapshot directly, so that window doesn't exist.
//
// That doesn't contradict "the kernel shouldn't open back doors for particular capabilities": the directory isn't a capability, it's the
// Catalog's read interface. This package knows nothing about cameras or bases.
//
// # Boundary: only return what a successful Resolve would already reveal
//
// handle / type / role / access mode / risk class / labels. No device node paths,
// serial numbers or Provider PIDs. This boundary is hard: the directory is readable by every App holding
// perm.resource.query, and adding a "might as well return this too" field would make that field
// public to the whole system.
package resourcedir

import (
	"log/slog"

	resourcedirv1 "github.com/nervus-os/nervus-ipc/protocol/interface/resourcedirv1"
	ipcv1 "github.com/nervus-os/nervus-ipc/protocol/ipcv1"
	"google.golang.org/protobuf/proto"

	"github.com/nervus-os/nervud/internal/catalog"
	"github.com/nervus-os/nervud/internal/endpoint"
)

// BuiltinInterfaceID is this built-in interface's ID.
//
// It must match that bootstrapInterface call in the catalog bootstrap, which sets the threshold.
const BuiltinInterfaceID = catalog.InterfaceResourceDirectory

// MethodListResources comes from the generated enum value.
//
// Not a locally copied literal 1: a copy doesn't just duplicate, it quietly goes stale.
// If the proto renumbers it, this keeps the old value and calls get routed to a method that doesn't exist.
// Same approach as safety (power has to be hand-written only because it has no .proto yet; see the note there).
const MethodListResources = uint32(
	resourcedirv1.ResourceDirectoryMethod_RESOURCE_DIRECTORY_METHOD_LIST_RESOURCES)

// Module holds the minimal dependencies for directory queries.
type Module struct {
	definitions *catalog.Registry
	log         *slog.Logger
}

// New constructs a Module. With nil definitions the handler always fails closed with UNAVAILABLE.
func New(definitions *catalog.Registry, log *slog.Logger) *Module {
	return &Module{definitions: definitions, log: log}
}

// BuiltinHandler returns a handler that can be passed straight to endpoint.RegisterBuiltin.
func (m *Module) BuiltinHandler() endpoint.BuiltinHandler {
	return func(call endpoint.BuiltinCall) endpoint.BuiltinResult {
		switch call.MethodID {
		case MethodListResources:
			return m.listResources(call.Payload)
		default:
			// Fail closed: an unimplemented method is a method that doesn't exist. Returning an empty list would make the caller
			// think this machine has no resources at all.
			return endpoint.BuiltinResult{Code: ipcv1.StatusCode_STATUS_CODE_NOT_FOUND}
		}
	}
}

func (m *Module) listResources(payload []byte) endpoint.BuiltinResult {
	if m == nil || m.definitions == nil {
		return endpoint.BuiltinResult{Code: ipcv1.StatusCode_STATUS_CODE_UNAVAILABLE}
	}

	req := &resourcedirv1.ListResourcesRequest{}
	if err := proto.Unmarshal(payload, req); err != nil {
		// An undecodable request is the caller's fault, not the kernel's. Reply INVALID_ARGUMENT rather than
		// treating it as an empty request and listing everything, which would turn an encoding error into a full disclosure.
		return endpoint.BuiltinResult{Code: ipcv1.StatusCode_STATUS_CODE_INVALID_ARGUMENT}
	}

	// Read once, so the whole response comes from one Snapshot. Reading twice could mix content from
	// two Catalog revisions into the response, producing unreproducible results like "listed a resource
	// that was just uninstalled".
	matched := catalog.FilterResources(
		m.definitions.Current(), req.GetResourceType(), req.GetStableRole(), req.GetLabels())

	out := &resourcedirv1.ResourceList{
		Resources: make([]*resourcedirv1.ResourceEntry, 0, len(matched)),
	}
	for _, def := range matched {
		out.Resources = append(out.Resources, entryOf(def))
	}

	wire, err := proto.Marshal(out)
	if err != nil {
		if m.log != nil {
			m.log.Warn("resourcedir: marshal response failed", "err", err)
		}
		return endpoint.BuiltinResult{Code: ipcv1.StatusCode_STATUS_CODE_INTERNAL}
	}
	return endpoint.BuiltinResult{Payload: wire, Code: ipcv1.StatusCode_STATUS_CODE_OK}
}

// entryOf projects a Catalog resource definition onto a public entry on the wire.
//
// Fields are listed explicitly rather than copied wholesale: when ResourceDefinition grows internal fields one day
//
//	(Provider PID, device node), explicit listing means new fields don't leak by default, while
//
// reflective copying means they do. The default direction decides what happens when someone forgets.
func entryOf(def catalog.ResourceDefinition) *resourcedirv1.ResourceEntry {
	entry := &resourcedirv1.ResourceEntry{
		Handle:       def.Handle,
		ResourceType: def.ResourceType,
		StableRole:   def.StableRole,
		AccessMode:   def.AccessMode,
		RiskClass:    def.RiskClass,
	}
	if len(def.Labels) > 0 {
		entry.Labels = make(map[string]string, len(def.Labels))
		for key, value := range def.Labels {
			entry.Labels[key] = value
		}
	}
	return entry
}
