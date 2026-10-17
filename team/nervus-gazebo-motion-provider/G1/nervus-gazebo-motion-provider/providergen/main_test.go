package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"

	simulationv1 "github.com/nervus-os/nervus-gazebo-provider/gen/go/gazebo/simulation/provider/v1"
	"github.com/nervus-os/nervus-gazebo-provider/internal/contract"
	"github.com/nervus-os/nervus-gazebo-provider/internal/motionprofile"
	"github.com/nervus-os/nervus-ipc/protocol/ipcv1"
	ipcregistry "github.com/nervus-os/nervus-ipc/registry"
)

func buildTestArtifacts(t *testing.T) ([]byte, []byte, *ipcregistry.ProviderArtifacts) {
	t.Helper()
	descriptorWire, schemaWire, err := BuildArtifacts()
	if err != nil {
		t.Fatalf("BuildArtifacts: %v", err)
	}
	artifacts, err := ipcregistry.ParseProviderArtifacts(descriptorWire, schemaWire)
	if err != nil {
		t.Fatalf("ParseProviderArtifacts: %v", err)
	}
	return descriptorWire, schemaWire, artifacts
}

func TestBuildArtifactsPassesRegistryInvariants(t *testing.T) {
	buildTestArtifacts(t)
}

func TestBuildArtifactsIsDeterministic(t *testing.T) {
	descriptorA, schemasA, _ := buildTestArtifacts(t)
	descriptorB, schemasB, _ := buildTestArtifacts(t)
	if !bytes.Equal(descriptorA, descriptorB) {
		t.Fatal("provider descriptor encoding is not deterministic")
	}
	if !bytes.Equal(schemasA, schemasB) {
		t.Fatal("schema bundle encoding is not deterministic")
	}
}

func TestDescriptorDeclaresPrivateAndStandardMotionContracts(t *testing.T) {
	_, _, artifacts := buildTestArtifacts(t)
	descriptor := artifacts.Descriptor
	if descriptor.GetPackageId() != packageID {
		t.Fatalf("package_id = %q, want %q", descriptor.GetPackageId(), packageID)
	}
	if len(descriptor.GetInterfaces()) != 4 {
		t.Fatalf("interface count = %d, want 4", len(descriptor.GetInterfaces()))
	}
	if len(descriptor.GetResources()) != 4 {
		t.Fatalf("resource count = %d, want 4", len(descriptor.GetResources()))
	}

	resources := make(map[string]*ipcv1.ManagedResource, 4)
	for _, item := range descriptor.GetResources() {
		resources[item.GetStableRole()] = item
	}
	resource := resources[worldResourceRole]
	if resource == nil {
		t.Fatal("world Resource is missing")
	}
	if resource.GetStableRole() != worldResourceRole || resource.GetResourceType() != worldResourceType {
		t.Fatalf("unexpected world resource: role=%q type=%q", resource.GetStableRole(), resource.GetResourceType())
	}
	if resource.GetAccessMode() != ipcv1.ResourceAccessMode_RESOURCE_ACCESS_MODE_EXCLUSIVE_CONTROL {
		t.Fatalf("world access mode = %v, want EXCLUSIVE_CONTROL", resource.GetAccessMode())
	}
	if resource.GetRiskClass() != ipcv1.RiskClass_RISK_CLASS_NORMAL {
		t.Fatalf("world risk = %v, want NORMAL", resource.GetRiskClass())
	}
	if resource.GetLabels()[worldKindLabel] != "world" {
		t.Fatalf("world label missing: %v", resource.GetLabels())
	}

	for role, resourceType := range map[string]string{
		baseResourceRole: baseResourceType,
		leftArmRole:      armResourceType,
		rightArmRole:     armResourceType,
	} {
		item := resources[role]
		if item == nil || item.GetResourceType() != resourceType ||
			item.GetRiskClass() != ipcv1.RiskClass_RISK_CLASS_PHYSICAL_CONTROL ||
			item.GetAccessMode() != ipcv1.ResourceAccessMode_RESOURCE_ACCESS_MODE_EXCLUSIVE_CONTROL {
			t.Errorf("invalid standard motion Resource %q: %+v", role, item)
		}
	}

	byID := make(map[string]*ipcv1.ProvidedInterface, 4)
	for _, iface := range descriptor.GetInterfaces() {
		byID[iface.GetInterfaceId()] = iface
	}
	manager := byID[managerInterface]
	if manager == nil {
		t.Fatal("manager interface is missing")
	}
	if len(manager.GetCompatibleResourceTypes()) != 0 {
		t.Fatalf("manager unexpectedly binds resources: %v", manager.GetCompatibleResourceTypes())
	}
	if manager.GetRequiredPermission() != queryPermission {
		t.Fatalf("manager Resolve permission = %q, want %q", manager.GetRequiredPermission(), queryPermission)
	}
	control := byID[controlInterface]
	if control == nil {
		t.Fatal("control interface is missing")
	}
	if control.GetDefaultResourceType() != worldResourceType || control.GetDefaultResourceRole() != worldResourceRole {
		t.Fatalf("control default resource = %q/%q", control.GetDefaultResourceType(), control.GetDefaultResourceRole())
	}
	if control.GetRequiredPermission() != queryPermission {
		t.Fatalf("control Resolve permission = %q, want %q", control.GetRequiredPermission(), queryPermission)
	}
	base := byID[baseInterface]
	if base == nil || base.GetRequiredPermission() != "perm.motion.control" ||
		base.GetDefaultResourceType() != baseResourceType ||
		base.GetDefaultResourceRole() != baseResourceRole {
		t.Fatalf("invalid BaseMotion contract: %+v", base)
	}
	arm := byID[armInterface]
	if arm == nil || arm.GetRequiredPermission() != "perm.manipulator.control" ||
		len(arm.GetCompatibleResourceTypes()) != 1 ||
		arm.GetCompatibleResourceTypes()[0] != armResourceType ||
		arm.GetDefaultResourceRole() != "" {
		t.Fatalf("invalid Manipulator contract: %+v", arm)
	}
	for _, interfaceID := range []string{baseInterface, armInterface} {
		if _, ok := artifacts.Schemas.Lookup(interfaceID, interfaceMajor); !ok {
			t.Errorf("standard schema %s@%d is missing", interfaceID, interfaceMajor)
		}
	}
}

