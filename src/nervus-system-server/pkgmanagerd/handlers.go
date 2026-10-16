// This file adapts IPC methods to the business implementation: decode the payload, call Service, encode the result,
// and turn failures into a StatusError with a typed detail.
//
// Deliberately separate from service.go: that file is "how to deal with nervud", this one is "how to deal with Apps".
// The error semantics on the two sides are completely different: nervud gives an adminwire code string,
// while Apps want a StatusCode + PackageManagerErrorDetail.
package main

import (
	"log/slog"
	"strings"

	"google.golang.org/protobuf/proto"

	pkgv1 "github.com/nervus-os/nervus-ipc/protocol/interface/pkgmanagerv1"
	ipcv1 "github.com/nervus-os/nervus-ipc/protocol/ipcv1"
	"github.com/nervus-os/nervus-ipc/sdk"
)

// registerHandlers wires the methods to the business implementation.
//
// [It must finish before RegisterEndpoint]. The moment registration succeeds nervud may forward
// a Dispatch, and a method not registered yet gets NOT_FOUND; the caller would think the method
// doesn't exist rather than "the service isn't ready yet", an error that's very hard to trace back.
func registerHandlers(host *sdk.ServiceHost, svc *Service, log *slog.Logger) {
	host.Handle(methodInstall, func(cc sdk.CallContext, payload []byte) ([]byte, error) {
		var req pkgv1.InstallRequest
		if err := proto.Unmarshal(payload, &req); err != nil {
			return nil, invalidArgument()
		}
		// consented_permissions is forwarded as-is. This service doesn't re-check it: who may fill this field
		// is decided by nervud's needs_user_confirmation gate (only callers holding
		// perm.permission.admin, i.e. the system's confirmation UI, get through), and which entries
		// are actually stored is decided by the Catalog. A check here would only create a second source of truth.
		info, err := svc.InstallFromRelPath(
			req.GetNspkgRelpath(),
			req.GetConsentedPermissions(),
			req.GetExpectedManifestDigest())
		if err != nil {
			return nil, mapErr(err, log, "install")
		}
		return proto.Marshal(&pkgv1.InstallResult{Package: toProto(info)})
	})

	// Inspect is read-only: unpack + verify the signature + work out the USER_CONSENT permissions requested, installing nothing.
	//
	// It's where the confirmation screen gets its data. Before it, the confirmation screen had no way to know what a package to be installed
	// asked for: List and ListGrants only cover installed packages, and the package to install isn't in the Catalog yet.
	host.Handle(methodInspect, func(cc sdk.CallContext, payload []byte) ([]byte, error) {
		var req pkgv1.InspectRequest
		if err := proto.Unmarshal(payload, &req); err != nil {
			return nil, invalidArgument()
		}
		info, err := svc.InspectFromRelPath(req.GetNspkgRelpath())
		if err != nil {
			return nil, mapErr(err, log, "inspect")
		}
		return proto.Marshal(inspectToProto(info))
	})

	host.Handle(methodUninstal, func(cc sdk.CallContext, payload []byte) ([]byte, error) {
		var req pkgv1.UninstallRequest
		if err := proto.Unmarshal(payload, &req); err != nil {
			return nil, invalidArgument()
		}
		if err := svc.Uninstall(req.GetPackageId()); err != nil {
			return nil, mapErr(err, log, "uninstall")
		}
		// An empty response_type = an empty success, with no payload
		return nil, nil
	})

	host.Handle(methodList, func(cc sdk.CallContext, payload []byte) ([]byte, error) {
		// ListRequest is an empty message for now, but decode it anyway: garbage in the payload means the caller
		// used the wrong type, and reporting INVALID_ARGUMENT early beats silently ignoring it.
		var req pkgv1.ListRequest
		if err := proto.Unmarshal(payload, &req); err != nil {
			return nil, invalidArgument()
		}
		infos, err := svc.List()
		if err != nil {
			return nil, mapErr(err, log, "list")
		}
		out := &pkgv1.ListResult{Packages: make([]*pkgv1.PackageInfo, 0, len(infos))}
		for i := range infos {
			out.Packages = append(out.Packages, toProto(&infos[i]))
		}
		return proto.Marshal(out)
	})

	host.Handle(methodSetComp, func(cc sdk.CallContext, payload []byte) ([]byte, error) {
		var req pkgv1.SetComponentEnabledRequest
		if err := proto.Unmarshal(payload, &req); err != nil {
			return nil, invalidArgument()
		}
		err := svc.SetComponentEnabled(req.GetPackageId(), req.GetComponentId(), req.GetEnabled())
		if err != nil {
			return nil, mapErr(err, log, "set-component-enabled")
		}
		return nil, nil
	})
}

