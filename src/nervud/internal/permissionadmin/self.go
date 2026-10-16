// This file is the built-in implementation of nervus.interface.permission.self: "do I have this permission?".
//
// # The gap it fills
//
// Before calling a method that needs a sensitive permission, an app needs to be able to ask first, like Android's
// checkSelfPermission. Without it, an app has two options: call blindly and infer from
// PERMISSION_DENIED (impossible for methods with side effects), or simply assume
// it has the permission and let the feature fail silently in front of the user.
//
// # Why the same Module as .admin but a different interface_id
//
// Both read the same state (permission.Registry), and the implementer really is the same code,
// so they share one Module and one built-in component (builtin.permission).
//
// But the bars differ by an order of magnitude: .admin needs perm.permission.admin (SYSTEM_ONLY +
// platform-release signature), while self-checking has no bar. In the Catalog's model an interface_id
// has only one bar, so different bars need two interfaces.
//
// # Where the bar-free boundary is
//
// The target package is always the caller itself: the request has no package_id field, and the identity comes from
// BuiltinCall.Caller (recognised by SO_PEERCRED, not self-reported in the request). So this interface
// isn't a bar-free way in to "check any package's grant state"; that belongs to .admin.
//
// The distinction matters: the permission layout is reconnaissance. "Which app on this machine has
// the camera and motion control" is directly valuable to an attacker, while "do I have it" is already
// knowable to the caller (it can always just try a call).
package permissionadmin

import (
	"sort"

	permissionv1 "github.com/nervus-os/nervus-ipc/protocol/interface/permissionv1"
	ipcv1 "github.com/nervus-os/nervus-ipc/protocol/ipcv1"
	"google.golang.org/protobuf/proto"

	"github.com/nervus-os/nervud/internal/catalog"
	"github.com/nervus-os/nervud/internal/endpoint"
	"github.com/nervus-os/nervud/internal/identity"
	"github.com/nervus-os/nervud/internal/permission"
)

// SelfBuiltinInterfaceID is the self-check interface's ID.
//
// It must match the bootstrapInterface call in the catalog bootstrap, which sets the bar.
const SelfBuiltinInterfaceID = catalog.InterfacePermissionSelf

// Method numbers come from the generated enum values, not literals copied here: a copy would quietly go stale.
const MethodCheckSelf = uint32(
	permissionv1.PermissionSelfMethod_PERMISSION_SELF_METHOD_CHECK)

// maxCheckPermissions caps how many permissions one Check can ask about.
//
// It's capped rather than accepting anything: this method has no bar and any app can call it, and each entry costs a lookup of the
// Catalog definition and the grant state. Without a cap, packing a hundred thousand permission IDs into one request would be a
// cheap amplification attack, with the attacker paying for only one request.
//
// 100 is far above any real use (the bootstrap has only a handful of USER_CONSENT permissions),
// so it never stops a normal caller, only clearly abnormal requests.
const maxCheckPermissions = 100

// SelfBuiltinHandler returns a handler that can be passed straight to endpoint.RegisterBuiltin.
//
// It's registered separately from BuiltinHandler: they are two interface_ids, and endpoint routes by interface.
func (m *Module) SelfBuiltinHandler() endpoint.BuiltinHandler {
	return func(call endpoint.BuiltinCall) endpoint.BuiltinResult {
		switch call.MethodID {
		case MethodCheckSelf:
			return m.checkSelf(call.Caller, call.Payload)
		default:
			// Fail closed: an unimplemented method doesn't exist
			return endpoint.BuiltinResult{Code: ipcv1.StatusCode_STATUS_CODE_NOT_FOUND}
		}
	}
}

// checkSelf answers what state each of a batch of permissions is in for the caller.
//
// It reads the Catalog Snapshot once, so the whole response comes from one revision: reading several times could mix
// two revisions into the response, with unreproducible results like "in the same answer, permission A used the new definition and B the old".
func (m *Module) checkSelf(caller identity.Caller, payload []byte) endpoint.BuiltinResult {
	if !m.ready() {
		return endpoint.BuiltinResult{Code: ipcv1.StatusCode_STATUS_CODE_UNAVAILABLE}
	}
	// An empty PackageID must be refused rather than treated as an "anonymous caller": that would make the whole answer
	// "a query about a package that doesn't exist", every entry granted=false, which looks like a normal
	// negative answer while the real cause is that the handshake didn't recognise the caller. This path isn't reachable normally (endpoint only
	// hands down connections with a recognised identity), but the fail-closed direction must be written down.
	if caller.PackageID == "" {
		return endpoint.BuiltinResult{Code: ipcv1.StatusCode_STATUS_CODE_UNAUTHENTICATED}
	}

	req := &permissionv1.CheckSelfPermissionRequest{}
	if err := proto.Unmarshal(payload, req); err != nil {
		// An undecodable request is the caller's fault. Don't treat it as an empty request, which would turn an encoding error
		// into an empty answer that looks successful.
		return endpoint.BuiltinResult{Code: ipcv1.StatusCode_STATUS_CODE_INVALID_ARGUMENT}
	}
	if len(req.GetPermissionIds()) > maxCheckPermissions {
		return endpoint.BuiltinResult{Code: ipcv1.StatusCode_STATUS_CODE_INVALID_ARGUMENT}
	}

	snap := m.definitions.Current()
	if snap == nil {
		// When definitions can't be read, don't return an "all missing" list: the app would think the user refused
		// everything and head down the dead end of "send the user to Settings", while the real cause is that the kernel temporarily can't read them.
		// Catalog
		return endpoint.BuiltinResult{Code: ipcv1.StatusCode_STATUS_CODE_UNAVAILABLE}
	}

	installed := m.installedGrantSetOf(caller.PackageID)

	out := &permissionv1.CheckSelfPermissionResult{}
	seen := make(map[string]struct{}, len(req.GetPermissionIds()))
	for _, permissionID := range req.GetPermissionIds() {
		if permissionID == "" {
			continue
		}
		// Deduplicate: duplicates are answered once. Callers index results by permission_id
		// (the proto comment says not to index by position).
		if _, dup := seen[permissionID]; dup {
			continue
		}
		seen[permissionID] = struct{}{}
		out.States = append(out.States,
			m.selfStateOf(snap, caller.PackageID, permissionID, installed))
	}
	// Deterministic order, independent of the request: a stably sorted answer keeps callers' logs comparable.
	sort.Slice(out.States, func(i, j int) bool {
		return out.States[i].GetPermissionId() < out.States[j].GetPermissionId()
	})

	wire, err := proto.Marshal(out)
	if err != nil {
		if m.log != nil {
			m.log.Warn("permissionadmin: marshal self-check failed",
				"package", caller.PackageID, "err", err)
		}
		return endpoint.BuiltinResult{Code: ipcv1.StatusCode_STATUS_CODE_INTERNAL}
	}
	return endpoint.BuiltinResult{Payload: wire, Code: ipcv1.StatusCode_STATUS_CODE_OK}
}

