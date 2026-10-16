// This file defines the request types and exported methods of each privileged operation. Each operation = one XxxRequest
// (Kind + Validate + optional Detail) + a Gate method that's a single do(...) line.
//
// Only operations whose caller shape is already settled are implemented:
//
//	CreatePrivateDataDirectory / SetOwner / Reboot / InstallVerifiedPackage
//
// The request shapes of the other 4 Kinds (PrepareAppIdentity, StartSandboxedProcess, StopProcess,
// EnableFsVerity) will be forced out by each one's first real caller (identity / service)
// when it lands; defining them now would only be guessing. StopProcess in particular must wait for service to settle:
// a bare PID has a reuse race (killing the wrong process after the PID is recycled), and the right answer is for service to hold a
// pidfd at spawn and for Authority to signal the pidfd, which would change the request fields directly. EnableFsVerity
// is postponed likewise: fs-verity is hardening, not a blocker on the v1 install path
package authority

import (
	"context"
	"fmt"
)

// ---- CreatePrivateDataDirectory ------------------------------------------

// CreateDataDirRequest creates an App private data directory.
// The parent directory must already exist: no MkdirAll; creating each directory level should be an explicit privileged operation
type CreateDataDirRequest struct {
	Path string // must be under Invariants.DataRoot (a slash-separated absolute Linux path)
	UID  uint32 // owner; must be in the App UID range
	GID  uint32
	Perm uint32 // such as 0o700; must not be open to group/other
}

func (CreateDataDirRequest) Kind() Kind { return KindCreatePrivateDataDir }

func (r CreateDataDirRequest) Detail() string { return r.Path }

func (r CreateDataDirRequest) Validate(inv *Invariants) error {
	if err := inv.CheckContained(r.Path, inv.DataRoot); err != nil {
		return err
	}
	if err := inv.CheckUID(r.UID); err != nil {
		return err
	}
	if err := inv.CheckUID(r.GID); err != nil {
		return err
	}
	// A private data directory must not be open to group/other: that's the definition of private, not a policy choice
	if r.Perm&0o077 != 0 {
		return fmt.Errorf("%w: perm %#o exposes private data dir to group/other",
			ErrInvariantViolated, r.Perm)
	}
	return nil
}

// DirHandle is the handle of a created directory. The bare fd is deliberately not exposed:
// once a bare fd leaves the package, any module can write/mmap it, and the Gate would be pointless
type DirHandle struct{ Path string }

func (g *Gate) CreatePrivateDataDirectory(
	ctx context.Context, subj Subject, req CreateDataDirRequest,
) (DirHandle, error) {
	return do(ctx, g, subj, req, g.osCreateDataDir)
}

// ---- SetOwner -------------------------------------------------------------

// SetOwnerRequest changes the owner of a single path inside DataRoot (not recursive; symbolic links themselves aren't followed)
type SetOwnerRequest struct {
	Path string // must be under Invariants.DataRoot
	UID  uint32
	GID  uint32
}

func (SetOwnerRequest) Kind() Kind { return KindSetOwner }

func (r SetOwnerRequest) Detail() string { return r.Path }

func (r SetOwnerRequest) Validate(inv *Invariants) error {
	if err := inv.CheckContained(r.Path, inv.DataRoot); err != nil {
		return err
	}
	if err := inv.CheckUID(r.UID); err != nil {
		return err
	}
	return inv.CheckUID(r.GID)
}

func (g *Gate) SetOwner(ctx context.Context, subj Subject, req SetOwnerRequest) error {
	_, err := do(ctx, g, subj, req, g.osSetOwner)
	return err
}

// ---- Reboot ---------------------------------------------------------------

// RebootRequest reboots the whole machine. The normal fault recovery path is a fail-fast exit + systemd restarting
// nervud (with StartLimitAction=reboot as the backstop); this operation is kept for cases that really need to reboot the machine actively
// (such as committing a system update)
type RebootRequest struct {
	Reason string // required: a reboot with no reason isn't accepted, and audit must be able to answer why
}

