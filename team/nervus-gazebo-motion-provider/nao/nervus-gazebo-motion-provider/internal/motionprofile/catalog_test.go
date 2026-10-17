package motionprofile

import (
	"math"
	"strings"
	"testing"

	profilev1 "github.com/nervus-os/nervus-gazebo-provider/gen/go/gazebo/simulation/motion/profile/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

func providerAllowlist() Allowlist {
	return Allowlist{Slots: map[string]Slot{
		"simulation.world.main": {
			ResourceType: "gazebo.simulation.motion.provider.resource.world",
			InterfaceID:  "gazebo.simulation.motion.provider.control", Major: 1,
		},
		"base.main": {
			ResourceType: "nervus.resource.motion.base",
			InterfaceID:  "nervus.interface.motion.base", Major: 1,
		},
		"arm.left": {
			ResourceType: "nervus.resource.manipulator.arm",
			InterfaceID:  "nervus.interface.manipulator.arm", Major: 1,
		},
		"arm.right": {
			ResourceType: "nervus.resource.manipulator.arm",
			InterfaceID:  "nervus.interface.manipulator.arm", Major: 1,
		},
	}}
}

func loadNAOTestCatalog(t *testing.T) *Catalog {
	t.Helper()
	catalog, err := LoadBundle("../../profiles", "nao-fixed-v1.json", providerAllowlist())
	if err != nil {
		t.Fatalf("LoadBundle: %v", err)
	}
	return catalog
}

func TestFixedNAOProfileIsCompatibilityOnlyAndContainsAllControllers(t *testing.T) {
	catalog := loadNAOTestCatalog(t)
	profile, ok := catalog.Profile("gazebo.nao.fixed.v1")
	if !ok {
		t.Fatal("fixed NAO Profile is missing")
	}
	if len(profile.GetJointGroups()) != 1 || len(profile.GetJointGroups()[0].GetJoints()) != 24 {
		t.Fatalf("fixed NAO joint count = %d, want 24", len(profile.GetJointGroups()[0].GetJoints()))
	}
	if len(profile.GetCapabilities()) != 1 {
		t.Fatalf("capability count = %d, want 1", len(profile.GetCapabilities()))
	}
	capability := profile.GetCapabilities()[0]
	if capability.GetSemantic() != profilev1.CapabilitySemantic_CAPABILITY_SEMANTIC_COMPATIBILITY_JOINT_CONTROL ||
		capability.GetInterfaceId() != "gazebo.simulation.motion.provider.control" {
		t.Fatalf("fixed NAO Profile claims a standard capability: %+v", capability)
	}
	resolution := catalog.Resolve([]string{"nervus_nao"}, nil)
	if len(resolution.Instances) != 1 || len(resolution.Issues) != 0 ||
		resolution.Instances[0].Resources[capability.GetRole()] != "simulation.world.main" {
		t.Fatalf("fixed NAO resolution = %+v", resolution)
	}
}

func TestFixedNAORuntimeSelectsOnlyTheSignedNAOInstance(t *testing.T) {
	catalog := loadNAOTestCatalog(t)
	runtime, err := LoadRuntime("../../profiles", "runtime-nao-fixed-v1.json", catalog)
	if err != nil {
		t.Fatalf("LoadRuntime: %v", err)
	}
	resolution := catalog.Resolve([]string{"nervus_nao", "other_robot"}, runtime)
	if len(resolution.Instances) != 1 || len(resolution.Issues) != 0 {
		t.Fatalf("NAO runtime resolution = %+v", resolution)
	}
	instance := resolution.Instances[0]
	if instance.Model != "nervus_nao" || instance.Profile.GetProfileId() != "gazebo.nao.fixed.v1" ||
		instance.Resources["compatibility.joints"] != "simulation.world.main" {
		t.Fatalf("NAO runtime instance = %+v", instance)
	}
}

func TestStrictBundleValidationRejectsUnsafeOrOpenEndedConfiguration(t *testing.T) {
	base := loadNAOTestCatalog(t).Bundle()
	tests := []struct {
		name   string
		mutate func(*profilev1.ProfileBundle)
	}{
		{name: "unsupported schema", mutate: func(bundle *profilev1.ProfileBundle) {
			bundle.SchemaVersion = 99
		}},
		{name: "duplicate resource role", mutate: func(bundle *profilev1.ProfileBundle) {
			bundle.ResourceSlots = append(bundle.ResourceSlots,
				proto.Clone(bundle.ResourceSlots[0]).(*profilev1.ResourceSlot))
		}},
		{name: "unsupported adapter", mutate: func(bundle *profilev1.ProfileBundle) {
			bundle.Profiles[0].CommandBindings[0].Adapter = profilev1.AdapterKind(99)
		}},
		{name: "message type override", mutate: func(bundle *profilev1.ProfileBundle) {
			bundle.Profiles[0].CommandBindings[0].MessageType = "gz.msgs.StringMsg"
		}},
		{name: "duplicate capability role", mutate: func(bundle *profilev1.ProfileBundle) {
			bundle.Profiles[0].Capabilities = append(bundle.Profiles[0].Capabilities,
				proto.Clone(bundle.Profiles[0].Capabilities[0]).(*profilev1.MotionCapability))
		}},
		{name: "unsafe model token", mutate: func(bundle *profilev1.ProfileBundle) {
			bundle.Profiles[0].Matcher.Selector = &profilev1.ModelMatcher_ExactName{ExactName: "../robot"}
		}},
		{name: "non finite", mutate: func(bundle *profilev1.ProfileBundle) {
			bundle.Profiles[0].JointGroups[0].Joints[0].HomePositionRad = math.NaN()
		}},
	}
	marshal := protojson.MarshalOptions{UseProtoNames: true}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			bundle := proto.Clone(base).(*profilev1.ProfileBundle)
			test.mutate(bundle)
			wire, err := marshal.Marshal(bundle)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			if _, err := ParseBundle(wire, providerAllowlist()); err == nil {
				t.Fatal("invalid bundle was accepted")
			}
		})
	}

	valid, err := marshal.Marshal(base)
	if err != nil {
		t.Fatal(err)
	}
	unknown := strings.Replace(string(valid), "{", `{"unknown_field":true,`, 1)
	if _, err := ParseBundle([]byte(unknown), providerAllowlist()); err == nil {
		t.Fatal("unknown JSON field was accepted")
	}
}