func TestResetWorldIsResourceScopedAndLeaseProtected(t *testing.T) {
	_, _, artifacts := buildTestArtifacts(t)
	manager, ok := artifacts.Schemas.Lookup(managerInterface, interfaceMajor)
	if !ok {
		t.Fatal("manager schema is missing")
	}
	if len(manager.Methods()) != 8 {
		t.Fatalf("manager method count = %d, want 8", len(manager.Methods()))
	}
	for id, method := range manager.Methods() {
		if method.GetRequiresControlLease() {
			t.Fatalf("resource-free manager method %d requires a ControlLease", id)
		}
	}

	control, ok := artifacts.Schemas.Lookup(controlInterface, interfaceMajor)
	if !ok {
		t.Fatal("control schema is missing")
	}
	if len(control.Methods()) != 6 {
		t.Fatalf("control method count = %d, want 6", len(control.Methods()))
	}
	motionMethods := map[uint32]bool{
		uint32(simulationv1.ControlMethod_CONTROL_METHOD_SET_JOINT_POSITION):  true,
		uint32(simulationv1.ControlMethod_CONTROL_METHOD_SET_JOINT_POSITIONS): true,
	}
	for id, method := range control.Methods() {
		if !method.GetRequiresControlLease() {
			t.Errorf("resource-bound control method %d does not require a ControlLease", id)
		}
		if method.GetIsMotion() != motionMethods[id] {
			t.Errorf("control method %d is_motion = %t, want %t", id, method.GetIsMotion(), motionMethods[id])
		}
	}
	resetID := uint32(simulationv1.ControlMethod_CONTROL_METHOD_RESET_WORLD)
	reset := control.Methods()[resetID]
	if reset == nil {
		t.Fatalf("control ResetWorld method %d is missing", resetID)
	}
	if !reset.GetRequiresControlLease() {
		t.Fatal("ResetWorld does not require a ControlLease")
	}
	if reset.GetIsMotion() {
		t.Fatal("ResetWorld must not be marked as robot motion")
	}
}