// selfStateOf computes three facts about one permission for one package: can it be used, the runtime state, and is it still worth requesting.
func (m *Module) selfStateOf(
	snap *catalog.Snapshot,
	packageID string,
	permissionID string,
	installed map[string]struct{},
) *permissionv1.SelfPermissionState {
	state := &permissionv1.SelfPermissionState{PermissionId: permissionID}

	definition, defined := snap.Permission(permissionID)
	if !defined {
		// An undefined permission (misspelt, or from a definer that was uninstalled): return a negative result rather than
		// failing the whole call. One misspelt permission name shouldn't stop the app from checking the others.
		// requestable is false: requesting a permission that doesn't exist can never succeed.
		return state
	}

	// granted comes from AllowedAt rather than being assembled here: it's the same code as the kernel's authorisation decision,
	// and already combines the facts (whether it's in the install-time set, which grant mode, whether the USER_CONSENT
	// runtime state is Granted, and the consent exemption for system software).
	//
	// Recomputing it here would create a second source of truth: once the two disagree, the app sees
	// "the self-check says yes, the real call is refused", the hardest kind of fault to track down, because each piece of code
	// looks right on its own.
	state.Granted = m.grants.AllowedAt(snap, packageID, permissionID)

	// Only the USER_CONSENT tier has a meaningful runtime state. The others stay UNSPECIFIED rather than
	// NOT_REQUESTED: the latter would make the app think "ask and you'll get it", while NORMAL takes effect on install
	// and PRIVILEGED / SYSTEM_ONLY aren't the user's to change.
	isConsent := definition.GrantMode == ipcv1.GrantMode_GRANT_MODE_USER_CONSENT
	if isConsent {
		state.State = wireGrantState(m.grants.GrantStateOf(packageID, permissionID))
	}

	state.Requestable = requestable(isConsent, installed, permissionID, state)
	return state
}

// requestable decides whether requesting again still makes sense.
//
// Four reasons to say no, each matching a real situation:
//
//	not USER_CONSENT              other tiers aren't the user's decision; a prompt can't change them
//	not in the install-time set   the install-time decision refused it (not enough trust / wrong signing role / wrong origin),
//	                              or the package never requested it in its manifest. The user saying yes
//	                              can't conjure it up; SetRuntimeState has this check built in
//	already Granted               already held; requesting again only shows a pointless prompt
//	DENIED_PERMANENT              the user refused permanently. No prompt appears and the result won't change
//
// This field exists to break a loop: without it, an app seeing granted=false can only
// request, and for any of the cases above the request is bound to fail without changing granted, so it retries: a loop that
// starts spinning at boot and may show a prompt every time.
func requestable(
	isConsent bool,
	installed map[string]struct{},
	permissionID string,
	state *permissionv1.SelfPermissionState,
) bool {
	if !isConsent {
		return false
	}
	if _, ok := installed[permissionID]; !ok {
		return false
	}
	if state.GetGranted() {
		return false
	}
	return state.GetState() != permissionv1.GrantState_GRANT_STATE_DENIED_PERMANENT
}

// installedGrantSetOf gets the permission set a package actually got at install time.
//
// It uses Entry.GrantedPermissions rather than manifest.Permissions: the former is the conclusion after the install-time decision,
// the latter only the request. Going by the request, a package that wrote one extra line in its manifest would get
// requestable = true, and its request would then fail, which is exactly the loop this field exists to avoid.
//
// When the package doesn't exist it returns an empty set (not a special nil case): then every permission's requestable is
// false, consistent with "this package got nothing".
func (m *Module) installedGrantSetOf(packageID string) map[string]struct{} {
	for _, entry := range m.packages.List() {
		if entry.Manifest.PackageID != packageID {
			continue
		}
		out := make(map[string]struct{}, len(entry.GrantedPermissions))
		for _, p := range entry.GrantedPermissions {
			out[p] = struct{}{}
		}
		return out
	}
	return map[string]struct{}{}
}

// Compile-time check that *permission.Registry satisfies the extended GrantReader.
//
// It's here rather than left to main.go's wiring to expose: a wiring failure says only
// "cannot use permReg as GrantReader", while this points at which interface requires it.
var _ GrantReader = (*permission.Registry)(nil)
