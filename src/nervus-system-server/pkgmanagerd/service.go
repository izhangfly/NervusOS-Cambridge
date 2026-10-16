// Package pkgmanager is the implementation of nervus.pkgmanagerd: it offers Apps the software install
// interface and forwards internally to nervud over the admin channel.
//
// # It's a bridge, not a decision-maker
//
//	App (Kotlin) ──IPC──▶ pkgmanagerd ──adminwire──▶ nervud
//	                          │
//	                     unpack .nspkg (tar-slip guarded)
//
// Every security decision is in nervud's pkgregistry: multi-role signature checks, digest re-checks, OEM countersign admission,
// host ABI matching, upgrade decisions (downgrade protection + signing lineage continuity), permission intersection, UID allocation,
// atomic commit. This package does none of them, and [isn't allowed] to: writing any code shaped like
// "if ... then allow" here is a design error.
//
// There's only one reason it exists: an App can't be root and can't reach the root-only admin channel,
// so it needs a service running in the app UID range, explicitly let through by the kernel, to forward for it.
package main

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	pkgv1 "github.com/nervus-os/nervus-ipc/protocol/interface/pkgmanagerv1"
)

// Method IDs come straight from the proto-generated enum; [no local copy of the constants].
//
// A copy wouldn't just duplicate; it would quietly go stale: if the proto changed a method_id, the local constant wouldn't
// complain, it would route calls to the wrong method, a fault only found at run time that looks like
// "the feature is wrong". With the generated enum, a renamed or renumbered method in the proto breaks the build.
var (
	methodInstall  = uint32(pkgv1.PackageManagerMethod_PACKAGE_MANAGER_METHOD_INSTALL)
	methodUninstal = uint32(pkgv1.PackageManagerMethod_PACKAGE_MANAGER_METHOD_UNINSTALL)
	methodList     = uint32(pkgv1.PackageManagerMethod_PACKAGE_MANAGER_METHOD_LIST)
	methodSetComp  = uint32(pkgv1.PackageManagerMethod_PACKAGE_MANAGER_METHOD_SET_COMPONENT_ENABLED)
	methodInspect  = uint32(pkgv1.PackageManagerMethod_PACKAGE_MANAGER_METHOD_INSPECT)
)

// Service is pkgmanagerd's business implementation.
type Service struct {
	admin *Client
	log   *slog.Logger
}

// newService constructs a Service.
func newService(admin *Client, log *slog.Logger) *Service {
	return &Service{admin: admin, log: log}
}

// handoffRoot is where .nspkg files are handed over by Apps: the user documents area shared across packages.
//
// # Why it isn't this service's private directory
//
// It used to be /var/lib/nervus/package-data/nervus.pkgmanagerd, a 0700 directory
// owned by this service's UID that [no other package can write to]. So the InstallRequest contract
// ("the relative path of the .nspkg in the caller's private directory") and the implementation pointed at two different places,
// and no install started by an App could ever succeed: where the caller can write, this service can't read,
// and where this service can read, the caller can't write.
//
// # Why user-data and not a dedicated hand-off directory
//
// user-data is already the place for "user files shared across packages" (01777 sticky, like /tmp),
// and .nspkg files the user downloads or copies in land there anyway; the file manager lives there too. A dedicated
// hand-off directory would add a pure copy (the App moving the package from user-data to the hand-off)
// and a second shared area to maintain, buying no isolation: both are readable system-wide.
//
// So the read surface only grows to "the user's own files", never touching any package's private data directory.
//
// # Who gets this far
//
// The INSTALL method needs perm.pkg.install. This service makes no authorisation decision; the kernel decides by
// required_permission. Precisely because of that, a caller that gets here [is already] authorised to
// install packages, and choosing which file under user-data to install isn't an extra privilege.
const handoffRoot = "/var/lib/nervus/user-data"

// ErrUnsafeRelPath: the path the caller gave tries to escape the hand-off directory.
var ErrUnsafeRelPath = errors.New("nspkg_relpath escapes the handoff directory")

// InstallFromRelPath runs a complete install: ask nervud for a staging directory, unpack the .nspkg
// into it, then have nervud re-check and commit.
//
// relPath is a [path relative to the hand-off directory], not an absolute path. That's not fussiness:
// this service runs in a ProtectSystem=strict sandbox and can only read its own data directory,
// and an absolute path it can't read would only produce a vague "can't open".
//
// The path is checked once here (refusing absolute paths and ".." escapes), and nervud checks again.
// Two checks are deliberate: some other bug inside this process could corrupt relPath, and nervud's is
// the final line of defence across a trust boundary.
//
// expectedDigest is the manifest digest from the confirmation screen's Inspect, forwarded to nervud as-is.
// Empty means no check (install scripts have no Inspect step). This service doesn't compare it: the comparison must happen after
// signature verification, on the same bytes that land on disk, and only nervud meets those conditions.
func (s *Service) InstallFromRelPath(
	relPath string, consented []string, expectedDigest string,
) (*PackageInfo, error) {
	nspkgPath, err := resolveHandoff(relPath)
	if err != nil {
		return nil, err
	}
	return s.installFile(nspkgPath, consented, expectedDigest)
}