func TestMethodAndEventMetadataIsBounded(t *testing.T) {
	_, _, artifacts := buildTestArtifacts(t)
	for _, interfaceID := range []string{managerInterface, controlInterface} {
		schema, ok := artifacts.Schemas.Lookup(interfaceID, interfaceMajor)
		if !ok {
			t.Fatalf("schema %s@%d is missing", interfaceID, interfaceMajor)
		}
		for id, method := range schema.Methods() {
			if method.GetRequestType() == "" || method.GetResponseType() == "" || method.GetErrorDetailType() == "" {
				t.Errorf("%s method %d has an untyped payload", interfaceID, id)
			}
			if method.GetDefaultTimeoutMs() == 0 || method.GetMaxTimeoutMs() == 0 ||
				method.GetMaxRequestBytes() == 0 || method.GetMaxResponseBytes() == 0 {
				t.Errorf("%s method %d has an unbounded budget", interfaceID, id)
			}
		}
		for id, event := range schema.Events() {
			if event.GetPayloadType() == "" {
				t.Errorf("%s event %d has no payload type", interfaceID, id)
			}
			if event.GetDeliveryClass() == ipcv1.DeliveryClass_DELIVERY_CLASS_UNSPECIFIED {
				t.Errorf("%s event %d has no delivery class", interfaceID, id)
			}
			if event.GetMaxPayloadBytes() == 0 || event.GetMaxEventsPerSecond() == 0 {
				t.Errorf("%s event %d has an unbounded budget", interfaceID, id)
			}
		}
	}
}

func TestMethodAndEventPermissionsAreSeparated(t *testing.T) {
	_, _, artifacts := buildTestArtifacts(t)
	manager, _ := artifacts.Schemas.Lookup(managerInterface, interfaceMajor)
	managerMethods := map[uint32]string{
		uint32(simulationv1.ManagerMethod_MANAGER_METHOD_GET_STATUS):       queryPermission,
		uint32(simulationv1.ManagerMethod_MANAGER_METHOD_LIST_MODELS):      queryPermission,
		uint32(simulationv1.ManagerMethod_MANAGER_METHOD_LIST_JOINTS):      queryPermission,
		uint32(simulationv1.ManagerMethod_MANAGER_METHOD_GET_JOINT_STATE):  queryPermission,
		uint32(simulationv1.ManagerMethod_MANAGER_METHOD_START_SIMULATION): lifecyclePermission,
		uint32(simulationv1.ManagerMethod_MANAGER_METHOD_STOP_SIMULATION):  lifecyclePermission,
		uint32(simulationv1.ManagerMethod_MANAGER_METHOD_LOAD_WORLD):       lifecyclePermission,
		uint32(simulationv1.ManagerMethod_MANAGER_METHOD_GET_CAPABILITIES): queryPermission,
	}
	for id, permission := range managerMethods {
		method := manager.Methods()[id]
		if method == nil {
			t.Errorf("manager method %d is missing", id)
			continue
		}
		if got := method.GetRequiredPermission(); got != permission {
			t.Errorf("manager method %d permission = %q, want %q", id, got, permission)
		}
	}
	for id, event := range manager.Events() {
		if got := event.GetRequiredPermission(); got != queryPermission {
			t.Errorf("manager event %d permission = %q, want %q", id, got, queryPermission)
		}
	}

	control, _ := artifacts.Schemas.Lookup(controlInterface, interfaceMajor)
	for id, method := range control.Methods() {
		if got := method.GetRequiredPermission(); got != controlPermission {
			t.Errorf("control method %d permission = %q, want %q", id, got, controlPermission)
		}
	}
	wantControlEventPermissions := map[uint32]string{
		uint32(simulationv1.ControlEvent_CONTROL_EVENT_JOINT_STATE_CHANGED): queryPermission,
		uint32(simulationv1.ControlEvent_CONTROL_EVENT_COMMAND_REJECTED):    controlPermission,
	}
	for id, permission := range wantControlEventPermissions {
		event := control.Events()[id]
		if event == nil {
			t.Errorf("control event %d is missing", id)
			continue
		}
		if got := event.GetRequiredPermission(); got != permission {
			t.Errorf("control event %d permission = %q, want %q", id, got, permission)
		}
	}
}

func TestPrivatePermissionsAreComplete(t *testing.T) {
	want := map[string]ipcv1.GrantMode{
		queryPermission:     ipcv1.GrantMode_GRANT_MODE_NORMAL,
		lifecyclePermission: ipcv1.GrantMode_GRANT_MODE_USER_CONSENT,
		controlPermission:   ipcv1.GrantMode_GRANT_MODE_USER_CONSENT,
	}
	for _, permission := range permissions() {
		mode, ok := want[permission.GetId()]
		if !ok {
			t.Errorf("unexpected permission %q", permission.GetId())
			continue
		}
		delete(want, permission.GetId())
		if permission.GetGrantMode() != mode {
			t.Errorf("%s grant mode = %v, want %v", permission.GetId(), permission.GetGrantMode(), mode)
		}
		if permission.GetRiskClass() != ipcv1.RiskClass_RISK_CLASS_NORMAL {
			t.Errorf("%s risk = %v, want NORMAL", permission.GetId(), permission.GetRiskClass())
		}
		if permission.GetDisplayName().GetEn() == "" ||
			permission.GetDescription().GetEn() == "" {
			t.Errorf("%s is missing bilingual UI text", permission.GetId())
		}
	}
	if len(want) != 0 {
		t.Fatalf("missing permissions: %v", want)
	}
}

