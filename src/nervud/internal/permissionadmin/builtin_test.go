package permissionadmin

import (
	"testing"

	permissionv1 "github.com/nervus-os/nervus-ipc/protocol/interface/permissionv1"
	ipcv1 "github.com/nervus-os/nervus-ipc/protocol/ipcv1"
	"google.golang.org/protobuf/proto"

	"github.com/nervus-os/nervud/internal/catalog"
	"github.com/nervus-os/nervud/internal/endpoint"
	"github.com/nervus-os/nervud/internal/permission"
	"github.com/nervus-os/nervud/internal/pkgregistry"
)

// Three kernel bootstrap permissions, each representing one kind:
//
//	permStorageUser / permMotionControl  USER_CONSENT: has runtime state, should appear
//	permPkgQuery                         NORMAL: no runtime state, shouldn't appear
const (
	permStorageUser   = "perm.storage.user"
	permMotionControl = "perm.motion.control"
	permPkgQuery      = "perm.pkg.query"
)

type fakeLister struct{ entries []pkgregistry.Entry }

func (f *fakeLister) List() []pkgregistry.Entry { return f.entries }

type fakeGrants struct {
	states map[[2]string]permission.GrantState
	setErr error
	sets   [][3]string
	// exempt is the "system software" set, matching permission.Grant.ConsentExempt.
	//
	// It must be modelled, not just the runtime state: exemption and grant are two fields on the wire
	// (state and effective_granted), and they disagree only when exempt. A fake that doesn't model
	// the exemption can never test that divergence, and that's exactly the bug this package once had.
	exempt map[string]struct{}
}

func (f *fakeGrants) GrantStateOf(pkg, perm string) permission.GrantState {
	if f.states == nil {
		return permission.GrantStateNotRequested
	}
	return f.states[[2]string{pkg, perm}]
}

// AllowedAt is the shared source of permission.self's granted and ListGrants' effective_granted.
//
// The fake decides by "exempt or runtime state", the same shape as the USER_CONSENT branch of the real Registry.AllowedAt:
// this package's tests only use USER_CONSENT permissions, and the install-time set and grant mode checks
// belong to internal/permission's tests. Copying them here would only let the fake and the real implementation drift
// together.
//
// The exemption branch must be modelled: it's the only place state and effective_granted can disagree.
func (f *fakeGrants) AllowedAt(_ *catalog.Snapshot, pkg, perm string) bool {
	if _, ok := f.exempt[pkg]; ok {
		return true
	}
	return f.GrantStateOf(pkg, perm) == permission.GrantStateGranted
}

func (f *fakeGrants) SetRuntimeState(pkg, perm string, state permission.GrantState) error {
	f.sets = append(f.sets, [3]string{pkg, perm, string(rune('0' + int(state)))})
	if f.setErr != nil {
		return f.setErr
	}
	if f.states == nil {
		f.states = map[[2]string]permission.GrantState{}
	}
	f.states[[2]string{pkg, perm}] = state
	return nil
}

func entry(packageID, label string, granted ...string) pkgregistry.Entry {
	return pkgregistry.Entry{
		Manifest:           pkgregistry.Manifest{PackageID: packageID, Label: label},
		GrantedPermissions: granted,
	}
}

func newTestModule(t *testing.T, lister *fakeLister, grants *fakeGrants) *Module {
	t.Helper()
	definitions, err := catalog.NewDefaultRegistry()
	if err != nil {
		t.Fatalf("new catalog: %v", err)
	}
	return New(definitions, lister, grants, nil)
}

func callList(t *testing.T, m *Module, req *permissionv1.ListGrantsRequest) *permissionv1.GrantList {
	t.Helper()
	payload, err := proto.Marshal(req)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	res := m.BuiltinHandler()(endpoint.BuiltinCall{MethodID: MethodListGrants, Payload: payload})
	if res.Code != ipcv1.StatusCode_STATUS_CODE_OK {
		t.Fatalf("ListGrants code = %v, want OK", res.Code)
	}
	out := &permissionv1.GrantList{}
	if err := proto.Unmarshal(res.Payload, out); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	return out
}

