//go:build linux

// This file is one of the places in the whole repository allowed to touch Linux privileged interfaces directly.
// The depguard rules in .golangci.yml allow syscall / x/sys/unix / os/exec here for that reason.
//
// Path resolution discipline: invariant.go's string checks cannot stop a symlink escape; the real guarantee is here,
// done by the kernel. Always open root first to get an fd, then resolve relative to it with openat2(RESOLVE_BENEATH |
// RESOLVE_NO_SYMLINKS); once resolved, operate only on the fd or (dirfd, leaf),
// never on the full string path again.
package authority

import (
	"context"
	"fmt"

	"golang.org/x/sys/unix"

	"github.com/nervus-os/nervud/internal/audit"
)

// resolveParent opens path's parent directory and returns (parent fd, leaf name).
// The caller closes the returned fd. Precondition: Validate has proved that path is inside root.
func resolveParent(root, p string) (int, string, error) {
	rel, err := containedRel(p, root)
	if err != nil {
		// Validate already checked this; failing here means the call order was broken: return an error rather than panic
		return -1, "", err
	}

	// O_PATH: needs only the right to resolve the path, not to read it; O_NOFOLLOW: root itself must not be a symlink either
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

// splitRel splits a slash-separated relative path into its parent and its leaf. rel has no trailing slash and is already Clean.
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

	// mkdirat does not follow a symlink in the last component: if leaf is already a link we get EEXIST, which is safe by construction
	if err := unix.Mkdirat(parentFD, leaf, req.Perm); err != nil {
		return DirHandle{}, fmt.Errorf("mkdirat %s: %w", leaf, err)
	}

	// Reopen the new directory by fd and do every chown/chmod on the fd: no TOCTOU
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
	// mkdir's mode is reduced by the process umask, so chmod it back to the requested value explicitly
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

	// AT_SYMLINK_NOFOLLOW: if the target is a symlink, change the link's own owner, not the target's
	if err := unix.Fchownat(parentFD, leaf, int(req.UID), int(req.GID), unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return struct{}{}, fmt.Errorf("fchownat %s: %w", leaf, err)
	}
	return struct{}{}, nil
}

func (g *Gate) osReboot(ctx context.Context, req RebootRequest) (struct{}, error) {
	// Special case: a successful reboot(2) never returns, so the audit after run in do() is never reached.
	// Why the machine rebooted is a question the audit must be able to answer, so this operation first writes
	// an intent record. It is the only place in the package allowed to call the auditor from inside run, for that reason.
	g.auditor.Record(ctx, audit.Event{
		Action: "Reboot.initiated", Subject: "kernel", Detail: req.Reason,
	})
	// reboot right after sync: reboot(2) takes effect at once and gives the layers above no chance to flush
	unix.Sync()
	if err := unix.Reboot(unix.LINUX_REBOOT_CMD_RESTART); err != nil {
		return struct{}{}, fmt.Errorf("reboot: %w", err)
	}
	return struct{}{}, nil
}
