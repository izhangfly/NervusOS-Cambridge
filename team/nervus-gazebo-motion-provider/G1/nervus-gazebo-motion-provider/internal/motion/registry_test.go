package motion

import (
	"testing"

	profilev1 "github.com/nervus-os/nervus-gazebo-provider/gen/go/gazebo/simulation/motion/profile/v1"
)

func TestRegistryIsClosedAndTyped(t *testing.T) {
	want := map[profilev1.AdapterKind]string{
		profilev1.AdapterKind_ADAPTER_KIND_JOINT_POSITION:   TypeDouble,
		profilev1.AdapterKind_ADAPTER_KIND_JOINT_TRAJECTORY: TypeJointTrajectory,
		profilev1.AdapterKind_ADAPTER_KIND_DIFF_DRIVE:       TypeTwist,
		profilev1.AdapterKind_ADAPTER_KIND_HUMANOID_GAIT:    TypeTwist,
	}
	specs := Specs()
	if len(specs) != len(want) {
		t.Fatalf("adapter count = %d, want %d", len(specs), len(want))
	}
	for _, spec := range specs {
		if spec.CommandType != want[spec.Kind] || spec.Direction == DirectionUnspecified ||
			len(spec.Semantics) == 0 || len(spec.CommandTokens) == 0 {
			t.Errorf("invalid adapter spec: %+v", spec)
		}
		delete(want, spec.Kind)
	}
	if len(want) != 0 {
		t.Fatalf("missing adapter specs: %v", want)
	}
	if _, ok := Lookup(profilev1.AdapterKind(99)); ok {
		t.Fatal("unknown adapter kind was accepted")
	}
}

func TestTemplateExpansionRejectsCallerDefinedNames(t *testing.T) {
	spec, _ := Lookup(profilev1.AdapterKind_ADAPTER_KIND_JOINT_POSITION)
	valid := "/model/{model}/joint/{joint}/{axis}/cmd_pos"
	if err := ValidateTemplate(valid, spec.CommandTokens); err != nil {
		t.Fatalf("valid template: %v", err)
	}
	got, err := ExpandTemplate(valid, map[string]string{
		"model": "robot", "joint": "HeadYaw", "axis": "0",
	})
	if err != nil || got != "/model/robot/joint/HeadYaw/0/cmd_pos" {
		t.Fatalf("ExpandTemplate = %q, %v", got, err)
	}
	for _, value := range []string{
		"/model/{caller_topic}",
		"/model/{model}/../secret",
		"relative/{model}",
		"/model/{model}/{joint",
	} {
		if err := ValidateTemplate(value, spec.CommandTokens); err == nil {
			t.Errorf("unsafe template %q was accepted", value)
		}
	}
}
