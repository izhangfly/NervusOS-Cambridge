package sdk

import (
	"testing"

	"google.golang.org/protobuf/proto"

	ipcv1 "github.com/nervus-os/nervus-ipc/protocol/ipcv1"
)

// Without a Selector, the two convenience fields type/role are used.
func TestResolveRequest_SelectorFallsBackToTypeRole(t *testing.T) {
	req := ResolveRequest{
		InterfaceID:  "nervus.interface.motion.base",
		ResourceType: "nervus.resource.motion.base",
		ResourceRole: "main",
	}
	got := req.selector()
	want := &ipcv1.ResourceSelector{
		Type: "nervus.resource.motion.base", Role: "main",
	}
	if !proto.Equal(got, want) {
		t.Fatalf("selector = %+v, want %+v", got, want)
	}
}

// When both are empty no selector is sent: the semantics of an empty selector (falling back to the interface's default resource)
// are nervud's to decide, and the SDK shouldn't build an empty shell for it.
func TestResolveRequest_EmptyStaysNil(t *testing.T) {
	if got := (ResolveRequest{InterfaceID: "x"}).selector(); got != nil {
		t.Fatalf("selector = %+v, want nil", got)
	}
}

// A given Selector **completely replaces** the convenience fields, with no merging. Merging would create
// semantics like "three places filled in, which one wins?" that you'd need the implementation to understand.
func TestResolveRequest_SelectorOverridesConvenienceFields(t *testing.T) {
	explicit := &ipcv1.ResourceSelector{
		Type:   "nervus.resource.camera",
		Labels: map[string]string{"nervus.camera.facing": "front"},
		Policy: ipcv1.ResourceSelectionPolicy_RESOURCE_SELECTION_POLICY_SYSTEM_PREFERRED,
	}
	req := ResolveRequest{
		InterfaceID:  "nervus.interface.camera",
		ResourceType: "should.be.ignored",
		ResourceRole: "ignored",
		Selector:     explicit,
	}
	got := req.selector()
	if !proto.Equal(got, explicit) {
		t.Fatalf("selector = %+v, want %+v", got, explicit)
	}
	if got.GetType() == "should.be.ignored" {
		t.Fatal("the convenience fields polluted the explicit Selector")
	}
}

// Labels and policy must reach the wire unchanged; that's the whole basis for Go services being able to use V2-2.
func TestResolveRequest_LabelsAndPolicySurvive(t *testing.T) {
	req := ResolveRequest{
		InterfaceID: "nervus.interface.camera",
		Selector: &ipcv1.ResourceSelector{
			Type: "nervus.resource.camera",
			Labels: map[string]string{
				"nervus.camera.facing": "front",
				"nervus.camera.class":  "4k",
			},
			Policy: ipcv1.ResourceSelectionPolicy_RESOURCE_SELECTION_POLICY_REQUIRE_UNIQUE,
		},
	}
	got := req.selector()
	if len(got.GetLabels()) != 2 ||
		got.GetLabels()["nervus.camera.facing"] != "front" ||
		got.GetLabels()["nervus.camera.class"] != "4k" {
		t.Fatalf("labels = %+v", got.GetLabels())
	}
	if got.GetPolicy() != ipcv1.ResourceSelectionPolicy_RESOURCE_SELECTION_POLICY_REQUIRE_UNIQUE {
		t.Fatalf("policy = %v", got.GetPolicy())
	}
}
