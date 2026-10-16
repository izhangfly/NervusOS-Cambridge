// This file is the wire wiring for "launch a component": it connects the Envelope's LaunchComponent(80) to
// internal/service's EnsureStarted.
//
// # Why it needs a message of its own
//
// Before this, the only path in the kernel that could start a component was endpoint.Resolve starting an on-demand
// provider. So "launch an App" had to be written as "send a ResolveEndpoint for some interface it exports",
// which cost three things:
//
//  1. every launchable App had to export a placeholder interface it didn't need at all;
//  2. "resolve an interface" and "launch an app" shared one message, so audit couldn't tell them apart; when tracking down
//     "who started this thing" all you saw was ResolveEndpoint;
//  3. a pure UI app with no interface at all had no way to be launched.
//
// # It sets up no call relationship
//
// Success only means "that component is running now". A caller wanting to talk to it still has to go through ResolveEndpoint,
// with every permission and visibility decision intact; nothing is relaxed because "I'm the one who started it".
package ipc

import (
	"context"
	"errors"

	"google.golang.org/protobuf/proto"

	ipcv1 "github.com/nervus-os/nervus-ipc/protocol/ipcv1"

	"github.com/nervus-os/nervud/internal/service"
)

// permSystemLaunch is the permission needed to send LaunchComponent.
//
// It must have the same name as the entry in the central catalog bootstrap: that's the definition, this is the enforcement point.
// It isn't a capability ordinary apps should have: launching components at will means bypassing on-demand's power-saving semantics,
// and being able to start a component immediately after it was disabled and re-enabled. v1 gives it only to the Launcher and the session service.
const permSystemLaunch = "perm.system.launch"

// permPermissionAdmin is the identity credential of the system confirmation UI.
//
// It's SYSTEM_ONLY + PLATFORM trust + the platform-release signing role (see the catalog
// bootstrap), so its holder can only be the one confirmation UI shipped with the read-only system image.
// This package only uses it to answer "was this call made by the confirmation UI"; see conn.callerIsConfirmationUI
const permPermissionAdmin = "perm.permission.admin"

// callerIsConfirmationUI answers whether this connection's caller is the system confirmation UI.
//
// When permission isn't wired (minimal wiring / tests) it returns false. This question must fail closed:
// getting it wrong would let any caller trigger operations that need user confirmation directly
func (co *conn) callerIsConfirmationUI() bool {
	if co == nil || co.s == nil || co.s.permission == nil {
		return false
	}
	return co.s.permission.Allowed(co.caller.PackageID, permPermissionAdmin)
}

// handleLaunchComponent handles one launch request.
func (co *conn) handleLaunchComponent(req *ipcv1.LaunchComponent) bool {
	reqID := req.GetRequestId()
	if reqID == 0 {
		co.log.Warn("ipc: LaunchComponent with reserved request_id 0, closing")
		co.s.auditViolation(co.caller, errZeroRequestID)
		return false
	}

	if co.s.launcher == nil {
		// service isn't wired (tests / trimmed builds). Return UNAVAILABLE rather than closing the connection:
		// a capability gap isn't a protocol violation. The same shape as the leases == nil degradation
		return co.enqueue(launchFailure(reqID,
			ipcv1.StatusCode_STATUS_CODE_UNAVAILABLE,
			ipcv1.LaunchComponentReason_LAUNCH_COMPONENT_REASON_SPAWN_FAILED))
	}

	// Permission: block before doing any lookup. Placed later, a caller without permission could still
	// probe which packages are installed on the machine from the difference between "NOT_FOUND or PERMISSION_DENIED"
	if co.s.permission == nil || !co.s.permission.Allowed(co.caller.PackageID, permSystemLaunch) {
		co.s.auditViolation(co.caller, errLaunchDenied)
		return co.enqueue(launchFailure(reqID,
			ipcv1.StatusCode_STATUS_CODE_PERMISSION_DENIED,
			ipcv1.LaunchComponentReason_LAUNCH_COMPONENT_REASON_UNSPECIFIED))
	}

	pkg := req.GetPackageId()
	comp := req.GetComponentId()
	if pkg == "" || comp == "" {
		// Both are required. No inference like "empty component = the package's only app component":
		// a package may well have several app components, and an inference rule would quietly change behaviour
		// the day it goes from one to two
		return co.enqueue(launchFailure(reqID,
			ipcv1.StatusCode_STATUS_CODE_INVALID_ARGUMENT,
			ipcv1.LaunchComponentReason_LAUNCH_COMPONENT_REASON_UNSPECIFIED))
	}

	// Ask whether it's running first: EnsureStarted is idempotent itself, but the caller needs to know whether it was the one
	// that really started it (the session service uses this to decide whether to log "the desktop restarted")
	alreadyRunning := co.s.launcher.IsRunning(pkg, comp)

	if err := co.s.launcher.EnsureStarted(context.Background(), pkg, comp); err != nil {
		code, reason := launchErrorToWire(err)
		co.s.auditViolation(co.caller, err)
		return co.enqueue(launchFailure(reqID, code, reason))
	}

	co.s.auditLaunch(co.caller, pkg, comp, alreadyRunning)
	return co.enqueue(&ipcv1.Envelope{
		Body: &ipcv1.Envelope_LaunchComponentResult{
			LaunchComponentResult: &ipcv1.LaunchComponentResult{
				RequestId: reqID,
				Outcome: &ipcv1.LaunchComponentResult_Success{
					Success: &ipcv1.LaunchComponentSuccess{AlreadyRunning: alreadyRunning},
				},
			},
		},
	})
}

