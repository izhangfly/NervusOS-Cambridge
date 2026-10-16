// This package wraps "which packages requested which USER_CONSENT permissions, and does the user agree right now" as a built-in
// endpoint, which nervud registers during wiring (endpoint.RegisterBuiltin).
//
// # The real gap it fills
//
// The runtime state of USER_CONSENT permissions (perm.storage.user, perm.camera.capture and the like)
// defaults to NotRequested, and AllowedAt only lets this tier through when the state is exactly Granted. In other
// words, an ordinary app that requests such a permission can never get it after installing, unless something changes that
// runtime state.
//
// # Before this interface, the only thing in the whole system that could change it was SetRuntimeState, which is only reachable over the admin channel
//
// (nervud-admin.sock, which only lets in packages holding perm.pkg.admin and the ops UID), so in practice only
//
// nervusctl could use it. The result: the app installed, passed the install-time decision, got PERMISSION_DENIED on its first
// runtime call, and nothing in the system had a UI to fix it; the app couldn't even ask for it.
//
// # Why the kernel implements it rather than a system service
//
// The grant state is permission.Registry's own state. Delegating it to a service would hand
// "who can change the system's dangerous permissions" to a package that can be replaced, and the bar for perm.permission.admin
// (SYSTEM_ONLY + PLATFORM trust + the platform-release signing role) exists precisely so it can't be
// replaced. Same shape as power / safety.control / resource.directory.
//
// # The boundary: runtime grant state only, not install-time decisions
//
// The install-time set (Entry.GrantedPermissions) is computed by pkgregistry's IntersectAt and
// answers "is this package entitled to this permission at all"; this interface changes "does the user agree right now".
// Both must pass to allow. So calling SetGrantState on a permission not granted at install time fails:
// the UI shouldn't give the user a switch that does nothing when pressed.
package permissionadmin

import (
	"log/slog"
	"sort"

	permissionv1 "github.com/nervus-os/nervus-ipc/protocol/interface/permissionv1"
	ipcv1 "github.com/nervus-os/nervus-ipc/protocol/ipcv1"
	"google.golang.org/protobuf/proto"

	"github.com/nervus-os/nervud/internal/catalog"
	"github.com/nervus-os/nervud/internal/endpoint"
	"github.com/nervus-os/nervud/internal/permission"
	"github.com/nervus-os/nervud/internal/pkgregistry"
)

// BuiltinInterfaceID is this built-in interface's ID.
//
// It must match the bootstrapInterface call in the catalog bootstrap, which sets the bar.
const BuiltinInterfaceID = catalog.InterfacePermissionAdmin

// Method numbers come from the generated enum values, not literals copied here: the cost of a copy isn't duplication, it's that
// it quietly goes stale. If the proto renumbers, this keeps the old value, and calls get routed to a method that doesn't exist.
// Same approach as resourcedir / safety.
const (
	MethodListGrants = uint32(
		permissionv1.PermissionAdminMethod_PERMISSION_ADMIN_METHOD_LIST_GRANTS)
	MethodSetGrantState = uint32(
		permissionv1.PermissionAdminMethod_PERMISSION_ADMIN_METHOD_SET_GRANT_STATE)
)

// PackageLister is this package's narrow dependency on pkgregistry.Registry: list installed packages.
// *pkgregistry.Registry satisfies it implicitly.
type PackageLister interface {
	List() []pkgregistry.Entry
}

// GrantReader is this package's narrow dependency on permission.Registry.
//
// Reads and writes are listed separately on purpose: the two things this package does (read a list, change one state) differ in risk by an
// order of magnitude, and separating them in the interface makes "does this method change state?" obvious at a glance.
type GrantReader interface {
	GrantStateOf(packageID, permissionID string) permission.GrantState
	SetRuntimeState(packageID, permissionID string, state permission.GrantState) error
	// AllowedAt is the authoritative answer to "can it be used right now", used by the self-check interface (permission.self).
	//
	// This method must be reused, not recomputed here: it's the same code as the kernel's authorisation decision,
	// and already combines whether it's in the install-time set, which grant mode applies, the USER_CONSENT runtime state,
	// and the consent exemption for system software. Recomputing would create a second source of truth,
	// and once the two disagree, the app sees "the self-check says yes, the real call is refused".
	//
	// It takes a Snapshot so one answer uses only one Catalog revision, for the same
	// reason endpoint.Route uses one.
	AllowedAt(definitions *catalog.Snapshot, packageID, permissionID string) bool
}

// Module holds the minimal dependencies needed for grant queries and changes.
type Module struct {
	definitions *catalog.Registry
	packages    PackageLister
	grants      GrantReader
	log         *slog.Logger
}

