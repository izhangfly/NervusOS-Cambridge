package permissionadmin

import (
	"testing"

	permissionv1 "github.com/nervus-os/nervus-ipc/protocol/interface/permissionv1"
	ipcv1 "github.com/nervus-os/nervus-ipc/protocol/ipcv1"
	"google.golang.org/protobuf/proto"

	"github.com/nervus-os/nervud/internal/endpoint"
	"github.com/nervus-os/nervud/internal/identity"
	"github.com/nervus-os/nervud/internal/permission"
	"github.com/nervus-os/nervud/internal/pkgregistry"
)

// rawCheck sends one Check and returns the result as is, for cases that assert on the error code.
func rawCheck(
	t *testing.T, m *Module, caller identity.Caller, req *permissionv1.CheckSelfPermissionRequest,
) endpoint.BuiltinResult {
	t.Helper()
	payload, err := proto.Marshal(req)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	return m.SelfBuiltinHandler()(endpoint.BuiltinCall{
		MethodID: MethodCheckSelf,
		Payload:  payload,
		Caller:   caller,
	})
}

// callCheck sends one Check, requires success, and indexes the results by permission ID.
//
// callerPackage is the caller identity the kernel recognised; this interface's target package is decided entirely by it,
// and the request has no package_id field.
//
// Indexed by ID rather than position: duplicates in the request are deduplicated, so positions stop lining up.
func callCheck(
	t *testing.T, m *Module, callerPackage string, permissionIDs ...string,
) map[string]*permissionv1.SelfPermissionState {
	t.Helper()
	res := rawCheck(t, m, identity.Caller{PackageID: callerPackage},
		&permissionv1.CheckSelfPermissionRequest{PermissionIds: permissionIDs})
	if res.Code != ipcv1.StatusCode_STATUS_CODE_OK {
		t.Fatalf("Check code = %v, want OK", res.Code)
	}
	out := &permissionv1.CheckSelfPermissionResult{}
	if err := proto.Unmarshal(res.Payload, out); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	byID := make(map[string]*permissionv1.SelfPermissionState, len(out.GetStates()))
	for _, s := range out.GetStates() {
		byID[s.GetPermissionId()] = s
	}
	return byID
}

func grantedState(pkg, perm string, state permission.GrantState) *fakeGrants {
	return &fakeGrants{states: map[[2]string]permission.GrantState{
		{pkg, perm}: state,
	}}
}

// TestCheckSelf_GrantedFollowsRuntimeState pins that granted follows the runtime state.
//
// This is the basic use of the self-check: if the user turns one off in permission management, the app's next self-check should see false,
// rather than carrying on as if "I have the permission" and being knocked back by PERMISSION_DENIED.
func TestCheckSelf_GrantedFollowsRuntimeState(t *testing.T) {
	lister := &fakeLister{entries: []pkgregistry.Entry{
		entry("com.example.cam", "Camera", permStorageUser),
	}}
	grants := grantedState("com.example.cam", permStorageUser, permission.GrantStateGranted)
	m := newTestModule(t, lister, grants)

	got := callCheck(t, m, "com.example.cam", permStorageUser)[permStorageUser]
	if got == nil {
		t.Fatal("missing the result for " + permStorageUser)
	}
	if !got.GetGranted() {
		t.Error("granted = false, want true (the runtime state is Granted)")
	}
	if got.GetState() != permissionv1.GrantState_GRANT_STATE_GRANTED {
		t.Errorf("state = %v, want GRANTED", got.GetState())
	}
	// Already held; requesting again only shows a pointless prompt
	if got.GetRequestable() {
		t.Error("requestable = true, want false (already Granted)")
	}
}

// TestCheckSelf_NotRequestedIsRequestable: never asked + in the install-time set =
// exactly when it should be requested. This is the entry condition of the Android-style flow.
func TestCheckSelf_NotRequestedIsRequestable(t *testing.T) {
	lister := &fakeLister{entries: []pkgregistry.Entry{
		entry("com.example.cam", "Camera", permStorageUser),
	}}
	m := newTestModule(t, lister, &fakeGrants{})

	got := callCheck(t, m, "com.example.cam", permStorageUser)[permStorageUser]
	if got.GetGranted() {
		t.Error("granted = true, want false (the user hasn't been asked yet)")
	}
	if got.GetState() != permissionv1.GrantState_GRANT_STATE_NOT_REQUESTED {
		t.Errorf("state = %v, want NOT_REQUESTED", got.GetState())
	}
	if !got.GetRequestable() {
		t.Error("requestable = false, want true (requesting it makes sense)")
	}
}

