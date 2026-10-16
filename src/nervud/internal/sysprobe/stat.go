//go:build linux

// This file is sysprobe's observation of path ownership and permissions (used by preflight).
//
// Like PeerCred it meets the purely-observational rule: it only reads the result of lstat(2), changes no system state
// and holds no capabilities. preflight's decision logic (which paths, what rules, whether fatal)
// lives in internal/preflight; this file only provides the one fact it needs that can't be had
// without touching syscall: a path's owner UID/GID, permission bits and file type.
package sysprobe

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// PathStat is a path's ownership and permission facts, from lstat(2).
//
// lstat rather than stat: symlinks aren't followed. If a directory that should be root-owned is
// replaced with a symlink pointing elsewhere, preflight must see the symlink itself (IsSymlink=true)
// and go fatal on it, rather than being carried transparently to the link target to check something unrelated.
type PathStat struct {
	UID uint32
	GID uint32
	// Perm is the low 12 bits of st_mode: setuid/setgid/sticky (0o7000) and rwx (0o777).
	// preflight compares the number directly (e.g. == 0o0755, or & 0o1000 for sticky) instead of mapping to
	// fs.FileMode's scattered bit layout.
	Perm      uint32
	IsDir     bool
	IsRegular bool
	IsSymlink bool
}

// LstatPath returns path's ownership/permission facts. When path doesn't exist, the error satisfies
// errors.Is(err, fs.ErrNotExist) (unix.ENOENT implements it), so the caller can tell
// "doesn't exist" from a real I/O error.
func LstatPath(path string) (PathStat, error) {
	var st unix.Stat_t
	if err := unix.Lstat(path, &st); err != nil {
		return PathStat{}, fmt.Errorf("sysprobe: lstat %s: %w", path, err)
	}
	typ := st.Mode & unix.S_IFMT
	return PathStat{
		UID:       st.Uid,
		GID:       st.Gid,
		Perm:      uint32(st.Mode) & 0o7777,
		IsDir:     typ == unix.S_IFDIR,
		IsRegular: typ == unix.S_IFREG,
		IsSymlink: typ == unix.S_IFLNK,
	}, nil
}
