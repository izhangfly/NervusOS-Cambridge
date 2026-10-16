//go:build linux

// This file is one of the places in the repository allowed to touch Linux privileged interfaces directly.
// .golangci.yml's depguard rule lets syscall / x/sys/unix / os/exec through on that basis.
//
// Path resolution discipline: invariant.go's string checks can't stop symlink escapes; the real guarantee is here,
// done by the kernel: always open root to get an fd first, then use openat2(RESOLVE_BENEATH |
// RESOLVE_NO_SYMLINKS) for fd-relative resolution; after resolving, only operate on the fd / (dirfd, leaf),
// never touching the full string path again
package authority

import (
	"context"
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"

	"github.com/nervus-os/nervud/internal/audit"
)

// resolveParent opens path's parent directory and returns (the parent fd, the last segment name).
// The caller closes the returned fd. Precondition: Validate has proved path is inside root
func resolveParent(root, p string) (int, string, error) {
	rel, err := containedRel(p, root)
	if err != nil {
		// Validate already checked this; failing here means the call order was broken, so return an error rather than panic
		return -1, "", err
	}

	// O_PATH: only the right to resolve the path, no read permission needed; O_NOFOLLOW: root itself may not be a symlink either
	rootFD, err := unix.Open(root, unix.O_PATH|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, "", fmt.Errorf("open root %s: %w", root, err)
	}

	dir, leaf := splitRel(rel)
	if dir == "" {
		return rootFD, leaf, nil // path is a direct child of root
	}

	how := unix.OpenHow{
		Flags:   unix.O_PATH | unix.O_DIRECTORY | unix.O_CLOEXEC,
		Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS,
	}
	parentFD, err := unix.Openat2(rootFD, dir, &how)
	_ = unix.Close(rootFD)
	if err != nil {
		return -1, "", fmt.Errorf("resolve %s under %s: %w", dir, root, err)
	}
	return parentFD, leaf, nil
}

// splitRel splits a slash-separated relative path into parent directory and last segment. rel has no trailing slash and is already Clean
func splitRel(rel string) (dir, leaf string) {
	for i := len(rel) - 1; i >= 0; i-- {
		if rel[i] == '/' {
			return rel[:i], rel[i+1:]
		}
	}
	return "", rel
}

func (g *Gate) osCreateDataDir(_ context.Context, req CreateDataDirRequest) (DirHandle, error) {
	parentFD, leaf, err := resolveParent(g.inv.DataRoot, req.Path)
	if err != nil {
		return DirHandle{}, err
	}
	defer func() { _ = unix.Close(parentFD) }()

	// mkdirat doesn't follow symlinks on the last segment: if leaf is already a link it returns EEXIST, which is naturally safe.
	// mkdirat succeeding = this call created the directory (if it already existed it would be EEXIST), so if any
	// later step fails, rolling back by deleting it is safe: what we delete is guaranteed to be what we just created
	if err := unix.Mkdirat(parentFD, leaf, req.Perm); err != nil {
		if errors.Is(err, unix.EEXIST) {
			// The directory already exists. For idempotent callers (the boot scan filling in each package's run prerequisites) that's a normal
			// result; wrap it as ErrAlreadyExists so they can recognise it without checking EEXIST themselves,
			// which would drag x/sys/unix into business packages, which depguard doesn't allow.
			return DirHandle{}, fmt.Errorf("%w: %s", ErrAlreadyExists, leaf)
		}
		return DirHandle{}, fmt.Errorf("mkdirat %s: %w", leaf, err)
	}

	dh, err := g.finishDataDir(parentFD, leaf, req)
	if err != nil {
		// Roll back: delete the empty directory just created. Without rollback you'd leave a half-finished directory owned by root with incomplete permissions,
		// and retries would hit EEXIST at mkdirat and could never fix it
		//
		// AT_REMOVEDIR only deletes empty directories: if leaf has meanwhile been replaced with a file or a non-empty directory
		//  (a concurrent attack), rmdir fails safely and never deletes the wrong thing. It operates relative to parentFD,
		// with the same path resolution as creation, not following symlinks
		if rmErr := unix.Unlinkat(parentFD, leaf, unix.AT_REMOVEDIR); rmErr != nil {
			// The rollback failed too: carry both errors out, so the rollback failure doesn't hide the main cause
			return DirHandle{}, fmt.Errorf("%w (rollback rmdir %s failed: %v)", err, leaf, rmErr)
		}
		return DirHandle{}, err
	}
	return dh, nil
}

