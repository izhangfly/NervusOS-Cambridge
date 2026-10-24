// handlers.go maps each admin command to the matching pkgregistry / permission operation and projects the result back into adminwire.Response.
//
// Security discipline: this file makes no security decision. Signature checks, Arbitrate, OEM countersignatures, ABI,
// permission Intersect and failure compensation all live inside pkgregistry.Install/Uninstall. This file only does
//  1. argument presence and path-escape checks (the staging directory must be a direct child of the staging root
//     nervud controls);
//  2. forwarding;
//  3. result/error projection.
package admin

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"

	ipcv1 "github.com/nervus-os/nervus-ipc/protocol/ipcv1"

	"github.com/nervus-os/nervud/internal/adminwire"
	"github.com/nervus-os/nervud/internal/permission"
	"github.com/nervus-os/nervud/internal/pkgregistry"
)

// dispatch routes by Request.Cmd. Unknown commands get CodeBadRequest (fail-closed, never silent).
func (s *Server) dispatch(ctx context.Context, req adminwire.Request) adminwire.Response {
	switch req.Cmd {
	case adminwire.CmdBeginStaging:
		return s.handleBeginStaging()
	case adminwire.CmdInspect:
		return s.handleInspect(ctx, req)
	case adminwire.CmdInstall:
		return s.handleInstall(ctx, req)
	case adminwire.CmdUninstall:
		return s.handleUninstall(ctx, req)
	case adminwire.CmdList:
		return s.handleList()
	case adminwire.CmdSetEnabled:
		return s.handleSetEnabled(ctx, req)
	case adminwire.CmdSetPermission:
		return s.handleSetPermission(req)
	case adminwire.CmdLaunch:
		return s.handleLaunch(ctx, req)
	default:
		return badRequest("unknown command %q", req.Cmd)
	}
}

// handleBeginStaging creates an empty directory under the staging root and returns its absolute path. nervud
// creating it (rather than the CLI choosing a path) guarantees: the same filesystem (no EXDEV on the install-time renameat2), a controlled
// owner/mode (0700, owned by nervud), and a clear criterion for install's path-escape check.
//
// It also sweeps stale staging: if the CLI crashes after begin and before install, it leaves an orphan directory.
// Each begin deletes ones that are too old (over staleStagingAge) on a best-effort basis. This doesn't affect correctness;
// it only reclaims disk.
func (s *Server) handleBeginStaging() adminwire.Response {
	s.sweepStaleStaging()

	dir, err := os.MkdirTemp(s.stagingRoot, "stage-")
	if err != nil {
		s.audit("admin.BeginStaging", "", true, err, "mkdir temp staging")
		return failed("create staging dir: %v", err)
	}

	// Hand it over to the install service: the directory is created 0700 and owned by nervud (root), while the unpacking is done by that service
	// under its own UID. Without handing over ownership it couldn't write into its own staging,
	// and the install would fail at the "unpack" step with permission denied.
	//
	// Create first, then chown, not the other way round: for that instant the directory belongs to root with 0700, and nobody can get in.
	// There's no "other way round" anyway: creating it offers no way to specify an owner.
	//
	// admittedUID of 0 means no package holds perm.pkg.admin (minimal image, development machine, or fail closed with several
	// holders). Then only root uses this channel (nervusctl), and nothing needs handing over.
	if s.admittedUID != 0 {
		// In this system a Package's GID always equals its UID, the same convention as the socket chown.
		if err := os.Chown(dir, int(s.admittedUID), int(s.admittedUID)); err != nil {
			_ = os.RemoveAll(dir) // staging that can't be handed over is garbage; don't leave it around
			s.audit("admin.BeginStaging", dir, true, err, "chown staging to service")
			return failed("chown staging dir: %v", err)
		}
	}

	s.audit("admin.BeginStaging", dir, false, nil, "")
	return adminwire.Response{OK: true, Code: adminwire.CodeOK, StagingDir: dir}
}