// InspectFromRelPath inspects a .nspkg read-only: unpack to staging and let nervud verify the signature and work out
// the USER_CONSENT permissions it asks for. **Installs nothing.**
//
// # Why it also needs staging + unpacking
//
// nervud has to read the package's manifest.json and manifest.sig, and it can't read this service's private
// data directory (each process has its own sandbox). Staging is the only hand-over point, so even
// "just having a look" goes through the same unpack flow.
//
// # Staging isn't consumed
//
// A successful install has nervud move staging away with renameat2; inspect moves nothing. The directory left behind
// is cleaned up by nervud at the next begin-staging (orphans older than an hour). This service
// has no permission to delete it and shouldn't have; that directory belongs to nervud.
//
// The cost is that "look once, then install" unpacks twice. That's right: the confirmation screen and the install are two independent user
// decisions that may be a long time apart, and reusing the first staging would let a package waiting to be installed hold
// nervud's staging root indefinitely.
func (s *Service) InspectFromRelPath(relPath string) (*InspectInfo, error) {
	nspkgPath, err := resolveHandoff(relPath)
	if err != nil {
		return nil, err
	}

	begin, err := s.admin.Do(Request{Cmd: CmdBeginStaging})
	if err != nil {
		return nil, fmt.Errorf("begin-staging: %w", err)
	}
	if !begin.OK {
		return nil, fmt.Errorf("begin-staging refused (%s): %s", begin.Code, begin.Message)
	}
	staging := begin.StagingDir

	// Unpack: the same Unpack as install (the tar-slip guard applies just the same).
	// A malformed package fails here, so the confirmation screen never shows anything from it
	if err := Unpack(nspkgPath, staging); err != nil {
		s.log.Warn("pkgmanager: inspect unpack failed", "nspkg", nspkgPath, "err", err)
		return nil, fmt.Errorf("unpack failed: %w", err)
	}

	res, err := s.admin.Do(Request{Cmd: CmdInspect, StagingDir: staging})
	if err != nil {
		return nil, fmt.Errorf("inspect: %w", err)
	}
	if !res.OK {
		// Keep nervud's code and message as-is: the most common reason an inspect is refused is an invalid signature,
		// which means something to the user; don't flatten it to "can't view"
		return nil, fmt.Errorf("inspect refused (%s): %s", res.Code, res.Message)
	}
	if res.Inspect == nil {
		// OK without a payload means the kernel contract is broken, not a business failure. Report it rather than returning
		// an empty list, which the confirmation screen would show as "this package asks for no sensitive permissions"
		return nil, fmt.Errorf("inspect: nervud replied OK but with no inspect payload")
	}
	return res.Inspect, nil
}

// resolveHandoff resolves a path relative to the hand-off directory to an absolute path, refusing every escape.
func resolveHandoff(relPath string) (string, error) {
	if relPath == "" {
		return "", fmt.Errorf("%w: empty", ErrUnsafeRelPath)
	}
	if filepath.IsAbs(relPath) {
		return "", fmt.Errorf("%w: %q is absolute", ErrUnsafeRelPath, relPath)
	}
	full := filepath.Join(handoffRoot, relPath)
	cleaned := filepath.Clean(full)
	// The prefix comparison [includes the separator], or /var/lib/nervus/user-data-evil would pass a naive
	// prefix check.
	if !strings.HasPrefix(cleaned, handoffRoot+string(os.PathSeparator)) {
		return "", fmt.Errorf("%w: %q", ErrUnsafeRelPath, relPath)
	}
	return cleaned, nil
}