func (RebootRequest) Kind() Kind { return KindReboot }

func (r RebootRequest) Detail() string { return r.Reason }

func (r RebootRequest) Validate(*Invariants) error {
	if r.Reason == "" {
		return fmt.Errorf("%w: reboot without a reason is not accepted", ErrInvariantViolated)
	}
	return nil
}

func (g *Gate) Reboot(ctx context.Context, subj Subject, req RebootRequest) error {
	_, err := do(ctx, g, subj, req, g.osReboot)
	return err
}

// ---- PowerAction ----------------------------------------------------------

// PowerAction is the kind of orderly power action
type PowerAction uint8

const (
	// PowerActionUnspecified: the zero value is invalid, so uninitialised requests can't pass as legal operations.
	// The default must never be "reboot": a call that forgot a field mustn't reboot the machine
	PowerActionUnspecified PowerAction = iota
	PowerActionReboot
	PowerActionPowerOff
)

func (a PowerAction) String() string {
	switch a {
	case PowerActionReboot:
		return "reboot"
	case PowerActionPowerOff:
		return "poweroff"
	default:
		return "unspecified"
	}
}

// PowerRequest reboots/shuts down the whole machine in an orderly way (through systemd's full shutdown.target).
//
// For the division of labour with RebootRequest see the top of authority/systemd/power.go: that one is a reboot(2)
// hard reboot (fault recovery), this one is a normal power action the user starts
type PowerRequest struct {
	Action PowerAction
	Reason string // required: a shutdown with no reason isn't accepted, and audit must be able to answer why
}

func (PowerRequest) Kind() Kind { return KindPowerAction }

// Detail goes into the audit's Detail field. It carries the action too: under the same Kind,
// "reboot" and "shut down" are two different events, and audit must tell them apart
func (r PowerRequest) Detail() string { return r.Action.String() + ": " + r.Reason }

func (r PowerRequest) Validate(*Invariants) error {
	if r.Action != PowerActionReboot && r.Action != PowerActionPowerOff {
		return fmt.Errorf("%w: unknown power action %d", ErrInvariantViolated, r.Action)
	}
	if r.Reason == "" {
		return fmt.Errorf("%w: power action without a reason is not accepted", ErrInvariantViolated)
	}
	return nil
}

func (g *Gate) Power(ctx context.Context, subj Subject, req PowerRequest) error {
	_, err := do(ctx, g, subj, req, g.osPower)
	return err
}

// ---- SetUserDataAccess ----------------------------------------------------

// SetUserDataAccessRequest adds or removes a UID's entry in the user documents area's ACL, i.e.
// the runtime grant and revocation of perm.storage.user.
//
// It deliberately has no Path: this operation only acts on the single directory Invariants.UserDataRoot. Opening up a path
// parameter would turn Authority into a general chmod service, exactly the kind of "arbitrary execution capability"
// doc.go forbids. For semantics and implementation see acl_linux.go
type SetUserDataAccessRequest struct {
	UID     uint32
	Allowed bool
}

func (SetUserDataAccessRequest) Kind() Kind { return KindSetUserDataAccess }

// Detail goes into audit: granting and revoking are two different events, and the UID alone can't tell them apart
func (r SetUserDataAccessRequest) Detail() string {
	if r.Allowed {
		return fmt.Sprintf("grant uid=%d", r.UID)
	}
	return fmt.Sprintf("revoke uid=%d", r.UID)
}

func (r SetUserDataAccessRequest) Validate(inv *Invariants) error {
	if inv == nil || inv.UserDataRoot == "" {
		return fmt.Errorf("%w: user data root is not configured", ErrInvariantViolated)
	}
	// Only the App UID range is allowed. Letting system UIDs through would let one grant write root or some distribution service
	// into the user documents area's ACL, and nothing would ever remove that record again
	return inv.CheckUID(r.UID)
}

func (g *Gate) SetUserDataAccess(ctx context.Context, subj Subject, req SetUserDataAccessRequest) error {
	_, err := do(ctx, g, subj, req, g.osSetUserDataAccess)
	return err
}