// New constructs the Module. If any dependency is nil, the handler fails closed with UNAVAILABLE:
// a grant UI that can't read definitions would show "can't look it up" as "no permissions need granting", which is worse than an error.
func New(
	definitions *catalog.Registry, packages PackageLister, grants GrantReader, log *slog.Logger,
) *Module {
	return &Module{definitions: definitions, packages: packages, grants: grants, log: log}
}

// BuiltinHandler returns a handler that can be passed straight to endpoint.RegisterBuiltin.
func (m *Module) BuiltinHandler() endpoint.BuiltinHandler {
	return func(call endpoint.BuiltinCall) endpoint.BuiltinResult {
		switch call.MethodID {
		case MethodListGrants:
			return m.listGrants(call.Payload)
		case MethodSetGrantState:
			return m.setGrantState(call.Payload)
		default:
			// Fail closed: an unimplemented method doesn't exist. Returning an empty list would make the UI
			// think no permissions on this machine need granting.
			return endpoint.BuiltinResult{Code: ipcv1.StatusCode_STATUS_CODE_NOT_FOUND}
		}
	}
}

func (m *Module) ready() bool {
	return m != nil && m.definitions != nil && m.packages != nil && m.grants != nil
}

// listGrants lists each package's USER_CONSENT permissions and their current state.
//
// It reads the Catalog Snapshot once, so the whole response comes from one revision: reading twice could mix
// two revisions into the response, with unreproducible results like "listed the permissions of a package that was just uninstalled".
func (m *Module) listGrants(payload []byte) endpoint.BuiltinResult {
	if !m.ready() {
		return endpoint.BuiltinResult{Code: ipcv1.StatusCode_STATUS_CODE_UNAVAILABLE}
	}

	req := &permissionv1.ListGrantsRequest{}
	if err := proto.Unmarshal(payload, req); err != nil {
		// An undecodable request is the caller's fault. Return INVALID_ARGUMENT rather than treating it as an empty request
		// and listing everything, which would turn an encoding error into a full listing.
		return endpoint.BuiltinResult{Code: ipcv1.StatusCode_STATUS_CODE_INVALID_ARGUMENT}
	}

	snap := m.definitions.Current()
	out := &permissionv1.GrantList{}
	for _, entry := range m.packages.List() {
		packageID := entry.Manifest.PackageID
		if filter := req.GetPackageId(); filter != "" && filter != packageID {
			continue
		}
		permissions := m.consentGrantsOf(snap, entry)
		if len(permissions) == 0 {
			// Packages with no grantable permission aren't listed: an entry in Settings that opens onto
			// nothing is pure noise to the user.
			continue
		}
		out.Packages = append(out.Packages, &permissionv1.PackageGrants{
			PackageId:   packageID,
			Label:       entry.Manifest.Label,
			Permissions: permissions,
		})
	}
	// Deterministic order, otherwise the entries in Settings jump on every refresh.
	sort.Slice(out.Packages, func(i, j int) bool {
		return out.Packages[i].GetPackageId() < out.Packages[j].GetPackageId()
	})

	wire, err := proto.Marshal(out)
	if err != nil {
		if m.log != nil {
			m.log.Warn("permissionadmin: marshal grant list failed", "err", err)
		}
		return endpoint.BuiltinResult{Code: ipcv1.StatusCode_STATUS_CODE_INTERNAL}
	}
	return endpoint.BuiltinResult{Payload: wire, Code: ipcv1.StatusCode_STATUS_CODE_OK}
}

// consentGrantsOf projects one package's USER_CONSENT permissions.
//
// It looks only at GrantedPermissions, not what the manifest requested: the former is the set the package
// actually got after the install-time decision. Listing a permission the decision refused gives the user a switch that does nothing.
func (m *Module) consentGrantsOf(
	snap *catalog.Snapshot, entry pkgregistry.Entry,
) []*permissionv1.PermissionGrant {
	var out []*permissionv1.PermissionGrant
	for _, permissionID := range entry.GrantedPermissions {
		definition, ok := snap.Permission(permissionID)
		if !ok || definition.GrantMode != ipcv1.GrantMode_GRANT_MODE_USER_CONSENT {
			// Other grant modes have no runtime state, and listing them would only give the user a row of switches that won't move.
			continue
		}
		out = append(out, &permissionv1.PermissionGrant{
			PermissionId: permissionID,
			// The text comes from the kernel rather than being hard-coded in the UI: third-party packages can define their own permissions,
			// and the UI can't know their text in advance.
			DisplayName: &ipcv1.LocalizedText{
				ZhCn: definition.DisplayNameZhCN, En: definition.DisplayNameEN,
			},
			Description: &ipcv1.LocalizedText{
				ZhCn: definition.DescriptionZhCN, En: definition.DescriptionEN,
			},
			RiskClass: definition.RiskClass,
			State:     wireGrantState(m.grants.GrantStateOf(entry.Manifest.PackageID, permissionID)),
			// EffectiveGranted comes from AllowedAt rather than being derived from State.
			//
			// The two disagree for system software: the consent exemption skips the step of "should we ask the user"
			// itself, and doesn't fake a grant record, so system software's State is always
			// NOT_REQUESTED while AllowedAt is always true.
			//
			// Reporting only State makes the UI contradict reality: the file manager's "user files" switch shows
			// off while it can actually read and write the user directory. This is the same reason permission.self's granted field
			// uses AllowedAt; that one avoided the bug, this one hit it.
			EffectiveGranted: m.grants.AllowedAt(snap, entry.Manifest.PackageID, permissionID),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].GetPermissionId() < out[j].GetPermissionId()
	})
	return out
}

