package registry_test

import (
	"testing"

	pkgv1 "github.com/nervus-os/nervus-ipc/protocol/interface/pkgmanagerv1"
	ipcv1 "github.com/nervus-os/nervus-ipc/protocol/ipcv1"
	"github.com/nervus-os/nervus-ipc/registry"
)

// When an interface is added, the Method Registry should include it **automatically**: OEMs/the platform only attach (method_meta)
// in their own .proto, and nervud doesn't change one line of Go.
//
// This test is the executable evidence for that: PackageManager is an interface added this round, the extraction code
// didn't change by one character, and its method metadata came out.
func TestExtractMethodMetas_PackageManager(t *testing.T) {
	metas, err := registry.ExtractMethodMetas(
		pkgv1.PackageManagerMethod(0).Descriptor(),
	)
	if err != nil {
		t.Fatalf("ExtractMethodMetas: %v", err)
	}
	if len(metas) != 5 {
		t.Fatalf("extracted %d method_meta, want 5", len(metas))
	}

	byID := make(map[uint32]*ipcv1.MethodMeta, len(metas))
	for _, m := range metas {
		byID[m.GetMethodId()] = m
	}

	// Install: long task + highest risk + user confirmation required
	install := byID[1]
	if install == nil {
		t.Fatal("missing method_id=1 (Install)")
	}
	if !install.GetReturnsOperation() {
		t.Error("Install must be an operation: unpacking + re-checking every digest goes far beyond an ordinary deadline budget")
	}
	if install.GetRiskClass() != ipcv1.RiskClass_RISK_CLASS_CRITICAL_SAFETY {
		t.Errorf("Install risk_class = %v, want CRITICAL_SAFETY (it brings in new executable code, with far more lasting consequences than one motion command)",
			install.GetRiskClass())
	}
	if !install.GetNeedsUserConfirmation() {
		t.Error("Install must require the system confirmation dialog: an Agent can only request, not confirm by itself")
	}
	// Installing packages doesn't drive actuators
	if install.GetIsMotion() {
		t.Error("Install is_motion must be false")
	}
	if install.GetRequiresControlLease() {
		t.Error("Install doesn't need a ControlLease: it doesn't touch actuators")
	}

	// List: read-only, no confirmation dialog needed
	list := byID[3]
	if list == nil {
		t.Fatal("missing method_id=3 (List)")
	}
	if !list.GetIsReadOnly() {
		t.Error("List must be marked is_read_only")
	}
	if list.GetNeedsUserConfirmation() {
		t.Error("a read-only query should not show a confirmation dialog")
	}
	if list.GetRiskClass() != ipcv1.RiskClass_RISK_CLASS_NORMAL {
		t.Errorf("List risk_class = %v, want NORMAL", list.GetRiskClass())
	}

	// Inspect: a read-only pre-check that installs nothing
	//
	// It's the source of the install confirmation screen's information: the screen must first know which sensitive permissions the package requests
	// before laying them out for the user. So it **must never** show a confirmation dialog: asking the user again about "whether to look at what this package
	// requests" is a pointless loop.
	inspect := byID[5]
	if inspect == nil {
		t.Fatal("missing method_id=5 (Inspect)")
	}
	if !inspect.GetIsReadOnly() {
		t.Error("Inspect must be marked is_read_only: it only parses, never commits")
	}
	if inspect.GetNeedsUserConfirmation() {
		t.Error("Inspect should not show a confirmation dialog: it exists to provide the dialog's content")
	}
	if inspect.GetRiskClass() != ipcv1.RiskClass_RISK_CLASS_NORMAL {
		t.Errorf("Inspect risk_class = %v, want NORMAL", inspect.GetRiskClass())
	}
	if inspect.GetReturnsOperation() {
		t.Error("Inspect is not an operation: it only reads the manifest and verifies the signature, without checking every digest")
	}

	// Every write operation must require confirmation. 5 (Inspect) isn't one: it's read-only
	for _, id := range []uint32{1, 2, 4} {
		if m := byID[id]; m != nil && !m.GetNeedsUserConfirmation() {
			t.Errorf("method_id=%d is a write operation and must have needs_user_confirmation", id)
		}
	}
}