// ReconcileUserDataAccessRequest replaces the named-user entries in the user documents area's ACL as a whole
// with UIDs.
//
// For reconciliation at startup: the ACL and _grants.json are persisted separately, and an uninstall that happened while nervud wasn't running
// leaves orphan ACL entries, which UID reuse would hand to a new package. See
// osReconcileUserDataAccess in acl_linux.go
//
// An empty UIDs is legal input, meaning "no package is currently granted", and the result is the ACL being removed entirely
type ReconcileUserDataAccessRequest struct {
	UIDs []uint32
}

func (ReconcileUserDataAccessRequest) Kind() Kind { return KindSetUserDataAccess }

func (r ReconcileUserDataAccessRequest) Detail() string {
	return fmt.Sprintf("reconcile uids=%v", r.UIDs)
}

func (r ReconcileUserDataAccessRequest) Validate(inv *Invariants) error {
	if inv == nil || inv.UserDataRoot == "" {
		return fmt.Errorf("%w: user data root is not configured", ErrInvariantViolated)
	}
	for _, uid := range r.UIDs {
		if err := inv.CheckUID(uid); err != nil {
			return err
		}
	}
	return nil
}

func (g *Gate) ReconcileUserDataAccess(
	ctx context.Context, subj Subject, req ReconcileUserDataAccessRequest,
) error {
	_, err := do(ctx, g, subj, req, g.osReconcileUserDataAccess)
	return err
}

// ---- InstallVerifiedPackage -------------------------------------------

// InstallVerifiedPackageRequest atomically commits a staging directory whose signatures, digests and permission decisions are complete
// as the final read-only code directory, so Authority doesn't reimplement package verification.
//
// This request only does "move + tighten the top-level owner", no compression/expansion/re-checking: compression and expansion are
// pkgmanagerd's job, re-checking is pkgregistry's, and Authority is only responsible for this one step,
// the only disk write with the right to cross the trust boundary
//
// It deliberately has no UID/GID fields: the final code directory's owner is nervud itself (see
// finishInstalledPackage in ops.go), not the Package's App UID. A read-only code directory must
// uphold the baseline "nobody may own their own code", and making the App UID the owner would let
// a compromised App chmod it back to writable and modify its own executable code
type InstallVerifiedPackageRequest struct {
	StagingDir string // the staging directory produced by pkgmanagerd and already re-checked by pkgregistry
	DestDir    string // must be under Invariants.PackageRoot: <PackageRoot>/<id>/<version>

	// ReplaceExisting allows overwriting an existing version directory at the same path, for same-version reinstalls
	//  (repairing a damaged install).
	//
	// Off by default, because "the same <id>/<version> appearing twice" usually means a duplicate commit or
	// a reused version number, and silently overwriting would swallow both errors. But pkgregistry's checkUpgrade
	// explicitly allows same-version reinstalls, and that policy couldn't be delivered before, because this layer always used
	// RENAME_NOREPLACE to refuse, and the install failed with a bare renameat2 EEXIST.
	//
	// It's an explicit switch rather than this layer deciding "is this a reinstall" itself: this layer can't see the registry's
	// records, so it can't decide. Whoever allows overwriting must be the layer that can see the context.
	ReplaceExisting bool
}

func (InstallVerifiedPackageRequest) Kind() Kind { return KindInstallVerifiedPackage }

func (r InstallVerifiedPackageRequest) Detail() string { return r.DestDir }

func (r InstallVerifiedPackageRequest) Validate(inv *Invariants) error {
	if err := inv.CheckContained(r.DestDir, inv.PackageRoot); err != nil {
		return err
	}
	if r.StagingDir == "" {
		return fmt.Errorf("%w: empty staging dir", ErrInvariantViolated)
	}
	return nil
}

func (g *Gate) InstallVerifiedPackage(ctx context.Context, subj Subject, req InstallVerifiedPackageRequest) error {
	_, err := do(ctx, g, subj, req, g.osInstallVerifiedPackage)
	return err
}