// setGrantState changes the runtime state of one USER_CONSENT permission.
func (m *Module) setGrantState(payload []byte) endpoint.BuiltinResult {
	if !m.ready() {
		return endpoint.BuiltinResult{Code: ipcv1.StatusCode_STATUS_CODE_UNAVAILABLE}
	}

	req := &permissionv1.SetGrantStateRequest{}
	if err := proto.Unmarshal(payload, req); err != nil {
		return endpoint.BuiltinResult{Code: ipcv1.StatusCode_STATUS_CODE_INVALID_ARGUMENT}
	}
	if req.GetPackageId() == "" || req.GetPermissionId() == "" {
		return endpoint.BuiltinResult{Code: ipcv1.StatusCode_STATUS_CODE_INVALID_ARGUMENT}
	}

	state, ok := kernelGrantState(req.GetState())
	if !ok {
		// UNSPECIFIED means not filled in; NOT_REQUESTED means "back to never asked", which isn't a decision the user
		// can make. To take it back, use DENIED.
		return endpoint.BuiltinResult{Code: ipcv1.StatusCode_STATUS_CODE_INVALID_ARGUMENT}
	}

	if err := m.grants.SetRuntimeState(req.GetPackageId(), req.GetPermissionId(), state); err != nil {
		if m.log != nil {
			m.log.Warn("permissionadmin: set grant state failed",
				"package", req.GetPackageId(), "permission", req.GetPermissionId(), "err", err)
		}
		// Every failure reason amounts to "this permission shouldn't be changed now": not USER_CONSENT, the package was uninstalled,
		// or it wasn't granted at install time. All three mean the caller is working from a stale view, and
		// FAILED_PRECONDITION says better than INTERNAL what to do about it (ListGrants again).
		return endpoint.BuiltinResult{Code: ipcv1.StatusCode_STATUS_CODE_FAILED_PRECONDITION}
	}

	// Return the explicit current state rather than an empty response: what the UI gets back should be the current fact,
	// not the value it just asked for.
	resp := &permissionv1.SetGrantStateResponse{
		State: wireGrantState(m.grants.GrantStateOf(req.GetPackageId(), req.GetPermissionId())),
	}
	wire, err := proto.Marshal(resp)
	if err != nil {
		if m.log != nil {
			m.log.Warn("permissionadmin: marshal set-grant response failed", "err", err)
		}
		return endpoint.BuiltinResult{Code: ipcv1.StatusCode_STATUS_CODE_INTERNAL}
	}
	return endpoint.BuiltinResult{Payload: wire, Code: ipcv1.StatusCode_STATUS_CODE_OK}
}

// wireGrantState projects the kernel state onto the wire enum.
//
// Value-by-value explicit mapping rather than numeric conversion: the two enums have different zero values. The kernel's 0 is NotRequested,
// the wire's 0 is UNSPECIFIED (proto3 can't tell "not filled in" from "filled in with zero"). A direct conversion would send
// "never asked the user" as "not filled in".
func wireGrantState(state permission.GrantState) permissionv1.GrantState {
	switch state {
	case permission.GrantStateGranted:
		return permissionv1.GrantState_GRANT_STATE_GRANTED
	case permission.GrantStateDenied:
		return permissionv1.GrantState_GRANT_STATE_DENIED
	case permission.GrantStateDeniedPermanent:
		return permissionv1.GrantState_GRANT_STATE_DENIED_PERMANENT
	case permission.GrantStateNotRequested:
		return permissionv1.GrantState_GRANT_STATE_NOT_REQUESTED
	default:
		return permissionv1.GrantState_GRANT_STATE_UNSPECIFIED
	}
}

// kernelGrantState maps the wire enum back to kernel state. The second return value is false when the value
// isn't a target state the caller may set.
func kernelGrantState(state permissionv1.GrantState) (permission.GrantState, bool) {
	switch state {
	case permissionv1.GrantState_GRANT_STATE_GRANTED:
		return permission.GrantStateGranted, true
	case permissionv1.GrantState_GRANT_STATE_DENIED:
		return permission.GrantStateDenied, true
	case permissionv1.GrantState_GRANT_STATE_DENIED_PERMANENT:
		return permission.GrantStateDeniedPermanent, true
	default:
		return permission.GrantStateNotRequested, false
	}
}
