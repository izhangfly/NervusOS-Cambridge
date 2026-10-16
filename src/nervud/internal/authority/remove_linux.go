//go:build linux

// This file is the Linux implementation of RemovePackageTree: recursively deleting an installed Package's code or
// data directory, for the uninstall path.
//
// The same path resolution discipline as ops.go: first get the target directory's fd under the given Root with openat2(RESOLVE_BENEATH |
// RESOLVE_NO_SYMLINKS), then traverse and unlink relative to dirfds throughout,
// never going back to the full string path. Otherwise, if any level were replaced with a symlink during deletion, the rm -rf could
// be led outside Root. That's a hard requirement for recursive deletion as root
package authority

import (
	"context"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// maxRemoveDepth limits recursion depth, defending against deeply nested directories blowing the stack (normal package trees are far shallower)
const maxRemoveDepth = 64

func (g *Gate) osRemovePackageTree(_ context.Context, req RemovePackageTreeRequest) (struct{}, error) {
	// resolveParent already guarantees with openat2(RESOLVE_BENEATH|NO_SYMLINKS) that leaf is inside Root.
	// Note root here is declared explicitly by the request, and Validate has checked it's one of PackageRoot/DataRoot
	parentFD, leaf, err := resolveParent(req.Root, req.Path)
	if err != nil {
		return struct{}{}, err
	}
	defer func() { _ = unix.Close(parentFD) }()

	// First delete the contents of the leaf directory tree, then rmdir leaf itself
	if err := removeChild(parentFD, leaf, 0); err != nil {
		return struct{}{}, err
	}
	return struct{}{}, nil
}

// removeChild deletes the entry named name under parentFD (a file or a directory tree), operating relative to fds throughout.
//
// Semantics: first try unlinkat as a non-directory; EISDIR means it's a directory, so empty it recursively and rmdir. It uses
// unlinkat with AT_SYMLINK semantics (symlinks not followed by default): deleting a symlink deletes the
// link itself and never reaches through to the link target
func removeChild(parentFD int, name string, depth int) error {
	if depth > maxRemoveDepth {
		return fmt.Errorf("%w: remove recursion exceeds depth %d", ErrInvariantViolated, maxRemoveDepth)
	}

	// Delete as a non-directory first. unlinkat doesn't follow symlinks by default, so symbolic links are deleted safely here
	err := unix.Unlinkat(parentFD, name, 0)
	if err == nil {
		return nil
	}
	if err != unix.EISDIR && err != unix.EPERM {
		// ENOENT: already gone (concurrent/idempotent); treat as success
		if err == unix.ENOENT {
			return nil
		}
		return fmt.Errorf("unlinkat %s: %w", name, err)
	}
	// It's a directory: open it (not following symlinks, confined under parentFD) and empty it recursively
	how := unix.OpenHow{
		Flags:   unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC | unix.O_RDONLY,
		Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS,
	}
	dirFD, oerr := unix.Openat2(parentFD, name, &how)
	if oerr != nil {
		if oerr == unix.ENOENT {
			return nil
		}
		return fmt.Errorf("openat2 dir %s: %w", name, oerr)
	}
	if cerr := removeDirContents(dirFD, depth); cerr != nil {
		_ = unix.Close(dirFD)
		return cerr
	}
	_ = unix.Close(dirFD)

	// The contents are empty; delete the directory itself (relative to parentFD, not following symlinks)
	if rerr := unix.Unlinkat(parentFD, name, unix.AT_REMOVEDIR); rerr != nil && rerr != unix.ENOENT {
		return fmt.Errorf("rmdir %s: %w", name, rerr)
	}
	return nil
}

// removeDirContents walks the directory dirFD points at and deletes its entries one by one (recursively).
//
// Directory entries are read with an os.File wrapping a dup'd fd: ReadDir uses getdents internally and entries are only names;
// later deletions are still relative to dirFD, resolved by removeChild's openat2, so they're unaffected by races between the ReadDir snapshot and
// the real filesystem (removeChild tolerates ENOENT when something is already gone)
func removeDirContents(dirFD int, depth int) error {
	// os.NewFile takes ownership of the fd for ReadDir; so as not to affect the caller's
	// Close/later unlinkat on dirFD, a dup is given to the os.File here
	dup, err := unix.Dup(dirFD)
	if err != nil {
		return fmt.Errorf("dup dirfd: %w", err)
	}
	f := os.NewFile(uintptr(dup), "removedir")
	if f == nil {
		_ = unix.Close(dup)
		return fmt.Errorf("%w: os.NewFile on duped dirfd", ErrInvariantViolated)
	}
	names, rerr := f.Readdirnames(-1)
	_ = f.Close() // close the dup
	if rerr != nil {
		return fmt.Errorf("readdirnames: %w", rerr)
	}

	for _, n := range names {
		if n == "." || n == ".." {
			continue
		}
		if err := removeChild(dirFD, n, depth+1); err != nil {
			return err
		}
	}
	return nil
}