// handleInspect checks the staging path, then triggers pkgregistry.Inspect: parse and verify, and return
// the USER_CONSENT permissions the package requests. Read-only; changes no state.
//
// # Staging is NOT cleaned up on failure
//
// The opposite of handleInstall. The reason is that the caller on this path is the confirmation screen, whose normal flow is
// inspect -> show the user -> install, both pointing at the same staging directory:
//
//	clean up on success  → the install right after can't find the directory and is bound to fail
//	clean up on failure  → the confirmation screen can't retry inspect on the same tree (a transient parse failure, or devmode
//	                       just switched on), and going through begin-staging + unpacking again is not cheap
//
// Orphan staging is reclaimed by handleBeginStaging's sweepStaleStaging; there's no need for a backstop here.
func (s *Server) handleInspect(ctx context.Context, req adminwire.Request) adminwire.Response {
	staging, err := s.validateStagingChild(req.StagingDir)
	if err != nil {
		s.audit("admin.Inspect", req.StagingDir, true, err, "staging path rejected")
		return badRequest("%v", err)
	}

	result, err := s.pkgs.Inspect(ctx, staging)
	if err != nil {
		s.audit("admin.Inspect", staging, true, err, "inspect")
		return failed("inspect: %v", err)
	}

	info := adminwire.InspectInfo{
		ID:          result.PackageID,
		Version:     result.Version,
		VersionCode: result.VersionCode,
		// Must be passed out: the confirmation screen uses it to bind the install back to the content it saw
		ManifestDigest: result.ManifestDigest,
	}
	for _, p := range result.ConsentPermissions {
		info.ConsentPermissions = append(info.ConsentPermissions, adminwire.ConsentPermissionInfo{
			ID:              p.ID,
			DisplayNameZhCN: p.DisplayNameZhCN,
			DisplayNameEN:   p.DisplayNameEN,
			DescriptionZhCN: p.DescriptionZhCN,
			DescriptionEN:   p.DescriptionEN,
			RiskClass:       riskClassToWire(p.RiskClass),
		})
	}

	s.audit("admin.Inspect", result.PackageID, false, nil,
		fmt.Sprintf("version=%s consent_permissions=%d",
			result.Version, len(result.ConsentPermissions)))
	return adminwire.Response{OK: true, Code: adminwire.CodeOK, Inspect: &info}
}

// handleInstall checks the staging path, then triggers pkgregistry.Install. nervud itself reads manifest.json / manifest.sig from the staging
// directory as the bytes to verify, and Install then does the full
// signature check + digest re-check + decision + atomic commit.
func (s *Server) handleInstall(ctx context.Context, req adminwire.Request) adminwire.Response {
	staging, err := s.validateStagingChild(req.StagingDir)
	if err != nil {
		s.audit("admin.Install", req.StagingDir, true, err, "staging path rejected")
		return badRequest("%v", err)
	}

	manifestBytes, err := os.ReadFile(filepath.Join(staging, pkgregistry.ManifestFileName))
	if err != nil {
		s.cleanupStaging(staging)
		s.audit("admin.Install", staging, true, err, "read manifest")
		return badRequest("read %s: %v", pkgregistry.ManifestFileName, err)
	}
	sigBlock, err := os.ReadFile(filepath.Join(staging, pkgregistry.SignatureFileName))
	if err != nil {
		s.cleanupStaging(staging)
		s.audit("admin.Install", staging, true, err, "read signature")
		return badRequest("read %s: %v", pkgregistry.SignatureFileName, err)
	}

	entry, err := s.pkgs.Install(ctx, pkgregistry.InstallTransaction{
		ManifestBytes: manifestBytes,
		SigBlock:      sigBlock,
		StagingDir:    staging,
		Source:        pkgregistry.SourceDynamicInstall,
		// The consent gathered by the confirmation screen is passed through unchanged. This layer trims nothing; trimming belongs to Install,
		// which is the one holding the install-time grant set and the permission definitions
		ConsentedPermissions:   req.ConsentedPermissions,
		ExpectedManifestDigest: req.ExpectedManifestDigest,
	})
	if err != nil {
		// When Install fails, the staging directory wasn't consumed by renameat2 (it's only moved on success), so compensate by deleting
		// the tree the CLI unpacked, to avoid piling up orphans in the staging root. Failure compensation for the install itself (removing
		// code/data orphans already written to disk) is pkgregistry's job; this only cleans staging.
		s.cleanupStaging(staging)
		s.audit("admin.Install", staging, true, err, "install")
		return failed("install: %v", err)
	}

	s.audit("admin.Install", entry.Manifest.PackageID, false, nil,
		fmt.Sprintf("version=%s trust=%s", entry.ActiveVersion, entry.Trust))
	info := entryToInfo(entry)
	return adminwire.Response{OK: true, Code: adminwire.CodeOK, Package: &info}
}

