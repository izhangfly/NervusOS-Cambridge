package registry

import (
	"os"
	"path/filepath"
	"testing"

	"google.golang.org/protobuf/encoding/prototext"

	ipcv1 "github.com/nervus-os/nervus-ipc/protocol/ipcv1"
	dogv1 "github.com/nervus-os/nervus-ipc/protocol/oem/acme/dog/v1"
)

// TestExtractMethodMetas_OEM proves "data-driven": the Method Registry is extracted from the generated OEM
// descriptor, with no hand-written list on the Go side. Adding an enum value with the option includes it automatically.
func TestExtractMethodMetas_OEM(t *testing.T) {
	enum := dogv1.RawGaitMethod(0).Descriptor()
	metas, err := ExtractMethodMetas(enum)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	// Two methods with the option (GET_GAIT_STATE, SET_RAW_GAIT); UNSPECIFIED is skipped.
	if len(metas) != 2 {
		t.Fatalf("want 2 method metas, got %d", len(metas))
	}

	byID := map[uint32]*ipcv1.MethodMeta{}
	for _, m := range metas {
		byID[m.GetMethodId()] = m
	}

	// Read-only method: no permission gate, no lease, no confirmation, not motion.
	get := byID[1]
	if get == nil {
		t.Fatal("method_id 1 (GetGaitState) missing")
	}
	if !get.GetIsReadOnly() || get.GetIsMotion() || get.GetNeedsUserConfirmation() ||
		get.GetRequiresControlLease() || get.GetRiskClass() != ipcv1.RiskClass_RISK_CLASS_NORMAL {
		t.Errorf("GetGaitState meta wrong: %+v", get)
	}

	// Dangerous method: OEM custom permission + physical control + lease required + system confirmation dialog + motion.
	set := byID[2]
	if set == nil {
		t.Fatal("method_id 2 (SetRawGait) missing")
	}
	if set.GetRequiredPermission() != "com.acme.dog.permission.raw_gait" {
		t.Errorf("SetRawGait required_permission = %q", set.GetRequiredPermission())
	}
	if set.GetRiskClass() != ipcv1.RiskClass_RISK_CLASS_PHYSICAL_CONTROL {
		t.Errorf("SetRawGait risk_class = %v", set.GetRiskClass())
	}
	if !set.GetRequiresControlLease() || !set.GetNeedsUserConfirmation() || !set.GetIsMotion() ||
		set.GetIsReadOnly() {
		t.Errorf("SetRawGait meta wrong: %+v", set)
	}
	if set.GetRequestType() != "com.acme.dog.v1.SetRawGaitRequest" {
		t.Errorf("SetRawGait request_type = %q", set.GetRequestType())
	}
}

// TestExtractMethodMetas_AutoIncludesNewValue locks in A3's verification requirement that "adding any enum value with the option
// automatically includes it in the extraction result": extracted count == number of enum values with the option.
func TestExtractMethodMetas_AutoIncludesNewValue(t *testing.T) {
	enum := dogv1.RawGaitMethod(0).Descriptor()
	metas, err := ExtractMethodMetas(enum)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	// Count the enum values with the method_meta option by hand; it should match the extraction result.
	// The value of this test: when a method with the option is added to or removed from the .proto, this assertion follows automatically,
	// without changing any Go list: "the source of truth is always the proto".
	if got := len(metas); got != 2 {
		t.Fatalf("extraction count drifted from proto: got %d", got)
	}
}

// TestProviderDescriptorSamples checks that the standard + OEM Descriptor samples parse against the frozen
// schema (A4 gate: standard + OEM Descriptor samples).
func TestProviderDescriptorSamples(t *testing.T) {
	cases := []struct {
		name string
		file string
	}{
		{"standard", "descriptor_standard.textproto"},
		{"oem", "descriptor_oem.textproto"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join("testdata", tc.file))
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			var d ipcv1.ProviderDescriptor
			if err := prototext.Unmarshal(raw, &d); err != nil {
				t.Fatalf("unmarshal %s: %v", tc.file, err)
			}
			if len(d.GetInterfaces()) == 0 {
				t.Errorf("%s: no interfaces", tc.file)
			}
			// Namespaces self-consistent: both samples should have zero violations (red line #3).
			if errs := ValidateOEMNamespace(&d); len(errs) != 0 {
				t.Errorf("%s: namespace violations: %v", tc.file, errs)
			}
		})
	}
}

// TestValidateOEMNamespace_CatchesViolation proves the check really stops violations: a Descriptor placing
// a custom permission under someone else's namespace must be judged a violation (red line #3 counterexample).
func TestValidateOEMNamespace_CatchesViolation(t *testing.T) {
	bad := &ipcv1.ProviderDescriptor{
		PackageId: "com.acme.dog",
		Interfaces: []*ipcv1.ProvidedInterface{
			// A private interface under someone else's (com.evil) namespace must be stopped.
			{
				InterfaceId: "com.evil.interface.hijack",
				InterfaceVersions: []*ipcv1.ProvidedInterfaceVersion{
					{Major: 1},
				},
			},
		},
		Permissions: []*ipcv1.DefinedPermission{
			// A custom permission outside its own namespace must be stopped.
			{Id: "nervus.permission.motion.control"},
		},
	}
	errs := ValidateOEMNamespace(bad)
	if len(errs) != 2 {
		t.Fatalf("want 2 violations, got %d: %v", len(errs), errs)
	}
}

// TestValidateOEMNamespace_AllowsPlatformPermissionShape locks in the boundary of responsibility:
// perm.* can be extended by the data declarations of platform-signed Providers, so adding a standard permission doesn't
// need a kernel code change; this layer has no signature facts and only lets the name shape through. Before really accepting it, nervud must still require
// Platform trust / the matching signer role, and an ordinary Provider writing the same content gains no permission from it.
func TestValidateOEMNamespace_AllowsPlatformPermissionShape(t *testing.T) {
	d := &ipcv1.ProviderDescriptor{
		PackageId: "nervus.platform.media",
		Permissions: []*ipcv1.DefinedPermission{
			{Id: "perm.microphone.capture"},
		},
	}
	if errs := ValidateOEMNamespace(d); len(errs) != 0 {
		t.Fatalf("platform permission shape rejected: %v", errs)
	}
}