func TestConfigurationPathsCannotEscapeAllowlistedRoot(t *testing.T) {
	if _, err := LoadBundle("../../profiles", "../go.mod", providerAllowlist()); err == nil {
		t.Fatal("parent traversal escaped profile root")
	}
	if _, err := LoadBundle("../../profiles", "", providerAllowlist()); err == nil {
		t.Fatal("empty profile path was accepted")
	}
}

func TestMultipleProfileInstancesRequireDistinctSignedResourceSlots(t *testing.T) {
	const bundleJSON = `{
	  "schemaVersion": 1,
	  "resourceSlots": [
	    {"stableRole":"arm.left","resourceType":"nervus.resource.manipulator.arm","interfaceId":"nervus.interface.manipulator.arm","interfaceMajor":1},
	    {"stableRole":"arm.right","resourceType":"nervus.resource.manipulator.arm","interfaceId":"nervus.interface.manipulator.arm","interfaceMajor":1}
	  ],
	  "profiles": [{
	    "schemaVersion":1,"profileId":"gazebo.test.arm.v1",
	    "matcher":{"namePrefix":"robot"},
	    "jointGroups":[{"role":"arm","joints":[{"role":"joint","name":"Joint1","axis":0,"positionToleranceRad":0.01}]}],
	    "commandBindings":[{"role":"trajectory","adapter":"ADAPTER_KIND_JOINT_TRAJECTORY","topicTemplate":"/model/{model}/joint_trajectory","messageType":"gz.msgs.JointTrajectory"}],
	    "stateSources":[{"role":"joints","kind":"STATE_SOURCE_KIND_JOINT_STATE","topicTemplate":"/world/{world}/model/{model}/joint_state","messageType":"gz.msgs.Model"}],
	    "capabilities":[{"role":"arm","semantic":"CAPABILITY_SEMANTIC_MANIPULATOR_ARM","adapter":"ADAPTER_KIND_JOINT_TRAJECTORY","interfaceId":"nervus.interface.manipulator.arm","interfaceMajor":1,"resourceRoles":["arm.left","arm.right"],"jointGroup":"arm","commandBinding":"trajectory","stateSources":["joints"]}]
	  }]
	}`
	armAllowlist := Allowlist{Slots: map[string]Slot{
		"arm.left": {
			ResourceType: "nervus.resource.manipulator.arm",
			InterfaceID:  "nervus.interface.manipulator.arm", Major: 1,
		},
		"arm.right": {
			ResourceType: "nervus.resource.manipulator.arm",
			InterfaceID:  "nervus.interface.manipulator.arm", Major: 1,
		},
	}}
	catalog, err := ParseBundle([]byte(bundleJSON), armAllowlist)
	if err != nil {
		t.Fatalf("ParseBundle: %v", err)
	}
	resolution := catalog.Resolve([]string{"robot_a", "robot_b", "robot_c"}, nil)
	if len(resolution.Instances) != 2 || len(resolution.Issues) != 1 ||
		resolution.Issues[0].Reason != "resource_slots_exhausted" {
		t.Fatalf("automatic resolution = %+v", resolution)
	}

	const runtimeOK = `{"schemaVersion":1,"instances":[
	  {"profileId":"gazebo.test.arm.v1","modelName":"robot_a","capabilityResources":[{"capabilityRole":"arm","resourceRole":"arm.left"}]},
	  {"profileId":"gazebo.test.arm.v1","modelName":"robot_b","capabilityResources":[{"capabilityRole":"arm","resourceRole":"arm.right"}]}
	]}`
	if _, err := ParseRuntime([]byte(runtimeOK), catalog); err != nil {
		t.Fatalf("distinct runtime Resources: %v", err)
	}
	runtimeBad := strings.Replace(runtimeOK, "arm.right", "arm.left", 1)
	if _, err := ParseRuntime([]byte(runtimeBad), catalog); err == nil {
		t.Fatal("duplicate runtime Resource assignment was accepted")
	}
}