// TestCheckSelf_RequestableFalseCases pins the cases where "a request is bound to fail", each with its reason.
//
// This field exists to break a loop: without it, an app seeing granted=false can only
// request, and for these cases the request is bound to fail without changing granted, so it retries: a loop that starts
// spinning at boot and may show a prompt every time.
func TestCheckSelf_RequestableFalseCases(t *testing.T) {
	const pkg = "com.example.app"

	tests := []struct {
		name       string
		granted    []string // what was actually granted at install time
		state      permission.GrantState
		permission string
		wantState  permissionv1.GrantState
	}{
		{
			// The install-time decision refused it (not enough trust / wrong signing role), or it was never requested in the manifest.
			// The user saying yes can't conjure it up; SetRuntimeState has this check built in
			name:       "not in the install-time set",
			granted:    []string{permStorageUser},
			permission: permMotionControl,
			wantState:  permissionv1.GrantState_GRANT_STATE_NOT_REQUESTED,
		},
		{
			// The user refused permanently: no prompt appears and the result won't change. The user should be pointed to
			// permission management instead of retrying
			name:       "refused permanently",
			granted:    []string{permStorageUser},
			state:      permission.GrantStateDeniedPermanent,
			permission: permStorageUser,
			wantState:  permissionv1.GrantState_GRANT_STATE_DENIED_PERMANENT,
		},
		{
			// NORMAL takes effect on install and has no runtime state. state stays UNSPECIFIED rather than
			// NOT_REQUESTED, which would make the app think "ask and you'll get it"
			name:       "not USER_CONSENT",
			granted:    []string{permPkgQuery},
			permission: permPkgQuery,
			wantState:  permissionv1.GrantState_GRANT_STATE_UNSPECIFIED,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			lister := &fakeLister{entries: []pkgregistry.Entry{
				entry(pkg, "App", tc.granted...),
			}}
			grants := grantedState(pkg, tc.permission, tc.state)
			m := newTestModule(t, lister, grants)

			got := callCheck(t, m, pkg, tc.permission)[tc.permission]
			if got == nil {
				t.Fatalf("missing the result for %s", tc.permission)
			}
			if got.GetRequestable() {
				t.Error("requestable = true, want false")
			}
			if got.GetState() != tc.wantState {
				t.Errorf("state = %v, want %v", got.GetState(), tc.wantState)
			}
		})
	}
}

// TestCheckSelf_TargetIsAlwaysTheCaller pins "you can only check yourself".
//
// The target package is decided by BuiltinCall.Caller; the request has no package_id. So the same request sent by
// two different callers gets each its own answer, and one package has no way to peek at another's grant state.
//
// It isn't just privacy: the permission layout is reconnaissance. "Which app on this machine has the camera and
// motion control" is directly valuable to an attacker.
func TestCheckSelf_TargetIsAlwaysTheCaller(t *testing.T) {
	lister := &fakeLister{entries: []pkgregistry.Entry{
		entry("com.example.has", "Has it", permStorageUser),
		entry("com.example.none", "Doesn't have it"),
	}}
	grants := grantedState("com.example.has", permStorageUser, permission.GrantStateGranted)
	m := newTestModule(t, lister, grants)

	mine := callCheck(t, m, "com.example.has", permStorageUser)[permStorageUser]
	if !mine.GetGranted() {
		t.Error("the holder's self-check has granted = false, want true")
	}

	// Same permission, different caller: it sees its own state, not someone else's
	theirs := callCheck(t, m, "com.example.none", permStorageUser)[permStorageUser]
	if theirs.GetGranted() {
		t.Error("another package sees granted = true; it must not see someone else's grant state")
	}
	if theirs.GetRequestable() {
		t.Error("another package sees requestable = true, want false (it didn't get this at install time)")
	}
}

// TestCheckSelf_UnknownPermissionDoesNotFailTheCall: one misspelt permission name shouldn't stop the app from checking
// the others. Return a negative result rather than failing the whole call.
func TestCheckSelf_UnknownPermissionDoesNotFailTheCall(t *testing.T) {
	lister := &fakeLister{entries: []pkgregistry.Entry{
		entry("com.example.cam", "Camera", permStorageUser),
	}}
	grants := grantedState("com.example.cam", permStorageUser, permission.GrantStateGranted)
	m := newTestModule(t, lister, grants)

	got := callCheck(t, m, "com.example.cam", "perm.does.not.exist", permStorageUser)

	unknown := got["perm.does.not.exist"]
	if unknown == nil {
		t.Fatal("an undefined permission should also get a result")
	}
	if unknown.GetGranted() || unknown.GetRequestable() {
		t.Errorf("undefined permission granted=%v requestable=%v, both should be false",
			unknown.GetGranted(), unknown.GetRequestable())
	}
	// The key point: the valid one in the same call still got its answer
	if real := got[permStorageUser]; real == nil || !real.GetGranted() {
		t.Error("the valid permission was dragged down by the undefined one")
	}
}

