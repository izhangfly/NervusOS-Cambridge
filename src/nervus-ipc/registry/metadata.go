package registry

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"sort"

	"google.golang.org/protobuf/proto"

	ipcv1 "github.com/nervus-os/nervus-ipc/protocol/ipcv1"
)

// Metadata interfaces only declare methods' ids, permissions, risk and Transfer budgets, carrying no protobuf
// message types and needing no schema bundle.
//
// What the kernel really needs to know is only three things: who is calling (identity), is it allowed (permission), and how big a
// pipe can be opened (Transfer budget). All three are in MethodMeta, regardless of message shape. Camera frames,
// microphone samples and lidar point clouds go through the Transfer data plane as opaque bytes anyway; inventing a set of
// Request/Response messages for them that will never be used only makes "adding a capability" bring one more .proto to maintain.
//
// The contract identity still exists, just computed from the method metadata instead (MethodsHash). So nervud's
// sameInterfaceContract still holds, and the "vendors are interchangeable" property is fully kept.

// MethodsHash computes the contract hash of a set of MethodMetas.
//
// # Why it must sort by itself rather than trust the caller
//
// This hash is a contract identity compared **across processes, repositories and builds**: the packager computes it once and writes it into
// the descriptor, the kernel computes it again when loading to check, and two Providers must also match byte for byte.
// If any party's method order differs, the hash differs, and the symptom is "two parties wrote the same
// interface, yet the kernel judged it a conflict", very hard to track down. So it sorts by method_id before hashing,
// and order has no effect on the result.
//
// Each MethodMeta uses Deterministic encoding; a length prefix stops adjacent entries' bytes from concatenating ambiguously
// (otherwise {id:1}{id:23} and {id:12}{id:3} could produce the same input).
func MethodsHash(methods []*ipcv1.MethodMeta) ([]byte, error) {
	return ContractHash(methods, nil)
}

// ContractHash computes the full contract hash of an interface major: methods **and events** go in together.
//
// Events must be in the hash: DeliveryClass decides whether a client seeing a sequence gap should "do nothing"
// or treat it as "data permanently lost". If two Providers could declare different classes for the same event,
// whichever one an App resolved to would decide whether it misses data, which is exactly what sameInterfaceContract must stop.
//
// Methods and events are fed in separate sections, each sorted by id: they're independent numbering spaces, and sorting them together would make
// the relative position of "method 1" and "event 1" depend on implementation details.
func ContractHash(methods []*ipcv1.MethodMeta, events []*ipcv1.EventMeta) ([]byte, error) {
	if len(methods) == 0 && len(events) == 0 {
		return nil, errors.New("registry: metadata interface has neither methods nor events")
	}
	marshal := proto.MarshalOptions{Deterministic: true}
	sum := sha256.New()

	writeChunk := func(wire []byte) {
		// A length prefix stops adjacent entries' bytes from concatenating ambiguously
		var lengthPrefix [8]byte
		for i := 0; i < 8; i++ {
			lengthPrefix[i] = byte(len(wire) >> (8 * (7 - i)))
		}
		sum.Write(lengthPrefix[:])
		sum.Write(wire)
	}

	orderedMethods := make([]*ipcv1.MethodMeta, len(methods))
	copy(orderedMethods, methods)
	sort.Slice(orderedMethods, func(i, j int) bool {
		return orderedMethods[i].GetMethodId() < orderedMethods[j].GetMethodId()
	})
	for _, meta := range orderedMethods {
		wire, err := marshal.Marshal(meta)
		if err != nil {
			return nil, fmt.Errorf("registry: marshal method %d: %w", meta.GetMethodId(), err)
		}
		writeChunk(wire)
	}

	// A separator: without it, "3 methods and 0 events" and "2 methods and 1 event" could feed
	// the same input under some encodings
	sum.Write([]byte("\x00events\x00"))

	orderedEvents := make([]*ipcv1.EventMeta, len(events))
	copy(orderedEvents, events)
	sort.Slice(orderedEvents, func(i, j int) bool {
		return orderedEvents[i].GetEventId() < orderedEvents[j].GetEventId()
	})
	for _, meta := range orderedEvents {
		wire, err := marshal.Marshal(meta)
		if err != nil {
			return nil, fmt.Errorf("registry: marshal event %d: %w", meta.GetEventId(), err)
		}
		writeChunk(wire)
	}
	return sum.Sum(nil), nil
}

// NewMetadataSchema builds a Schema from inline method metadata.
//
// The returned Schema's files are nil: there are no message types to resolve. Consumers
// (nervud catalog's findMessage) return nil directly for empty request/response type names,
// so they never reach Files(), and metadata interfaces are forbidden to declare those type names anyway.
func NewMetadataSchema(
	interfaceID string, major uint32,
	metas []*ipcv1.MethodMeta, eventMetas []*ipcv1.EventMeta,
) (*Schema, error) {
	if interfaceID == "" {
		return nil, errors.New("registry: empty interface id")
	}
	if major == 0 {
		return nil, errors.New("registry: interface major 0 is reserved")
	}
	hash, err := ContractHash(metas, eventMetas)
	if err != nil {
		return nil, err
	}

	methods := make(map[uint32]*ipcv1.MethodMeta, len(metas))
	for _, meta := range metas {
		if err := validateMetadataMethod(meta); err != nil {
			return nil, fmt.Errorf("registry: interface %q@%d: %w", interfaceID, major, err)
		}
		if _, dup := methods[meta.GetMethodId()]; dup {
			return nil, fmt.Errorf("registry: interface %q@%d duplicate method id %d",
				interfaceID, major, meta.GetMethodId())
		}
		methods[meta.GetMethodId()] = proto.Clone(meta).(*ipcv1.MethodMeta)
	}

	events := make(map[uint32]*ipcv1.EventMeta, len(eventMetas))
	for _, meta := range eventMetas {
		if err := validateMetadataEvent(meta); err != nil {
			return nil, fmt.Errorf("registry: interface %q@%d: %w", interfaceID, major, err)
		}
		if _, dup := events[meta.GetEventId()]; dup {
			return nil, fmt.Errorf("registry: interface %q@%d duplicate event id %d",
				interfaceID, major, meta.GetEventId())
		}
		events[meta.GetEventId()] = proto.Clone(meta).(*ipcv1.EventMeta)
	}

	return &Schema{
		bundle: &ipcv1.InterfaceSchemaBundle{
			InterfaceId: interfaceID,
			Version:     major,
			SchemaHash:  hash,
		},
		files:   nil,
		methods: methods,
		events:  events,
	}, nil
}