// handleUninstall uninstalls a Package. Rules such as "system image packages can't be uninstalled dynamically" are decided by pkgregistry.
func (s *Server) handleUninstall(ctx context.Context, req adminwire.Request) adminwire.Response {
	if req.PackageID == "" {
		return badRequest("uninstall requires package_id")
	}
	if err := s.pkgs.Uninstall(ctx, req.PackageID); err != nil {
		s.audit("admin.Uninstall", req.PackageID, true, err, "")
		return classifyPkgErr(err)
	}
	s.audit("admin.Uninstall", req.PackageID, false, nil, "")
	return adminwire.Response{OK: true, Code: adminwire.CodeOK}
}

// handleList lists the currently installed Packages.
func (s *Server) handleList() adminwire.Response {
	entries := s.reg.List()
	infos := make([]adminwire.PackageInfo, 0, len(entries))
	for _, e := range entries {
		infos = append(infos, entryToInfo(e))
	}
	return adminwire.Response{OK: true, Code: adminwire.CodeOK, Packages: infos}
}

// handleSetEnabled disables/enables a Component. The protected list and whether it can be disabled are decided by pkgregistry.
func (s *Server) handleSetEnabled(ctx context.Context, req adminwire.Request) adminwire.Response {
	if req.PackageID == "" || req.ComponentID == "" {
		return badRequest("set-enabled requires package_id and component_id")
	}
	if err := s.pkgs.SetComponentEnabled(ctx, req.PackageID, req.ComponentID, req.Enabled); err != nil {
		s.audit("admin.SetComponentEnabled", req.PackageID, true, err,
			fmt.Sprintf("%s enabled=%v", req.ComponentID, req.Enabled))
		return classifyPkgErr(err)
	}
	s.audit("admin.SetComponentEnabled", req.PackageID, false, nil,
		fmt.Sprintf("%s enabled=%v", req.ComponentID, req.Enabled))
	return adminwire.Response{OK: true, Code: adminwire.CodeOK}
}

// handleLaunch starts a component through the same service manager the launcher's LaunchComponent uses.
func (s *Server) handleLaunch(ctx context.Context, req adminwire.Request) adminwire.Response {
	if req.PackageID == "" || req.ComponentID == "" {
		return badRequest("launch requires package_id and component_id")
	}
	if s.launcher == nil {
		return adminwire.Response{OK: false, Code: adminwire.CodeFailed, Message: "no service manager"}
	}
	if err := s.launcher.EnsureStarted(ctx, req.PackageID, req.ComponentID); err != nil {
		s.audit("admin.Launch", req.PackageID, true, err, req.ComponentID)
		return classifyPkgErr(err)
	}
	s.audit("admin.Launch", req.PackageID, false, nil, req.ComponentID)
	return adminwire.Response{OK: true, Code: adminwire.CodeOK}
}

// handleSetPermission sets the grant state of a runtime (GrantUser) permission (grant/revoke).
// Whether a permission can be granted at run time, and lease revocation when a motion-group permission is revoked, are all decided by permission.Registry.
func (s *Server) handleSetPermission(req adminwire.Request) adminwire.Response {
	if req.PackageID == "" || req.Permission == "" {
		return badRequest("set-permission requires package_id and permission")
	}
	state, ok := grantStateFromWire(req.GrantState)
	if !ok {
		return badRequest("unknown grant state %q", req.GrantState)
	}
	if err := s.perms.SetRuntimeState(req.PackageID, req.Permission, state); err != nil {
		s.audit("admin.SetPermission", req.PackageID, true, err,
			fmt.Sprintf("%s=%s", req.Permission, req.GrantState))
		return failed("set permission: %v", err)
	}
	s.audit("admin.SetPermission", req.PackageID, false, nil,
		fmt.Sprintf("%s=%s", req.Permission, req.GrantState))
	return adminwire.Response{OK: true, Code: adminwire.CodeOK}
}

// ---- Checks and projection helpers -------------------------------------------------------