// toProto projects adminwire's PackageInfo onto the interface-level PackageInfo.
//
// The two types are deliberately not shared: adminwire's is the kernel's internal projection (it may carry more diagnostic
// fields one day), while the interface-level one is the public contract Apps see. Sharing them would let an internal field
// the kernel adds leak to Apps by accident.
func toProto(p *PackageInfo) *pkgv1.PackageInfo {
	if p == nil {
		return nil
	}
	return &pkgv1.PackageInfo{
		PackageId:          p.ID,
		Version:            p.Version,
		VersionCode:        p.VersionCode,
		Trust:              p.Trust,
		Source:             p.Source,
		GrantedPermissions: p.Granted,
		DisabledComponents: p.Disabled,
	}
}

// inspectToProto projects adminwire's InspectInfo onto the interface-level InspectResult.
//
// Not shared, for the same reason as toProto: that's the kernel's internal projection, this is the public contract the confirmation screen sees.
//
// Permission text is passed through as-is, [this service changes not one word]: the text comes from the Catalog's permission definitions,
// and third-party packages can define their own permissions. Any filling in or replacing here would make the description the user sees
// disagree with the permission the kernel actually decides on.
func inspectToProto(in *InspectInfo) *pkgv1.InspectResult {
	if in == nil {
		return &pkgv1.InspectResult{}
	}
	out := &pkgv1.InspectResult{
		PackageId:   in.ID,
		Version:     in.Version,
		VersionCode: in.VersionCode,
		// The confirmation screen must bring this back to INSTALL unchanged, or the gap of "you looked at A, you installed B"
		// stays open
		ManifestDigest: in.ManifestDigest,
		ConsentPermissions: make([]*pkgv1.RequestedPermission, 0,
			len(in.ConsentPermissions)),
	}
	for i := range in.ConsentPermissions {
		p := &in.ConsentPermissions[i]
		out.ConsentPermissions = append(out.ConsentPermissions, &pkgv1.RequestedPermission{
			PermissionId: p.ID,
			DisplayName: &ipcv1.LocalizedText{
				ZhCn: p.DisplayNameZhCN,
				En:   p.DisplayNameEN,
			},
			Description: &ipcv1.LocalizedText{
				ZhCn: p.DescriptionZhCN,
				En:   p.DescriptionEN,
			},
			RiskClass: riskClassFromWire(p.RiskClass),
		})
	}
	return out
}

// riskClassFromWire maps adminwire's risk class string back to the proto enum.
//
// Anything unrecognised becomes UNSPECIFIED rather than a guess: the confirmation screen decides how much emphasis to give by risk class,
// and showing a permission of unknown class as NORMAL would make the user underestimate it. UNSPECIFIED makes the UI
// take the conservative "risk unknown" branch.
func riskClassFromWire(s string) ipcv1.RiskClass {
	switch s {
	case RiskClassNormal:
		return ipcv1.RiskClass_RISK_CLASS_NORMAL
	case RiskClassPrivacySensitive:
		return ipcv1.RiskClass_RISK_CLASS_PRIVACY_SENSITIVE
	case RiskClassPhysicalControl:
		return ipcv1.RiskClass_RISK_CLASS_PHYSICAL_CONTROL
	case RiskClassCriticalSafety:
		return ipcv1.RiskClass_RISK_CLASS_CRITICAL_SAFETY
	default:
		return ipcv1.RiskClass_RISK_CLASS_UNSPECIFIED
	}
}

func invalidArgument() error {
	return &sdk.StatusError{Code: ipcv1.StatusCode_STATUS_CODE_INVALID_ARGUMENT}
}

// mapErr turns business errors into a StatusError with a typed detail.
//
// # Why this matches strings, and why that's temporary
//
// nervud's admin channel (adminwire) only returns a coarse code string (failed /
// not-found / bad-request) plus a human-readable message. Giving Apps a reason they can act on
// (invalid signature vs downgrade protection vs ABI mismatch, three cases with completely different right responses) means, for now,
// recognising it from the message.
//
// This is **acknowledged technical debt**, and it lives in nervud's adminwire: it should return structured reason codes
// instead of making downstream code infer them from human-readable text. Until then, anything unrecognised falls to
// UNSPECIFIED, failing closed: the UI says "contact the publisher" instead of guessing a possibly misleading cause.
//
// The original error only goes to the local log: handing internal error text to Apps would leak nervud's
// implementation details.
func mapErr(err error, log *slog.Logger, op string) error {
	log.Warn("pkgmanager: operation failed", "op", op, "err", err)

	reason := classify(err.Error())
	detail, merr := proto.Marshal(&pkgv1.PackageManagerErrorDetail{Reason: reason})
	if merr != nil {
		// Failing even to encode the detail is a bug on our side, and mustn't hide the original failure
		log.Error("pkgmanager: marshal error detail failed", "err", merr)
		return &sdk.StatusError{Code: ipcv1.StatusCode_STATUS_CODE_INTERNAL}
	}
	return &sdk.StatusError{
		Code:   codeFor(reason),
		Detail: detail,
	}
}