// TestCheckSelf_DeduplicatesAndSorts: duplicates are answered once, in a deterministic order.
func TestCheckSelf_DeduplicatesAndSorts(t *testing.T) {
	lister := &fakeLister{entries: []pkgregistry.Entry{
		entry("com.example.app", "App", permStorageUser, permMotionControl),
	}}
	m := newTestModule(t, lister, &fakeGrants{})

	res := rawCheck(t, m, identity.Caller{PackageID: "com.example.app"},
		&permissionv1.CheckSelfPermissionRequest{PermissionIds: []string{
			permStorageUser, permMotionControl, permStorageUser, "", permStorageUser,
		}})
	if res.Code != ipcv1.StatusCode_STATUS_CODE_OK {
		t.Fatalf("code = %v, want OK", res.Code)
	}
	out := &permissionv1.CheckSelfPermissionResult{}
	if err := proto.Unmarshal(res.Payload, out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if len(out.GetStates()) != 2 {
		t.Fatalf("states = %d, want 2 (after deduplication, empty strings dropped)", len(out.GetStates()))
	}
	// Deterministic order: motion < storage
	if a, b := out.GetStates()[0].GetPermissionId(), out.GetStates()[1].GetPermissionId(); a > b {
		t.Errorf("results not sorted by permission ID: %q before %q", a, b)
	}
}

// TestCheckSelf_EmptyRequestIsOK: an app that requests no sensitive permission can still call it
// (usually generic startup code that doesn't care how many permissions there are). An empty list gets an empty list, not an error.
func TestCheckSelf_EmptyRequestIsOK(t *testing.T) {
	m := newTestModule(t, &fakeLister{}, &fakeGrants{})

	res := rawCheck(t, m, identity.Caller{PackageID: "com.example.app"},
		&permissionv1.CheckSelfPermissionRequest{})
	if res.Code != ipcv1.StatusCode_STATUS_CODE_OK {
		t.Fatalf("code = %v, want OK", res.Code)
	}
	out := &permissionv1.CheckSelfPermissionResult{}
	if err := proto.Unmarshal(res.Payload, out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(out.GetStates()) != 0 {
		t.Errorf("states = %d, want 0", len(out.GetStates()))
	}
}

// TestCheckSelf_RejectsUnauthenticatedCaller pins the fail-closed direction.
//
// An empty PackageID must be refused rather than treated as an "anonymous caller": that would make the whole answer a query about a
// package that doesn't exist, every entry granted=false, which looks like a normal negative answer while the
// real cause is that the handshake didn't recognise the caller.
func TestCheckSelf_RejectsUnauthenticatedCaller(t *testing.T) {
	m := newTestModule(t, &fakeLister{}, &fakeGrants{})

	res := rawCheck(t, m, identity.Caller{},
		&permissionv1.CheckSelfPermissionRequest{PermissionIds: []string{permStorageUser}})
	if res.Code != ipcv1.StatusCode_STATUS_CODE_UNAUTHENTICATED {
		t.Fatalf("code = %v, want UNAUTHENTICATED", res.Code)
	}
}

// TestCheckSelf_RejectsOversizedRequest: this method has no bar and any app can call it,
// and each entry costs a lookup of the Catalog definition and grant state. Without a cap, packing a hundred thousand
// permission IDs into one request would be a cheap amplification attack.
func TestCheckSelf_RejectsOversizedRequest(t *testing.T) {
	m := newTestModule(t, &fakeLister{}, &fakeGrants{})

	ids := make([]string, maxCheckPermissions+1)
	for i := range ids {
		ids[i] = permStorageUser
	}
	res := rawCheck(t, m, identity.Caller{PackageID: "com.example.app"},
		&permissionv1.CheckSelfPermissionRequest{PermissionIds: ids})
	if res.Code != ipcv1.StatusCode_STATUS_CODE_INVALID_ARGUMENT {
		t.Fatalf("code = %v, want INVALID_ARGUMENT", res.Code)
	}
}

// TestCheckSelf_UnknownMethodIsNotFound: fail closed; an unimplemented method doesn't exist.
func TestCheckSelf_UnknownMethodIsNotFound(t *testing.T) {
	m := newTestModule(t, &fakeLister{}, &fakeGrants{})

	res := m.SelfBuiltinHandler()(endpoint.BuiltinCall{
		MethodID: 9999,
		Caller:   identity.Caller{PackageID: "com.example.app"},
	})
	if res.Code != ipcv1.StatusCode_STATUS_CODE_NOT_FOUND {
		t.Fatalf("code = %v, want NOT_FOUND", res.Code)
	}
}