// finishDataDir opens the directory just created and sets its owner and permissions.
//
// It's a separate function so osCreateDataDir has a single failure exit for rollback: this returns
// an error and that deletes the directory. Everything operates on the fd (Openat2, then Fchown/Fchmod), ruling out TOCTOU
func (g *Gate) finishDataDir(parentFD int, leaf string, req CreateDataDirRequest) (DirHandle, error) {
	how := unix.OpenHow{
		Flags:   unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC,
		Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS,
	}
	fd, err := unix.Openat2(parentFD, leaf, &how)
	if err != nil {
		return DirHandle{}, fmt.Errorf("open created dir %s: %w", leaf, err)
	}
	defer func() { _ = unix.Close(fd) }()

	if err := unix.Fchown(fd, int(req.UID), int(req.GID)); err != nil {
		return DirHandle{}, fmt.Errorf("fchown: %w", err)
	}
	// mkdir's mode is reduced by the process umask, so it must be chmodded back to the requested value explicitly
	if err := unix.Fchmod(fd, req.Perm); err != nil {
		return DirHandle{}, fmt.Errorf("fchmod: %w", err)
	}
	return DirHandle{Path: req.Path}, nil
}

func (g *Gate) osSetOwner(_ context.Context, req SetOwnerRequest) (struct{}, error) {
	parentFD, leaf, err := resolveParent(g.inv.DataRoot, req.Path)
	if err != nil {
		return struct{}{}, err
	}
	defer func() { _ = unix.Close(parentFD) }()

	// AT_SYMLINK_NOFOLLOW: when the target is a symlink, change the link's own owner, not the link target's
	if err := unix.Fchownat(parentFD, leaf, int(req.UID), int(req.GID), unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return struct{}{}, fmt.Errorf("fchownat %s: %w", leaf, err)
	}
	return struct{}{}, nil
}

// osInstallVerifiedPackage atomically commits the staging directory pkgregistry has already re-checked as
// <PackageRoot>/<id>/<version>
//
// The <id> level is only a container grouping versions, not an independent object in the permission sense: it has no
// owner/permission semantics of its own; the first time a Package is installed it's created with mkdirat as needed (EEXIST ignored),
// without requiring the parent directory to exist explicitly beforehand as CreateDataDir does
func (g *Gate) osInstallVerifiedPackage(_ context.Context, req InstallVerifiedPackageRequest) (struct{}, error) {
	rel, err := containedRel(req.DestDir, g.inv.PackageRoot)
	if err != nil {
		// Validate already checked this; failing here means the call order was broken
		return struct{}{}, err
	}
	idDir, version := splitRel(rel)
	if idDir == "" || version == "" {
		return struct{}{}, fmt.Errorf("%w: dest dir %q must be <PackageRoot>/<id>/<version>",
			ErrInvariantViolated, req.DestDir)
	}

	rootFD, err := unix.Open(g.inv.PackageRoot, unix.O_PATH|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return struct{}{}, fmt.Errorf("open package root %s: %w", g.inv.PackageRoot, err)
	}
	defer func() { _ = unix.Close(rootFD) }()

	if err := unix.Mkdirat(rootFD, idDir, 0o755); err != nil && !errors.Is(err, unix.EEXIST) {
		return struct{}{}, fmt.Errorf("mkdirat %s: %w", idDir, err)
	}

	how := unix.OpenHow{
		Flags:   unix.O_PATH | unix.O_DIRECTORY | unix.O_CLOEXEC,
		Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS,
	}
	idFD, err := unix.Openat2(rootFD, idDir, &how)
	if err != nil {
		return struct{}{}, fmt.Errorf("resolve %s under %s: %w", idDir, g.inv.PackageRoot, err)
	}
	defer func() { _ = unix.Close(idFD) }()

	// RENAME_NOREPLACE by default: if the target version directory already exists, fail as a whole instead of silently overwriting. The same
	// <id>/<version> appearing twice usually means a duplicate commit or a reused version number, and neither should be
	// swallowed silently. The source uses an absolute path: in renameat2(2), when pathname is absolute the matching
	// dirfd is ignored; isolating the staging directory itself is the job of pkgmanagerd's separate UID/
	// mount namespace, outside this call's responsibility
	//
	// With ReplaceExisting it uses RENAME_EXCHANGE instead: the two trees swap atomically, and at the same instant the new tree
	// takes its place the old tree lands on the staging path. Not "delete the old, then rename": that has a window in which
	// the package doesn't exist, and a power cut then gives a system whose records say it's installed but whose directory is missing.
	flags := uint(unix.RENAME_NOREPLACE)
	if req.ReplaceExisting {
		flags = unix.RENAME_EXCHANGE
	}
	if err := unix.Renameat2(unix.AT_FDCWD, req.StagingDir, idFD, version, flags); err != nil {
		return struct{}{}, fmt.Errorf("renameat2 %s -> %s: %w", req.StagingDir, req.DestDir, err)
	}

	if err := g.finishInstalledPackage(idFD, version); err != nil {
		// Roll back: rename the directory just moved in back to its original staging path. rename rather than
		// recursive deletion: this is already a whole directory tree moved over from pkgmanagerd, and recursively deleting
		// a directory whose ownership may be half changed is riskier
		//
		// The rollback must use the same flag as the forward move: a successful NOREPLACE forward move guarantees the original staging
		// path is free right now; an EXCHANGE forward move left the old tree there, so rolling back with NOREPLACE
		// would always hit EEXIST and couldn't roll back, leaving a package whose new tree is in place but never finished
		if rerr := unix.Renameat2(idFD, version, unix.AT_FDCWD, req.StagingDir, flags); rerr != nil {
			return struct{}{}, fmt.Errorf("%w (rollback rename to staging failed: %v)", err, rerr)
		}
		return struct{}{}, err
	}

	// After EXCHANGE the old tree is on the staging path; delete it.
	//
	// A failure here doesn't count as an install failure: the new version is atomically in place and finish passed, so the install
	// has happened. Failing to delete only leaves an old tree under the staging root, which admin's sweepStaleStaging
	// reclaims at the next begin-staging. Reporting a successful install as a failure for this would make
	// the caller reinstall, and the reinstall would take the same path again, inflating a disk reclamation problem into an endless loop
	if req.ReplaceExisting {
		if rerr := os.RemoveAll(req.StagingDir); rerr != nil && g.log != nil {
			g.log.Warn("authority: replaced package tree left behind in staging",
				"path", req.StagingDir, "err", rerr)
		}
	}
	return struct{}{}, nil
}

