package main

import (
	"testing"

	ipcv1 "github.com/nervus-os/nervus-ipc/protocol/ipcv1"
	ipcregistry "github.com/nervus-os/nervus-ipc/registry"

	"github.com/nervus-os/nervus-system-server/camerad/boardprofile"
)

const testProfile = `{
  "schema": 1, "board": "test-board",
  "cameras": [
    {"role": "cam.front", "match": {"usb_path": "1-1.1"},
     "labels": {"nervus.camera.facing": "front", "nervus.camera.kind": "color"}}
  ],
  "usb_pool": {"role_prefix": "cam.usb", "max_slots": 2}
}`

func buildTestArtifacts(t *testing.T) *ipcregistry.ProviderArtifacts {
	t.Helper()
	profile, err := boardprofile.Parse([]byte(testProfile))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	descriptorWire, schemaWire, err := BuildArtifacts(profile)
	if err != nil {
		t.Fatalf("BuildArtifacts: %v", err)
	}
	// [Go through the real ParseProviderArtifacts] rather than only asserting struct fields: that function carries
	// a whole set of in-package invariants (resource types an interface references must be managed by this package, the risk floor must not
	// exceed the resource's risk, the default resource must be declared). Bypassing it would postpone those checks to installation on site.
	artifacts, err := ipcregistry.ParseProviderArtifacts(descriptorWire, schemaWire)
	if err != nil {
		t.Fatalf("ParseProviderArtifacts: %v", err)
	}
	return artifacts
}

func TestBuildArtifacts_PassesRegistryInvariants(t *testing.T) {
	buildTestArtifacts(t)
}

// Each role produces two resources: capture + config.
//
// The config side's stable_role must be [different]: the Catalog's handle index is globally unique across resource types,
// and a shared name would collide at install time with an error pointing at the Catalog, not at this code.
func TestBuildArtifacts_EachRoleYieldsCaptureAndConfig(t *testing.T) {
	profile, err := boardprofile.Parse([]byte(testProfile))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	resources := resourcesOf(profile)

	// 1 fixed + 2 pool slots = 3 roles, two resources each
	if len(resources) != 6 {
		t.Fatalf("resource count = %d, want 6", len(resources))
	}

	handles := make(map[string]bool, len(resources))
	for _, r := range resources {
		if handles[r.GetStableRole()] {
			t.Fatalf("stable_role %q is duplicated; the Catalog's handle index would refuse it outright",
				r.GetStableRole())
		}
		handles[r.GetStableRole()] = true
	}

	byRole := make(map[string]*ipcv1.ManagedResource, len(resources))
	for _, r := range resources {
		byRole[r.GetStableRole()] = r
	}

	capture := byRole["cam.front"]
	if capture.GetResourceType() != resourceCamera {
		t.Errorf("cam.front type = %q", capture.GetResourceType())
	}
	// [Shared observation]: several Apps watching one stream at once is normal.
	if capture.GetAccessMode() != ipcv1.ResourceAccessMode_RESOURCE_ACCESS_MODE_SHARED_OBSERVE {
		t.Errorf("cam.front access_mode = %v, want SHARED_OBSERVE", capture.GetAccessMode())
	}

	config := byRole["cam.front.config"]
	if config == nil {
		t.Fatal("missing the config resource for cam.front")
	}
	// [Exclusive control]: with two processes changing exposure at once, the result depends on timing.
	if config.GetAccessMode() != ipcv1.ResourceAccessMode_RESOURCE_ACCESS_MODE_EXCLUSIVE_CONTROL {
		t.Errorf("cam.front.config access_mode = %v, want EXCLUSIVE_CONTROL", config.GetAccessMode())
	}
	if config.GetResourceType() != resourceCameraConfig {
		t.Errorf("cam.front.config type = %q", config.GetResourceType())
	}
}

// Semantic labels only go on the capture side.
//
// Putting them on both would make facing=front match two resources of different types, and ResourceSelector
// defaults to REQUIRE_UNIQUE, so Apps selecting devices by label would fail outright.
func TestBuildArtifacts_LabelsOnlyOnCaptureSide(t *testing.T) {
	profile, err := boardprofile.Parse([]byte(testProfile))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	for _, r := range resourcesOf(profile) {
		switch r.GetResourceType() {
		case resourceCamera:
			if r.GetStableRole() == "cam.front" &&
				r.GetLabels()["nervus.camera.facing"] != "front" {
				t.Errorf("cam.front lost its facing label: %v", r.GetLabels())
			}
		case resourceCameraConfig:
			if len(r.GetLabels()) != 0 {
				t.Errorf("config resource %q carries labels: %v", r.GetStableRole(), r.GetLabels())
			}
		}
	}
}