// validateMetadataEvent follows the same rules as validateMetadataMethod.
func validateMetadataEvent(meta *ipcv1.EventMeta) error {
	if meta.GetEventId() == 0 {
		return errors.New("event id 0 is reserved")
	}
	if !knownRiskClass(meta.GetRiskClass()) {
		return fmt.Errorf("event %d has invalid risk class %d",
			meta.GetEventId(), meta.GetRiskClass())
	}
	if name := meta.GetPayloadType(); name != "" {
		return fmt.Errorf(
			"event %d declares payload type %q, but a metadata interface carries no schema bundle",
			meta.GetEventId(), name)
	}
	return nil
}

// addMetadataSchemas synthesises every interface version in the descriptor that inlines methods into a Schema,
// and puts it into the existing SchemaSet.
//
// Synthesising rather than opening a new channel: nervud's Catalog Builder only knows SchemaSet, and both kinds of interface
// go out the same way, so the kernel side doesn't change at all.
func addMetadataSchemas(descriptor *ipcv1.ProviderDescriptor, schemas *SchemaSet) error {
	if descriptor == nil || schemas == nil {
		return errors.New("registry: nil descriptor or schema set")
	}
	for _, iface := range descriptor.GetInterfaces() {
		for _, version := range iface.GetInterfaceVersions() {
			if len(version.GetMethods()) == 0 && len(version.GetEvents()) == 0 {
				continue
			}
			schema, err := NewMetadataSchema(
				iface.GetInterfaceId(), version.GetMajor(),
				version.GetMethods(), version.GetEvents())
			if err != nil {
				return err
			}
			key := SchemaKey{InterfaceID: iface.GetInterfaceId(), Major: version.GetMajor()}
			if _, dup := schemas.byKey[key]; dup {
				// The same (interface, major) both inlines methods and carries a bundle: two contracts,
				// with no telling which is authoritative. Refusing is safer than picking one
				return fmt.Errorf(
					"registry: interface %q@%d declares inline methods and a schema bundle",
					key.InterfaceID, key.Major)
			}
			if schemas.byKey == nil {
				schemas.byKey = make(map[SchemaKey]*Schema, 1)
			}
			schemas.byKey[key] = schema
		}
	}
	return nil
}

// validateMetadataMethod is the metadata interface version of validateMethodMeta: the same general checks,
// but it also forbids declaring message type names: there's no schema bundle to resolve them, so declaring them can't be checked,
// and letting them through would only let a type name that can never be resolved travel all the way to dispatch before failing.
func validateMetadataMethod(meta *ipcv1.MethodMeta) error {
	if meta.GetMethodId() == 0 {
		return errors.New("method id 0 is reserved")
	}
	if !knownRiskClass(meta.GetRiskClass()) {
		return fmt.Errorf("method %d has invalid risk class %d", meta.GetMethodId(), meta.GetRiskClass())
	}
	if timeMax := meta.GetMaxTimeoutMs(); timeMax != 0 &&
		meta.GetDefaultTimeoutMs() != 0 && meta.GetDefaultTimeoutMs() > timeMax {
		return fmt.Errorf("method %d default timeout exceeds maximum", meta.GetMethodId())
	}
	for label, name := range map[string]string{
		"request":      meta.GetRequestType(),
		"response":     meta.GetResponseType(),
		"error detail": meta.GetErrorDetailType(),
	} {
		if name != "" {
			return fmt.Errorf(
				"method %d declares %s type %q, but a metadata interface carries no schema bundle",
				meta.GetMethodId(), label, name)
		}
	}
	if transfer := meta.GetTransfer(); transfer != nil {
		if err := validateTransferPolicy(meta.GetMethodId(), transfer); err != nil {
			return err
		}
	}
	return nil
}

// validateTransferPolicy catches unbounded Transfer declarations at packaging time.
//
// nervud's transfer.Manager.Begin refuses with "method transfer policy is unbounded" if any of max_streams / max_packet_bytes /
// max_bytes_per_second is 0.
// By then the error appears at the first stream opening, far from "the manifest left one item out".
func validateTransferPolicy(methodID uint32, policy *ipcv1.TransferPolicy) error {
	if policy.GetMaxStreams() == 0 {
		return fmt.Errorf("method %d transfer policy has max_streams = 0", methodID)
	}
	if policy.GetMaxPacketBytes() == 0 {
		return fmt.Errorf("method %d transfer policy has max_packet_bytes = 0", methodID)
	}
	if policy.GetMaxBytesPerSecond() == 0 {
		return fmt.Errorf("method %d transfer policy has max_bytes_per_second = 0", methodID)
	}
	return nil
}
