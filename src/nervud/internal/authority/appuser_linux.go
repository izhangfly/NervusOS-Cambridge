//go:build linux

// This file is the Linux implementation of EnsureAppUser. Like ops.go it's one of the files "allowed to touch Linux privileged
// interfaces directly", and .golangci.yml's depguard lets x/sys/unix through on that basis.
package authority

import (
	"context"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// osEnsureAppUser registers the UID/GID in /etc/passwd and /etc/group. Idempotent.
//
// It holds the exclusive lock on /etc/.pwd.lock throughout: that's the shadow-utils convention, and holding it gives mutual exclusion with other
// user management tools on the system (useradd/usermod/vipw). Without the lock, two processes appending at once
// could write interleaved half-lines.
func (g *Gate) osEnsureAppUser(_ context.Context, req EnsureAppUserRequest) (struct{}, error) {
	var zero struct{}

	// O_CREAT: the lock file itself may not exist (a minimal image without shadow-utils).
	// 0600: it's only a lock and shouldn't be read or written by anyone else.
	lockFD, err := unix.Open(pwdLockPath, unix.O_RDWR|unix.O_CREAT|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return zero, fmt.Errorf("authority: open %s: %w", pwdLockPath, err)
	}
	defer func() { _ = unix.Close(lockFD) }()

	if err := unix.Flock(lockFD, unix.LOCK_EX); err != nil {
		return zero, fmt.Errorf("authority: lock %s: %w", pwdLockPath, err)
	}
	defer func() { _ = unix.Flock(lockFD, unix.LOCK_UN) }()

	if err := ensureGroupLocked(req); err != nil {
		return zero, err
	}
	if err := ensurePasswdLocked(req); err != nil {
		return zero, err
	}
	return zero, nil
}

// ensureGroupLocked appends the /etc/group entry (the caller holds the lock).
func ensureGroupLocked(req EnsureAppUserRequest) error {
	name, found, err := existingUIDName(groupPath, req.GID)
	if err != nil {
		return err
	}
	if found {
		// Already exists. If it isn't our name, this GID is taken by something else on the system:
		// never overwrite it, which would graft a real user group's memberships onto the App.
		if name != req.Name {
			return fmt.Errorf("%w: gid %d belongs to group %q, not %q",
				ErrAppUserConflict, req.GID, name, req.Name)
		}
		return nil
	}
	// name:passwd:gid:members
	return appendLine(groupPath, fmt.Sprintf("%s:x:%d:\n", req.Name, req.GID))
}

// ensurePasswdLocked appends the /etc/passwd entry (the caller holds the lock).
func ensurePasswdLocked(req EnsureAppUserRequest) error {
	name, found, err := existingUIDName(passwdPath, req.UID)
	if err != nil {
		return err
	}
	if found {
		if name != req.Name {
			return fmt.Errorf("%w: uid %d belongs to user %q, not %q",
				ErrAppUserConflict, req.UID, name, req.Name)
		}
		return nil
	}
	// name:passwd:uid:gid:gecos:home:shell
	//
	// gecos holds the package's purpose rather than being empty: /etc/passwd is a file ops read directly when troubleshooting,
	// and a bare nervus-app-20000 with no explanation would only confuse.
	return appendLine(passwdPath, fmt.Sprintf("%s:x:%d:%d:Nervus OS app sandbox identity:%s:%s\n",
		req.Name, req.UID, req.GID, appUserHome, appUserShell))
}

// Make sure os is referenced (appendLine in appuser.go uses it; this is only for readability under build tags)
var _ = os.O_APPEND