// validateStagingChild checks that dir is a direct child of the staging root and currently exists as a directory.
// This is this module's only path-safety job: the staging
// path the CLI submits must be the kind of directory nervud issued earlier through begin-staging, not an arbitrary path. It uses pure
// string path arithmetic (Linux semantics) + os.Lstat to refuse a symlink at the top, failing fast.
//
// Note: this is only a fast precheck. The trust-boundary-crossing operation that really commits staging as a code directory goes through
// authority's openat2(RESOLVE_BENEATH|RESOLVE_NO_SYMLINKS), which is the final guarantee.
func (s *Server) validateStagingChild(dir string) (string, error) {
	if dir == "" {
		return "", errors.New("install requires staging_dir")
	}
	if !path.IsAbs(dir) {
		return "", fmt.Errorf("staging_dir %q must be absolute", dir)
	}
	clean := path.Clean(dir)
	if path.Dir(clean) != s.stagingRoot {
		return "", fmt.Errorf("staging_dir %q is not a direct child of staging root %q", clean, s.stagingRoot)
	}
	// The top must not be a symlink (so nobody can trick it into committing a tree from elsewhere as staging). lstat doesn't follow.
	fi, err := os.Lstat(clean)
	if err != nil {
		return "", fmt.Errorf("staging_dir %q: %v", clean, err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("staging_dir %q is a symlink", clean)
	}
	if !fi.IsDir() {
		return "", fmt.Errorf("staging_dir %q is not a directory", clean)
	}
	return clean, nil
}

// cleanupStaging deletes a staging directory on a best-effort basis (on install failure / missing metadata). It only deletes after confirming
// it's still a child of the staging root, never recursing somewhere else because of a misdelivered path.
func (s *Server) cleanupStaging(dir string) {
	if path.Dir(path.Clean(dir)) != s.stagingRoot {
		return
	}
	if err := os.RemoveAll(dir); err != nil {
		s.log.Warn("admin: failed to clean staging dir", "dir", dir, "err", err)
	}
}

// entryToInfo projects a pkgregistry.Entry into the outward-facing PackageInfo.
func entryToInfo(e pkgregistry.Entry) adminwire.PackageInfo {
	return adminwire.PackageInfo{
		ID:          e.Manifest.PackageID,
		Version:     e.ActiveVersion,
		VersionCode: e.VersionCode,
		Trust:       e.Trust.String(),
		Source:      e.Source.String(),
		Granted:     e.GrantedPermissions,
		Disabled:    e.DisabledComponents,
	}
}

// grantStateFromWire maps a wire grant-state string to permission.GrantState.
func grantStateFromWire(s string) (permission.GrantState, bool) {
	switch s {
	case adminwire.GrantStateNotRequested:
		return permission.GrantStateNotRequested, true
	case adminwire.GrantStateGranted:
		return permission.GrantStateGranted, true
	case adminwire.GrantStateDenied:
		return permission.GrantStateDenied, true
	case adminwire.GrantStateDeniedPermanent:
		return permission.GrantStateDeniedPermanent, true
	default:
		return 0, false
	}
}

// riskClassToWire maps a RiskClass to adminwire's string form.
//
// adminwire is a leaf package and doesn't import ipcv1 (the same reason as GrantState), so the mapping is in this layer.
//
// Unknown values map to UNSPECIFIED rather than leaking the raw number: when the confirmation screen gets a level it doesn't know,
// it should treat it as "most uncertain" (no downgraded display), not show a number it can't explain.
func riskClassToWire(rc ipcv1.RiskClass) string {
	switch rc {
	case ipcv1.RiskClass_RISK_CLASS_NORMAL:
		return adminwire.RiskClassNormal
	case ipcv1.RiskClass_RISK_CLASS_PRIVACY_SENSITIVE:
		return adminwire.RiskClassPrivacySensitive
	case ipcv1.RiskClass_RISK_CLASS_PHYSICAL_CONTROL:
		return adminwire.RiskClassPhysicalControl
	case ipcv1.RiskClass_RISK_CLASS_CRITICAL_SAFETY:
		return adminwire.RiskClassCriticalSafety
	default:
		return adminwire.RiskClassUnspecified
	}
}

// classifyPkgErr classifies pkgregistry errors into wire Codes: "not installed" / "component not found" kinds
// go to CodeNotFound (so the CLI can give better wording / exit codes), everything else to CodeFailed.
func classifyPkgErr(err error) adminwire.Response {
	code := adminwire.CodeFailed
	if errors.Is(err, pkgregistry.ErrPackageNotInstalled) || errors.Is(err, pkgregistry.ErrComponentNotFound) {
		code = adminwire.CodeNotFound
	}
	return adminwire.Response{OK: false, Code: code, Message: err.Error()}
}

func badRequest(format string, args ...any) adminwire.Response {
	return adminwire.Response{OK: false, Code: adminwire.CodeBadRequest, Message: fmt.Sprintf(format, args...)}
}

func failed(format string, args ...any) adminwire.Response {
	return adminwire.Response{OK: false, Code: adminwire.CodeFailed, Message: fmt.Sprintf(format, args...)}
}