// installFile is the shared implementation of install.
//
// consented goes to nervud as-is: this service [neither anticipates nor re-checks] it. Which entries actually become
// GRANTED is decided by the kernel from the Catalog (the intersection of the consent list, the install-time grant set and USER_CONSENT,
// silently ignoring anything outside it); filtering here would only create a second source of truth that can drift.
func (s *Service) installFile(
	nspkgPath string, consented []string, expectedDigest string,
) (*PackageInfo, error) {
	// 1. Have nervud create the staging directory.
	//
	// [nervud must create it; never pick one yourself]. It guarantees three things: the location is on the same filesystem as
	// PackageRoot (so renameat2 at install time can't fail with EXDEV), the owner and mode are controlled,
	// and the path-escape check at install has a clear criterion of "it must be a directory I issued".
	begin, err := s.admin.Do(Request{Cmd: CmdBeginStaging})
	if err != nil {
		return nil, fmt.Errorf("begin-staging: %w", err)
	}
	if !begin.OK {
		return nil, fmt.Errorf("begin-staging refused (%s): %s", begin.Code, begin.Message)
	}
	staging := begin.StagingDir
	s.log.Info("pkgmanager: staging allocated", "dir", staging)

	// 2. Unpack. The only step with security responsibility in this service (tar-slip guarded).
	//
	// On failure, staging is [not cleaned up]: nervud sweeps orphan directories older than an hour at the next
	// begin-staging. We have no permission to delete it and shouldn't have: that directory is nervud's
	// domain, and giving the forwarding layer delete rights would open one more hole to abuse.
	if err := Unpack(nspkgPath, staging); err != nil {
		s.log.Warn("pkgmanager: unpack failed", "nspkg", nspkgPath, "err", err)
		return nil, fmt.Errorf("unpack failed: %w", err)
	}

	// 3. Have nervud re-check and commit. Signatures, digests and decisions all happen over there.
	res, err := s.admin.Do(Request{
		Cmd:        CmdInstall,
		StagingDir: staging,
		// Forwarded as-is with no filtering: this service [doesn't know] which permissions are USER_CONSENT,
		// and shouldn't; that's a fact in the Catalog. nervud only stores the intersection of "the consent list ∩ the install-time
		// grant set ∩ USER_CONSENT" and silently ignores anything outside it, so filtering here
		// would only add a copy that could drift from the kernel's rules.
		ConsentedPermissions: consented,
		// Also only forwarded. The binding check is in nervud: it computes the digest after signature verification,
		// on the manifest that matches the tree on disk byte for byte, and this service meets neither condition.
		ExpectedManifestDigest: expectedDigest,
	})
	if err != nil {
		return nil, fmt.Errorf("install: %w", err)
	}
	if !res.OK {
		// Keep nervud's code and message as-is: why an install was refused (bad signature,
		// version downgrade, ABI mismatch, missing permission) means something to the user; don't flatten it here
		// to "install failed".
		return nil, fmt.Errorf("install refused (%s): %s", res.Code, res.Message)
	}
	return res.Package, nil
}

// Uninstall removes a dynamically installed Package.
//
// System image packages can't be uninstalled; nervud refuses with ErrSystemPackageImmutable (they follow
// whole-image OTA). This service doesn't anticipate it; it forwards and lets nervud give the reason.
func (s *Service) Uninstall(packageID string) error {
	res, err := s.admin.Do(Request{
		Cmd:       CmdUninstall,
		PackageID: packageID,
	})
	if err != nil {
		return fmt.Errorf("uninstall: %w", err)
	}
	if !res.OK {
		return fmt.Errorf("uninstall refused (%s): %s", res.Code, res.Message)
	}
	return nil
}

// List lists installed Packages.
func (s *Service) List() ([]PackageInfo, error) {
	res, err := s.admin.Do(Request{Cmd: CmdList})
	if err != nil {
		return nil, fmt.Errorf("list: %w", err)
	}
	if !res.OK {
		return nil, fmt.Errorf("list refused (%s): %s", res.Code, res.Message)
	}
	return res.Packages, nil
}

// SetComponentEnabled disables or enables a Component.
//
// Components on the protected list (pkgmanagerd itself, settings, permissionui, sessiond,
// safety.recovery) can't be stopped; nervud refuses. That baseline is in a compile-time switch in the kernel,
// not decided by this service; putting the decision here would let the forwarding layer decide whether the system can repair itself.
func (s *Service) SetComponentEnabled(packageID, componentID string, enabled bool) error {
	res, err := s.admin.Do(Request{
		Cmd:         CmdSetEnabled,
		PackageID:   packageID,
		ComponentID: componentID,
		Enabled:     enabled,
	})
	if err != nil {
		return fmt.Errorf("set-enabled: %w", err)
	}
	if !res.OK {
		return fmt.Errorf("disable/enable refused (%s): %s", res.Code, res.Message)
	}
	return nil
}

// SetPermission sets the grant state of a runtime (GrantUser) permission.
//
// [Note the v1 situation]: while the kernel's permission.V1GrantAll is on, runtime user confirmation is short-circuited
// and every declared permission passes. This method still delivers the command (nervud's
// SetRuntimeState still stores the state), but for now it has no effect on what Allowed returns.
// Once enforcement is back it takes effect automatically, with no change to this service.
func (s *Service) SetPermission(packageID, permission, state string) error {
	res, err := s.admin.Do(Request{
		Cmd:        CmdSetPermission,
		PackageID:  packageID,
		Permission: permission,
		GrantState: state,
	})
	if err != nil {
		return fmt.Errorf("set-permission: %w", err)
	}
	if !res.OK {
		return fmt.Errorf("permission change refused (%s): %s", res.Code, res.Message)
	}
	return nil
}