// classify works out the reason from nervud's error text. See mapErr on the technical debt.
func classify(msg string) pkgv1.PackageManagerReason {
	m := strings.ToLower(msg)
	switch {
	case strings.Contains(m, "signature"), strings.Contains(m, "signer"):
		return pkgv1.PackageManagerReason_PACKAGE_MANAGER_REASON_SIGNATURE_INVALID
	// [This must come before "digest"]. Its error message also contains "digest", and falling into the case below
	// would call it "the package is broken, download it again", when the package is perfectly valid, just not the one the user
	// confirmed; the right fix is to go through confirmation again.
	case strings.Contains(m, "does not match the confirmed package"),
		strings.Contains(m, "content changed after user confirmation"):
		return pkgv1.PackageManagerReason_PACKAGE_MANAGER_REASON_CONTENT_CHANGED
	case strings.Contains(m, "digest"):
		return pkgv1.PackageManagerReason_PACKAGE_MANAGER_REASON_DIGEST_MISMATCH
	case strings.Contains(m, "downgrade"), strings.Contains(m, "version_code"):
		return pkgv1.PackageManagerReason_PACKAGE_MANAGER_REASON_DOWNGRADE
	case strings.Contains(m, "abi"):
		return pkgv1.PackageManagerReason_PACKAGE_MANAGER_REASON_ABI_MISMATCH
	case strings.Contains(m, "manifest"):
		return pkgv1.PackageManagerReason_PACKAGE_MANAGER_REASON_MANIFEST_INVALID
	case strings.Contains(m, "lineage"):
		return pkgv1.PackageManagerReason_PACKAGE_MANAGER_REASON_LINEAGE_BROKEN
	case strings.Contains(m, "immutable"), strings.Contains(m, "protected"):
		return pkgv1.PackageManagerReason_PACKAGE_MANAGER_REASON_IMMUTABLE
	case strings.Contains(m, "unpack failed"), strings.Contains(m, "archive"), strings.Contains(m, "escapes"):
		return pkgv1.PackageManagerReason_PACKAGE_MANAGER_REASON_ARCHIVE_INVALID
	case strings.Contains(m, "not-found"), strings.Contains(m, "not found"):
		return pkgv1.PackageManagerReason_PACKAGE_MANAGER_REASON_PACKAGE_NOT_FOUND
	default:
		// Fail closed: if it can't be recognised, say so; don't guess
		return pkgv1.PackageManagerReason_PACKAGE_MANAGER_REASON_UNSPECIFIED
	}
}

// codeFor gives the outer StatusCode that matches the reason.
//
// The outer code decides the SDK's recovery behaviour (IsRetryable / NeedsReResolve), the typed detail
// decides how the UI explains it. They must agree: a FAILED_PRECONDITION paired with
// PACKAGE_NOT_FOUND would make the SDK not re-resolve while the UI says the package can't be found, contradicting each other.
func codeFor(r pkgv1.PackageManagerReason) ipcv1.StatusCode {
	switch r {
	case pkgv1.PackageManagerReason_PACKAGE_MANAGER_REASON_PACKAGE_NOT_FOUND:
		return ipcv1.StatusCode_STATUS_CODE_NOT_FOUND
	case pkgv1.PackageManagerReason_PACKAGE_MANAGER_REASON_MANIFEST_INVALID,
		pkgv1.PackageManagerReason_PACKAGE_MANAGER_REASON_ARCHIVE_INVALID:
		// The package itself is the problem = the caller supplied the wrong thing
		return ipcv1.StatusCode_STATUS_CODE_INVALID_ARGUMENT
	case pkgv1.PackageManagerReason_PACKAGE_MANAGER_REASON_UNSPECIFIED:
		return ipcv1.StatusCode_STATUS_CODE_INTERNAL
	default:
		// Signature/digest/downgrade/ABI/lineage/immutable: a precondition isn't met, so retrying doesn't help
		return ipcv1.StatusCode_STATUS_CODE_FAILED_PRECONDITION
	}
}