// finishInstalledPackage tightens the top-level owner and permissions of the version directory just committed.
//
// The owner is tightened to nervud itself (in production the account running nervud, usually root),
// not the Package's App UID: a read-only code directory must uphold the baseline "nobody may own their own code"
//
//	("neither Apps nor Services may modify their own executable code").
//
// If the owner were the App's own UID, a compromised App could chmod it back to writable and tamper with its own code.
//
// Only the top level is tightened: recursively tightening the owner/permissions of every file in the tree is more complete hardening, but pkgmanagerd
// itself hadn't landed yet and the staging isolation model was still embryonic, so this whole chain was at an early v1 stage;
// deep hardening is left as a TODO rather than hastily implementing a recursive walk that hasn't been thought through
func (g *Gate) finishInstalledPackage(parentFD int, leaf string) error {
	how := unix.OpenHow{
		Flags:   unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC,
		Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS,
	}
	fd, err := unix.Openat2(parentFD, leaf, &how)
	if err != nil {
		return fmt.Errorf("open installed dir %s: %w", leaf, err)
	}
	defer func() { _ = unix.Close(fd) }()

	if err := unix.Fchown(fd, os.Geteuid(), os.Getegid()); err != nil {
		return fmt.Errorf("fchown: %w", err)
	}
	if err := unix.Fchmod(fd, 0o755); err != nil {
		return fmt.Errorf("fchmod: %w", err)
	}
	return nil
}

func (g *Gate) osReboot(ctx context.Context, req RebootRequest) (struct{}, error) {
	// Special case: reboot(2) never returns on success, so the audit after run in do is never reached.
	// Why the machine rebooted is a question audit must be able to answer, so this operation writes an
	// intent record before executing. This is the only place in the package allowed to call the auditor directly inside run, for that reason
	g.auditor.Record(ctx, audit.Event{
		Action: "Reboot.initiated", Subject: "kernel", Detail: req.Reason,
	})
	// sync, then reboot immediately: reboot(2) takes effect at once, giving no upper layer any chance to flush
	unix.Sync()
	if err := unix.Reboot(unix.LINUX_REBOOT_CMD_RESTART); err != nil {
		return struct{}{}, fmt.Errorf("reboot: %w", err)
	}
	return struct{}{}, nil
}

// osPower starts an orderly reboot/shutdown through systemd.
func (g *Gate) osPower(ctx context.Context, req PowerRequest) (struct{}, error) {
	pm, ok := g.spawner.(PowerManager)
	if !ok {
		// No systemd backend (a test double / non-Linux wiring). Fail closed:
		// never fall back to a reboot(2) hard reboot, which means something else; the user asked for an "orderly shutdown"
		return struct{}{}, ErrUnsupportedPlatform
	}

	// The same special case as osReboot: success never returns (systemd starts shutting down at once, and this process
	// gets SIGTERM with it), so the audit after run in do is never reached. So the intent record is written first.
	// This is the second, and last, place in the package allowed to call the auditor directly inside run
	g.auditor.Record(ctx, audit.Event{
		Action: "PowerAction.initiated", Subject: "kernel", Detail: req.Detail(),
	})
	// No unix.Sync(): systemd's shutdown.target unmounts filesystems properly,
	// which is far more complete than grabbing a sync here

	var err error
	switch req.Action {
	case PowerActionReboot:
		err = pm.Reboot(ctx)
	case PowerActionPowerOff:
		err = pm.PowerOff(ctx)
	default:
		// Validate already blocked this; this is defence in depth: never guess at an unknown action
		return struct{}{}, fmt.Errorf("%w: unknown power action %d", ErrInvariantViolated, req.Action)
	}
	if err != nil {
		return struct{}{}, err
	}
	return struct{}{}, nil
}