func TestFixedProfileSlotsCloseWithCheckedContract(t *testing.T) {
	_, _, artifacts := buildTestArtifacts(t)
	catalog, err := motionprofile.LoadBundle(
		filepath.Join("..", "profiles"),
		"nao-fixed-v1.json",
		contract.MotionProfileAllowlist(),
	)
	if err != nil {
		t.Fatalf("load fixed Profile bundle: %v", err)
	}
	resources := make(map[string]*ipcv1.ManagedResource)
	for _, resource := range artifacts.Descriptor.GetResources() {
		resources[resource.GetStableRole()] = resource
	}
	interfaces := make(map[string]*ipcv1.ProvidedInterface)
	for _, iface := range artifacts.Descriptor.GetInterfaces() {
		interfaces[iface.GetInterfaceId()] = iface
	}
	for _, slot := range catalog.Bundle().GetResourceSlots() {
		resource := resources[slot.GetStableRole()]
		iface := interfaces[slot.GetInterfaceId()]
		if resource == nil || resource.GetResourceType() != slot.GetResourceType() || iface == nil {
			t.Errorf("Profile slot does not close: %+v", slot)
			continue
		}
		matchedType := false
		for _, resourceType := range iface.GetCompatibleResourceTypes() {
			if resourceType == slot.GetResourceType() {
				matchedType = true
				break
			}
		}
		if !matchedType {
			t.Errorf("interface %q does not accept Profile resource type %q", slot.GetInterfaceId(), slot.GetResourceType())
		}
	}
}

func TestCheckedInArtifactsMatchGenerator(t *testing.T) {
	descriptorWire, schemaWire, _ := buildTestArtifacts(t)
	for name, want := range map[string][]byte{
		descriptorFileName: descriptorWire,
		schemasFileName:    schemaWire,
	} {
		got, err := os.ReadFile(filepath.Join("..", "provider", name))
		if err != nil {
			t.Fatalf("read checked-in %s: %v", name, err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("checked-in %s is stale; run scripts/generate-provider.sh", name)
		}
	}
}

func TestManifestExportsAndArtifactsClose(t *testing.T) {
	type manifestExport struct {
		Interface string `json:"interface"`
	}
	type manifestComponent struct {
		Exports []manifestExport `json:"exports"`
	}
	type manifestProvider struct {
		Descriptor string `json:"descriptor"`
		Schemas    string `json:"schemas"`
	}
	var manifest struct {
		PackageID     string              `json:"package_id"`
		Label         string              `json:"label"`
		SupportedABIs []string            `json:"supported_abis"`
		Components    []manifestComponent `json:"components"`
		Provider      manifestProvider    `json:"provider"`
	}
	wire, err := os.ReadFile(filepath.Join("..", "manifest.json.in"))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	if err := json.Unmarshal(wire, &manifest); err != nil {
		t.Fatalf("parse manifest: %v", err)
	}
	if manifest.PackageID != packageID {
		t.Fatalf("manifest package_id = %q, want %q", manifest.PackageID, packageID)
	}
	if manifest.Label != "Nervus Gazebo Motion Provider - Unitree G1 Fixed" {
		t.Fatalf("manifest label = %q", manifest.Label)
	}
	if len(manifest.SupportedABIs) != 1 || manifest.SupportedABIs[0] != "linux-x86_64" {
		t.Fatalf("manifest supported_abis = %v, want [linux-x86_64]", manifest.SupportedABIs)
	}
	var exports []string
	for _, component := range manifest.Components {
		for _, export := range component.Exports {
			exports = append(exports, export.Interface)
		}
	}
	sort.Strings(exports)
	wantExports := []string{armInterface, baseInterface, controlInterface, managerInterface}
	sort.Strings(wantExports)
	if !reflect.DeepEqual(exports, wantExports) {
		t.Fatalf("manifest exports = %v, want %v", exports, wantExports)
	}
	if manifest.Provider.Descriptor != "provider/provider.binpb" || manifest.Provider.Schemas != "provider/schemas.binpb" {
		t.Fatalf("manifest provider artifacts = %q/%q", manifest.Provider.Descriptor, manifest.Provider.Schemas)
	}
}
