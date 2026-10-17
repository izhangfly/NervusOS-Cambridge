package motionprofile

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestMobileG1Dex3ProfileIsSignedAndResolvable(t *testing.T) {
	root := filepath.Join("..", "..", "..", "mobile-g1-dex3", "profile")
	catalog, err := LoadBundle(root, "mobile-g1-dex3-v1.json", Allowlist{Slots: map[string]Slot{
		"simulation.world.main": {ResourceType: "gazebo.simulation.motion.provider.resource.world", InterfaceID: "gazebo.simulation.motion.provider.control", Major: 1},
		"base.main":             {ResourceType: "nervus.resource.motion.base", InterfaceID: "nervus.interface.motion.base", Major: 1},
		"arm.left":              {ResourceType: "nervus.resource.manipulator.arm", InterfaceID: "nervus.interface.manipulator.arm", Major: 1},
		"arm.right":             {ResourceType: "nervus.resource.manipulator.arm", InterfaceID: "nervus.interface.manipulator.arm", Major: 1},
	}})
	if err != nil {
		t.Fatalf("LoadBundle: %v", err)
	}
	profile, ok := catalog.Profile("gazebo.unitree.g1.dex3.mobile.v1")
	if !ok {
		t.Fatal("mobile G1 Profile is missing")
	}
	if len(profile.GetJointGroups()) != 6 || len(profile.GetCapabilities()) != 2 {
		t.Fatalf("unexpected groups/capabilities: %d/%d", len(profile.GetJointGroups()), len(profile.GetCapabilities()))
	}
	var native struct {
		Groups map[string][]string `json:"groups"`
	}
	nativeData, err := os.ReadFile(filepath.Join(root, "..", "config", "native-joint-control.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(nativeData, &native); err != nil {
		t.Fatal(err)
	}
	for _, group := range profile.GetJointGroups() {
		want, compared := native.Groups[group.GetRole()]
		if !compared {
			continue
		}
		got := make([]string, 0, len(group.GetJoints()))
		for _, joint := range group.GetJoints() {
			got = append(got, joint.GetName())
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("group %s differs from native control config", group.GetRole())
		}
	}
	runtimeData, err := os.ReadFile(filepath.Join(root, "runtime-mobile-g1-dex3-v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := ParseRuntime(runtimeData, catalog)
	if err != nil {
		t.Fatalf("ParseRuntime: %v", err)
	}
	resolution := catalog.Resolve([]string{"nervus_mobile_g1_dex3"}, runtime)
	if len(resolution.Issues) != 0 || len(resolution.Instances) != 1 {
		t.Fatalf("unexpected resolution: %+v", resolution)
	}
	if resolution.Instances[0].Resources["base.motion"] != "base.main" || resolution.Instances[0].Resources["compatibility.joints"] != "simulation.world.main" {
		t.Fatalf("unexpected resource assignment: %+v", resolution.Instances[0].Resources)
	}
}

func TestMobileG1Dex3ProfileRejectsMessageAndResourceMismatch(t *testing.T) {
	root := filepath.Join("..", "..", "..", "mobile-g1-dex3", "profile")
	data, err := os.ReadFile(filepath.Join(root, "mobile-g1-dex3-v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	allowed := Allowlist{Slots: map[string]Slot{
		"simulation.world.main": {ResourceType: "gazebo.simulation.motion.provider.resource.world", InterfaceID: "gazebo.simulation.motion.provider.control", Major: 1},
		"base.main":             {ResourceType: "nervus.resource.motion.base", InterfaceID: "nervus.interface.motion.base", Major: 1},
		"arm.left":              {ResourceType: "nervus.resource.manipulator.arm", InterfaceID: "nervus.interface.manipulator.arm", Major: 1},
		"arm.right":             {ResourceType: "nervus.resource.manipulator.arm", InterfaceID: "nervus.interface.manipulator.arm", Major: 1},
	}}
	badType := bytes.Replace(data, []byte(`"message_type":"gz.msgs.Twist"`), []byte(`"message_type":"gz.msgs.Double"`), 1)
	if _, err := ParseBundle(badType, allowed); err == nil {
		t.Fatal("Profile accepted an overridden DiffDrive message type")
	}
	catalog, err := ParseBundle(data, allowed)
	if err != nil {
		t.Fatal(err)
	}
	badRuntime := []byte(`{"schemaVersion":1,"instances":[{"profileId":"gazebo.unitree.g1.dex3.mobile.v1","modelName":"nervus_mobile_g1_dex3","capabilityResources":[{"capabilityRole":"base.motion","resourceRole":"arm.left"}]}]}`)
	if _, err := ParseRuntime(badRuntime, catalog); err == nil {
		t.Fatal("runtime accepted an incompatible Resource assignment")
	}
}
