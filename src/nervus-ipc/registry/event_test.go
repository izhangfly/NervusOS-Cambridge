package registry

import (
	"bytes"
	"strings"
	"testing"

	ipcv1 "github.com/nervus-os/nervus-ipc/protocol/ipcv1"
)

func eventDescriptor(
	t *testing.T, methods []*ipcv1.MethodMeta, events []*ipcv1.EventMeta,
) *ipcv1.ProviderDescriptor {
	t.Helper()
	hash, err := ContractHash(methods, events)
	if err != nil {
		t.Fatalf("ContractHash: %v", err)
	}
	return &ipcv1.ProviderDescriptor{
		PackageId: metaPackageID,
		Interfaces: []*ipcv1.ProvidedInterface{{
			InterfaceId: metaInterfaceID,
			InterfaceVersions: []*ipcv1.ProvidedInterfaceVersion{{
				Major: 1, SchemaHash: hash, Methods: methods, Events: events,
			}},
			RequiredPermission: metaPermission,
		}},
		Permissions: []*ipcv1.DefinedPermission{{
			Id:           metaPermission,
			GrantMode:    ipcv1.GrantMode_GRANT_MODE_NORMAL,
			RiskClass:    ipcv1.RiskClass_RISK_CLASS_NORMAL,
			MinimumTrust: ipcv1.PermissionTrustFloor_PERMISSION_TRUST_FLOOR_ORDINARY,
			DisplayName:  &ipcv1.LocalizedText{En: "Stream"},
			Description:  &ipcv1.LocalizedText{En: "Stream"},
		}},
	}
}

// A metadata interface with events must get all the way through Parse, with its event metadata queryable.
func TestEventsParseIntoSchema(t *testing.T) {
	descriptor := eventDescriptor(t, cameraLikeMethods(), cameraLikeEvents())
	descriptorWire, schemaWire, err := MarshalProviderArtifacts(
		descriptor, &ipcv1.InterfaceSchemaBundleSet{})
	if err != nil {
		t.Fatalf("MarshalProviderArtifacts: %v", err)
	}
	artifacts, err := ParseProviderArtifacts(descriptorWire, schemaWire)
	if err != nil {
		t.Fatalf("ParseProviderArtifacts: %v", err)
	}
	schema, ok := artifacts.Schemas.Lookup(metaInterfaceID, 1)
	if !ok {
		t.Fatal("interface not in the SchemaSet")
	}
	if len(schema.Events()) != 2 {
		t.Fatalf("event count = %d, want 2", len(schema.Events()))
	}
	state, ok := schema.Event(1)
	if !ok {
		t.Fatal("event 1 not found")
	}
	if state.GetDeliveryClass() != ipcv1.DeliveryClass_DELIVERY_CLASS_STATE {
		t.Errorf("delivery_class = %v, want STATE", state.GetDeliveryClass())
	}
	if state.GetMaxEventsPerSecond() != 10 {
		t.Errorf("max_events_per_second = %d, want 10", state.GetMaxEventsPerSecond())
	}
}

// **Events must be part of the contract hash**. DeliveryClass decides whether a client seeing a sequence gap should
// "do nothing" or treat it as "data permanently lost". If two Providers could declare different classes for the same event,
// whichever the App resolved to would decide whether it loses data.
func TestContractHashCoversEventChanges(t *testing.T) {
	base, err := ContractHash(cameraLikeMethods(), cameraLikeEvents())
	if err != nil {
		t.Fatalf("ContractHash: %v", err)
	}

	for _, tc := range []struct {
		name   string
		mutate func([]*ipcv1.EventMeta)
	}{
		{"change delivery class", func(e []*ipcv1.EventMeta) {
			e[0].DeliveryClass = ipcv1.DeliveryClass_DELIVERY_CLASS_LOSSY
		}},
		{"change event permission", func(e []*ipcv1.EventMeta) {
			e[0].RequiredPermission = "com.vendor.cam.permission.other"
		}},
		{"loosen publish rate", func(e []*ipcv1.EventMeta) { e[0].MaxEventsPerSecond = 1000 }},
		{"change event id", func(e []*ipcv1.EventMeta) { e[1].EventId = 3 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			events := cameraLikeEvents()
			tc.mutate(events)
			got, err := ContractHash(cameraLikeMethods(), events)
			if err != nil {
				t.Fatalf("ContractHash: %v", err)
			}
			if bytes.Equal(got, base) {
				t.Fatalf("contract hash unchanged after %s", tc.name)
			}
		})
	}
}

// "3 methods and 0 events" and "2 methods and 1 event" must not collide on the same hash.
// That's what the separator is for.
func TestContractHashSeparatesMethodsFromEvents(t *testing.T) {
	methodsOnly, err := ContractHash(cameraLikeMethods(), nil)
	if err != nil {
		t.Fatalf("ContractHash: %v", err)
	}
	withEvents, err := ContractHash(cameraLikeMethods(), cameraLikeEvents())
	if err != nil {
		t.Fatalf("ContractHash: %v", err)
	}
	if bytes.Equal(methodsOnly, withEvents) {
		t.Fatal("contract hash unchanged after adding an event")
	}
}

// Event order doesn't affect the contract identity, for the same reason as methods.
func TestContractHashEventOrderIndependent(t *testing.T) {
	events := cameraLikeEvents()
	forward, err := ContractHash(nil, events)
	if err != nil {
		t.Fatalf("ContractHash: %v", err)
	}
	backward, err := ContractHash(nil, []*ipcv1.EventMeta{events[1], events[0]})
	if err != nil {
		t.Fatalf("ContractHash reversed: %v", err)
	}
	if !bytes.Equal(forward, backward) {
		t.Fatal("ContractHash depends on event order")
	}
}

// An interface with only events and no methods is valid (a pure observation interface).
func TestEventOnlyInterfaceIsValid(t *testing.T) {
	if _, err := ContractHash(nil, cameraLikeEvents()); err != nil {
		t.Fatalf("pure event interface refused: %v", err)
	}
	if _, err := ContractHash(nil, nil); err == nil {
		t.Fatal("an interface with neither methods nor events was accepted")
	}
}

// A metadata interface's events may not declare payload_type: there's no schema bundle to resolve it.
func TestEventRejectsPayloadType(t *testing.T) {
	events := cameraLikeEvents()
	events[0].PayloadType = "com.vendor.cam.v1.StateChanged"
	descriptor := eventDescriptor(t, cameraLikeMethods(), events)

	_, _, err := MarshalProviderArtifacts(descriptor, &ipcv1.InterfaceSchemaBundleSet{})
	if err == nil || !strings.Contains(err.Error(), "carries no schema bundle") {
		t.Fatalf("err = %v, want the payload type name refused", err)
	}
}

// event id 0 is reserved.
func TestEventRejectsZeroID(t *testing.T) {
	events := cameraLikeEvents()
	events[0].EventId = 0
	if _, err := NewMetadataSchema(metaInterfaceID, 1, nil, events); err == nil {
		t.Fatal("event id 0 was accepted")
	}
}