// TestListGrants_OnlyUserConsent pins "only switches that can move appear in the list".
//
// Non-USER_CONSENT permissions have no runtime state, so listing them gives a row of switches that won't move;
// listing a package with no grantable permission gives an entry that opens onto nothing.
func TestListGrants_OnlyUserConsent(t *testing.T) {
	lister := &fakeLister{entries: []pkgregistry.Entry{
		entry("com.example.cam", "Camera", permStorageUser, permPkgQuery),
		entry("com.example.plain", "Query only", permPkgQuery), // not a single USER_CONSENT
	}}
	m := newTestModule(t, lister, &fakeGrants{})

	out := callList(t, m, &permissionv1.ListGrantsRequest{})

	if len(out.GetPackages()) != 1 {
		t.Fatalf("packages = %d, want 1 (the query-only package shouldn't appear)", len(out.GetPackages()))
	}
	pkg := out.GetPackages()[0]
	if pkg.GetPackageId() != "com.example.cam" || pkg.GetLabel() != "Camera" {
		t.Fatalf("package = %q/%q", pkg.GetPackageId(), pkg.GetLabel())
	}
	if len(pkg.GetPermissions()) != 1 {
		t.Fatalf("permissions = %+v, want only %s", pkg.GetPermissions(), permStorageUser)
	}
	got := pkg.GetPermissions()[0]
	if got.GetPermissionId() != permStorageUser {
		t.Fatalf("permission = %q, want %q", got.GetPermissionId(), permStorageUser)
	}
	// The text comes from the kernel: the UI can't know the text of third-party packages' own permissions in advance
	if got.GetDisplayName().GetEn() == "" || got.GetDescription().GetEn() == "" {
		t.Fatalf("English text is empty: name=%q desc=%q",
			got.GetDisplayName().GetEn(), got.GetDescription().GetEn())
	}
	if got.GetDisplayName().GetEn() == got.GetDescription().GetEn() {
		t.Fatalf("title and description are the same (%q): no explanation on the consent screen",
			got.GetDisplayName().GetEn())
	}
}

// TestListGrants_NeverRequestedIsNotUnspecified pins the fact that the two enums have different zero values.
//
// The kernel's zero is NotRequested, the wire's zero is UNSPECIFIED. A direct numeric conversion would send
// "never asked the user" as "not filled in", and the UI treats those two completely differently.
func TestListGrants_NeverRequestedIsNotUnspecified(t *testing.T) {
	lister := &fakeLister{entries: []pkgregistry.Entry{
		entry("com.example.cam", "Camera", permStorageUser),
	}}
	m := newTestModule(t, lister, &fakeGrants{}) // empty states = never asked

	out := callList(t, m, &permissionv1.ListGrantsRequest{})
	state := out.GetPackages()[0].GetPermissions()[0].GetState()
	if state != permissionv1.GrantState_GRANT_STATE_NOT_REQUESTED {
		t.Fatalf("state = %v, want NOT_REQUESTED", state)
	}
}

// TestListGrants_SystemSoftwareIsEffectivelyGranted pins that state and
// effective_granted must disagree for system software.
//
// This is a bug the package once had: the UI looked only at state, and system software goes through the consent exemption.
// The exemption fakes no grant record, so state is always NOT_REQUESTED while AllowedAt is always true.
// So the file manager's "user files" switch showed off while it could actually read and write the user directory:
// the switch contradicted reality.
//
// Each field is pinned separately; checking only one isn't enough. Checking only effective_granted would miss a "fix" that
// also changes state to GRANTED, which would erase the fact of "what the user decided", and
// ordinary apps rely on that to tell "never asked" from "asked and refused".
func TestListGrants_SystemSoftwareIsEffectivelyGranted(t *testing.T) {
	const systemPkg = "nervus.filemanager"
	lister := &fakeLister{entries: []pkgregistry.Entry{
		entry(systemPkg, "Files", permStorageUser),
	}}
	m := newTestModule(t, lister, &fakeGrants{
		// Exempt, and with no runtime grant record: exactly the state of real system software
		exempt: map[string]struct{}{systemPkg: {}},
	})

	got := callList(t, m, &permissionv1.ListGrantsRequest{}).
		GetPackages()[0].GetPermissions()[0]

	if !got.GetEffectiveGranted() {
		t.Fatalf("effective_granted = false, want true: " +
			"system software goes through the consent exemption and really has access; the UI shows the switch from this field")
	}
	if got.GetState() != permissionv1.GrantState_GRANT_STATE_NOT_REQUESTED {
		t.Fatalf("state = %v, want NOT_REQUESTED: the exemption doesn't fake a grant record; "+
			"changing that loses the fact of 'what the user decided'", got.GetState())
	}
}

// TestListGrants_EffectiveGrantedTracksUserDecision: for an ordinary non-exempt app the two fields
// agree. It has no exemption, so whether it can use the permission equals whether the user agreed.
func TestListGrants_EffectiveGrantedTracksUserDecision(t *testing.T) {
	const appPkg = "com.example.cam"
	lister := &fakeLister{entries: []pkgregistry.Entry{
		entry(appPkg, "Camera", permStorageUser),
	}}

	for _, tc := range []struct {
		name  string
		state permission.GrantState
		want  bool
	}{
		{"never asked", permission.GrantStateNotRequested, false},
		{"user agreed", permission.GrantStateGranted, true},
		{"user refused", permission.GrantStateDenied, false},
		{"refused permanently", permission.GrantStateDeniedPermanent, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newTestModule(t, lister, &fakeGrants{
				states: map[[2]string]permission.GrantState{
					{appPkg, permStorageUser}: tc.state,
				},
			})
			got := callList(t, m, &permissionv1.ListGrantsRequest{}).
				GetPackages()[0].GetPermissions()[0]
			if got.GetEffectiveGranted() != tc.want {
				t.Fatalf("effective_granted = %v, want %v",
					got.GetEffectiveGranted(), tc.want)
			}
		})
	}
}