var errLaunchDenied = errors.New("ipc: launch denied: missing " + permSystemLaunch)

// launchErrorToWire maps service-layer errors to (StatusCode, reason).
//
// It uses errors.Is on sentinel values rather than matching error strings: string matching would silently break the day someone rewords
// an error message, and breaking would show up as every failure collapsing into SPAWN_FAILED,
// so actionable reasons like "the component is disabled" would disappear.
func launchErrorToWire(err error) (ipcv1.StatusCode, ipcv1.LaunchComponentReason) {
	switch {
	case errors.Is(err, service.ErrUnknownPackage):
		return ipcv1.StatusCode_STATUS_CODE_NOT_FOUND,
			ipcv1.LaunchComponentReason_LAUNCH_COMPONENT_REASON_PACKAGE_NOT_FOUND
	case errors.Is(err, service.ErrUnknownComponent):
		return ipcv1.StatusCode_STATUS_CODE_NOT_FOUND,
			ipcv1.LaunchComponentReason_LAUNCH_COMPONENT_REASON_COMPONENT_NOT_FOUND
	case errors.Is(err, service.ErrComponentDisabled):
		// FAILED_PRECONDITION rather than NOT_FOUND: the component exists, it was just switched off deliberately.
		// The Launcher uses this to prompt the user to enable it in Settings, rather than saying "can't find this app"
		return ipcv1.StatusCode_STATUS_CODE_FAILED_PRECONDITION,
			ipcv1.LaunchComponentReason_LAUNCH_COMPONENT_REASON_COMPONENT_DISABLED
	case errors.Is(err, service.ErrComponentFailed):
		return ipcv1.StatusCode_STATUS_CODE_FAILED_PRECONDITION,
			ipcv1.LaunchComponentReason_LAUNCH_COMPONENT_REASON_COMPONENT_FAILED
	default:
		return ipcv1.StatusCode_STATUS_CODE_INTERNAL,
			ipcv1.LaunchComponentReason_LAUNCH_COMPONENT_REASON_SPAWN_FAILED
	}
}

// launchFailure builds a failure result carrying a typed error_detail.
func launchFailure(reqID uint64, code ipcv1.StatusCode, reason ipcv1.LaunchComponentReason) *ipcv1.Envelope {
	failure := &ipcv1.Failure{Code: code}
	// Failing to encode the error_detail shouldn't stop the whole response being sent: the outer code already carries
	// the minimum information the caller needs to decide, and the detail only lets it display things more precisely
	if detail, err := proto.Marshal(&ipcv1.LaunchComponentErrorDetail{Reason: reason}); err == nil {
		failure.ErrorDetail = detail
	}
	return &ipcv1.Envelope{
		Body: &ipcv1.Envelope_LaunchComponentResult{
			LaunchComponentResult: &ipcv1.LaunchComponentResult{
				RequestId: reqID,
				Outcome:   &ipcv1.LaunchComponentResult_Failure{Failure: failure},
			},
		},
	}
}