// [Pool slots have no semantic labels]: nobody knows which way a camera plugged in on the fly is facing.
func TestBuildArtifacts_PoolSlotsAreUnlabelled(t *testing.T) {
	profile, err := boardprofile.Parse([]byte(testProfile))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	for _, r := range resourcesOf(profile) {
		if r.GetStableRole() == "cam.usb.0" && len(r.GetLabels()) != 0 {
			t.Fatalf("a pool slot carries labels: %v", r.GetLabels())
		}
	}
}

// [No default_resource_type/role].
//
// A machine with several cameras is normal, and an implicit default would make "I want a camera" silently resolve to one of them,
// while v2 has only just removed the base's implicit default; this shouldn't plant a new one.
func TestBuildArtifacts_NoImplicitDefaultResource(t *testing.T) {
	artifacts := buildTestArtifacts(t)
	for _, iface := range artifacts.Descriptor.GetInterfaces() {
		if iface.GetDefaultResourceType() != "" || iface.GetDefaultResourceRole() != "" {
			t.Errorf("%s declares an implicit default resource %q/%q",
				iface.GetInterfaceId(),
				iface.GetDefaultResourceType(), iface.GetDefaultResourceRole())
		}
	}
}

// Both interfaces must carry event metadata, [including payload_type].
//
// Without it the service registers fine, then every PublishEvent is refused by the kernel, and the refusal
// only goes into the kernel audit with no reply visible to the service: "subscribed but never receives events".
//
// Events go through the schema bundle rather than the descriptor's inline field: the inline route is for metadata
// interfaces, and it doesn't allow payload_type (with no descriptor set, type names can't be checked).
func TestBuildArtifacts_InterfacesCarryTypedEvents(t *testing.T) {
	artifacts := buildTestArtifacts(t)

	for _, interfaceID := range []string{interfaceCamera, interfaceCameraConfig} {
		schema, ok := artifacts.Schemas.Lookup(interfaceID, interfaceMajor)
		if !ok {
			t.Fatalf("%s has no schema", interfaceID)
		}
		events := schema.Events()
		if len(events) == 0 {
			t.Fatalf("%s declares no events", interfaceID)
		}
		for id, meta := range events {
			if meta.GetPayloadType() == "" {
				t.Errorf("%s event %d has no payload type", interfaceID, id)
			}
			// delivery_class can't be empty: when empty, nervud fails closed and treats it as RELIABLE,
			// and a video stream's state events as RELIABLE would get disconnected whenever the consumer is a bit slow.
			if meta.GetDeliveryClass() == ipcv1.DeliveryClass_DELIVERY_CLASS_UNSPECIFIED {
				t.Errorf("%s event %d declares no delivery_class", interfaceID, id)
			}
		}
	}
}

// Events [shouldn't] be inlined in the descriptor: that route is for metadata interfaces, and writing both
// would be refused by the registry as "the same interface both inline and with a bundle".
func TestBuildArtifacts_DescriptorDoesNotInlineEvents(t *testing.T) {
	artifacts := buildTestArtifacts(t)
	for _, iface := range artifacts.Descriptor.GetInterfaces() {
		for _, version := range iface.GetInterfaceVersions() {
			if len(version.GetEvents()) != 0 {
				t.Errorf("%s@%d inlines events in the descriptor",
					iface.GetInterfaceId(), version.GetMajor())
			}
		}
	}
}

// Both permissions are USER_CONSENT: a camera starting to film you shouldn't be agreed to on your behalf by installing something.
func TestBuildArtifacts_CameraPermissionsRequireUserConsent(t *testing.T) {
	for _, permission := range permissions() {
		if permission.GetGrantMode() != ipcv1.GrantMode_GRANT_MODE_USER_CONSENT {
			t.Errorf("%s grant_mode = %v, want USER_CONSENT",
				permission.GetId(), permission.GetGrantMode())
		}
		if permission.GetRiskClass() != ipcv1.RiskClass_RISK_CLASS_PRIVACY_SENSITIVE {
			t.Errorf("%s risk_class = %v, want PRIVACY_SENSITIVE",
				permission.GetId(), permission.GetRiskClass())
		}
	}
}

// The reference board config in the repository must build a valid contract: it's the template copied for new boards,
// and a broken template would start every new board off wrong.
func TestBuildArtifacts_ReferenceBoardIsValid(t *testing.T) {
	profile, err := boardprofile.Load("../boards/reference.json")
	if err != nil {
		t.Fatalf("loading the reference board config: %v", err)
	}
	descriptorWire, schemaWire, err := BuildArtifacts(profile)
	if err != nil {
		t.Fatalf("BuildArtifacts: %v", err)
	}
	if _, err := ipcregistry.ParseProviderArtifacts(descriptorWire, schemaWire); err != nil {
		t.Fatalf("the reference board config produces an invalid contract: %v", err)
	}
}