// TestListGrants_FilterAndOrder: the package_id filter works and the output order is deterministic;
// a nondeterministic order makes the entries in Settings jump on every refresh.
func TestListGrants_FilterAndOrder(t *testing.T) {
	lister := &fakeLister{entries: []pkgregistry.Entry{
		entry("com.z.app", "Z", permStorageUser),
		entry("com.a.app", "A", permMotionControl, permStorageUser),
	}}
	m := newTestModule(t, lister, &fakeGrants{})

	all := callList(t, m, &permissionv1.ListGrantsRequest{})
	if len(all.GetPackages()) != 2 {
		t.Fatalf("packages = %d, want 2", len(all.GetPackages()))
	}
	if all.GetPackages()[0].GetPackageId() != "com.a.app" {
		t.Fatalf("packages not in lexicographic order: %q first", all.GetPackages()[0].GetPackageId())
	}
	perms := all.GetPackages()[0].GetPermissions()
	if len(perms) != 2 || perms[0].GetPermissionId() != permMotionControl {
		t.Fatalf("permissions not in lexicographic order: %+v", perms)
	}

	only := callList(t, m, &permissionv1.ListGrantsRequest{PackageId: "com.z.app"})
	if len(only.GetPackages()) != 1 || only.GetPackages()[0].GetPackageId() != "com.z.app" {
		t.Fatalf("filter didn't work: %+v", only.GetPackages())
	}
}

// TestSetGrantState_RejectsUnsettableStates: UNSPECIFIED means not filled in, NOT_REQUESTED means
// "back to never asked", which isn't a decision the user can make. To take it back, use DENIED.
func TestSetGrantState_RejectsUnsettableStates(t *testing.T) {
	grants := &fakeGrants{}
	m := newTestModule(t, &fakeLister{}, grants)

	for _, state := range []permissionv1.GrantState{
		permissionv1.GrantState_GRANT_STATE_UNSPECIFIED,
		permissionv1.GrantState_GRANT_STATE_NOT_REQUESTED,
	} {
		payload, err := proto.Marshal(&permissionv1.SetGrantStateRequest{
			PackageId: "com.example.cam", PermissionId: permStorageUser, State: state,
		})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		res := m.BuiltinHandler()(endpoint.BuiltinCall{
			MethodID: MethodSetGrantState, Payload: payload})
		if res.Code != ipcv1.StatusCode_STATUS_CODE_INVALID_ARGUMENT {
			t.Fatalf("state=%v code = %v, want INVALID_ARGUMENT", state, res.Code)
		}
	}
	if len(grants.sets) != 0 {
		t.Fatalf("no state should be stored, got %+v", grants.sets)
	}
}

// TestSetGrantState_ReturnsActualState: what comes back is the current fact, not the value in the request.
func TestSetGrantState_ReturnsActualState(t *testing.T) {
	grants := &fakeGrants{}
	m := newTestModule(t, &fakeLister{}, grants)

	payload, err := proto.Marshal(&permissionv1.SetGrantStateRequest{
		PackageId:    "com.example.cam",
		PermissionId: permStorageUser,
		State:        permissionv1.GrantState_GRANT_STATE_GRANTED,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	res := m.BuiltinHandler()(endpoint.BuiltinCall{
		MethodID: MethodSetGrantState, Payload: payload})
	if res.Code != ipcv1.StatusCode_STATUS_CODE_OK {
		t.Fatalf("code = %v, want OK", res.Code)
	}
	out := &permissionv1.SetGrantStateResponse{}
	if err := proto.Unmarshal(res.Payload, out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.GetState() != permissionv1.GrantState_GRANT_STATE_GRANTED {
		t.Fatalf("state = %v, want GRANTED", out.GetState())
	}
}

// TestBuiltin_FailsClosedWithoutDependencies: UNAVAILABLE whenever dependencies aren't wired.
// An empty list would make the UI show "can't look it up" as "no permissions need granting".
func TestBuiltin_FailsClosedWithoutDependencies(t *testing.T) {
	m := New(nil, nil, nil, nil)
	for _, method := range []uint32{MethodListGrants, MethodSetGrantState} {
		res := m.BuiltinHandler()(endpoint.BuiltinCall{MethodID: method})
		if res.Code != ipcv1.StatusCode_STATUS_CODE_UNAVAILABLE {
			t.Fatalf("method %d code = %v, want UNAVAILABLE", method, res.Code)
		}
	}
}

// TestBuiltin_UnknownMethodIsNotFound: an unimplemented method doesn't exist.
func TestBuiltin_UnknownMethodIsNotFound(t *testing.T) {
	m := newTestModule(t, &fakeLister{}, &fakeGrants{})
	res := m.BuiltinHandler()(endpoint.BuiltinCall{MethodID: 9999})
	if res.Code != ipcv1.StatusCode_STATUS_CODE_NOT_FOUND {
		t.Fatalf("code = %v, want NOT_FOUND", res.Code)
	}
}
