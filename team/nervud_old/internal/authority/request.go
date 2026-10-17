// This file defines the request types and exported methods of the privileged operations. Each operation = one XxxRequest
// (Kind + Validate + an optional Detail) + one Gate method whose body is a single do(...) line.
//
// Only the operations whose callers' needs are already settled are implemented:
//
//	CreatePrivateDataDirectory / SetOwner / Reboot
//
// The request shapes of the other 5 Kinds (PrepareAppIdentity, InstallVerifiedPackage, StartSandboxedProcess,
// StopProcess, EnableFsVerity) will be forced out by their first real callers (identity /
// pkgregistry / service); defining them now would only be guessing. StopProcess in particular
// has to wait for service: a bare PID has a reuse race (once the PID is recycled you kill the wrong process). The right answer is for service
// to hold a pidfd from spawn and for Authority to signal the pidfd, which changes the request's fields directly.
package authority

import (
	"context"
	"fmt"
)

// ---- CreatePrivateDataDirectory ------------------------------------------

// CreateDataDirRequest creates one private App data directory.
// The parent must already exist: no MkdirAll, because creating each level should be an explicit privileged operation.
type CreateDataDirRequest struct {
	Path string // must be under Invariants.DataRoot (an absolute Linux path, slash-separated)
	UID  uint32 // owner; must be in the App UID range
	GID  uint32
	Perm uint32 // e.g. 0o700; must not be open to group/other
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
	// A private data directory must not be open to group/other: that is the definition of private, not a policy choice
	if r.Perm&0o077 != 0 {
		return fmt.Errorf("%w: perm %#o exposes private data dir to group/other",
			ErrInvariantViolated, r.Perm)
	}
	return nil
}

// DirHandle is a handle to a created directory. It deliberately does not expose the raw fd:
// once a raw fd leaves the package, any module can write or mmap through it, and the Gate is pointless.
type DirHandle struct{ Path string }

func (g *Gate) CreatePrivateDataDirectory(
	ctx context.Context, subj Subject, req CreateDataDirRequest,
) (DirHandle, error) {
	return do(ctx, g, subj, req, g.osCreateDataDir)
}

// ---- SetOwner -------------------------------------------------------------

// SetOwnerRequest changes the owner of a single path inside DataRoot (not recursive; a symlink itself is not followed).
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

// RebootRequest reboots the machine. The normal recovery path is to fail fast and let systemd restart
// nervud (with StartLimitAction=reboot as the backstop); this operation is for when a full reboot is really intended
// (such as committing a system update).
type RebootRequest struct {
	Reason string // required: a reboot without a reason is refused; the audit must be able to say why
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
