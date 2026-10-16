// This file is part of the integrity re-check: it checks each file's content against the digest list the manifest declares.
// Pure computation that touches no privileged interface, so it isn't bound by depguard's syscall boundary
package pkgregistry

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const (
	// ManifestFileName is the fixed file name of the in-package manifest
	ManifestFileName = "manifest.json"
	// SignatureFileName is the fixed file name of the in-package detached signature
	SignatureFileName = "manifest.sig"
)

// ErrDigestMismatch: at least one file's actual content doesn't match the digest the manifest declares
var ErrDigestMismatch = errors.New("pkgregistry: digest verification failed")

// ErrIrregularFile: the digest re-check met a non-regular file (symlink/FIFO/device node and so on).
// These can't be hashed (a symlink would be followed outside the package, a FIFO/device would block io.Copy forever),
// so they're always refused: only regular files and directories are allowed in a package
var ErrIrregularFile = errors.New("pkgregistry: refusing to hash non-regular file")

// DigestDiff is the structured result of an integrity re-check.
//
// It returns the full difference rather than stopping at the first mismatch: audit needs to record exactly what's wrong
// with the package, not just a "wrong" boolean
type DigestDiff struct {
	Mismatched []string // the manifest declares a digest, but the hash of the content on disk differs
	Missing    []string // the manifest declares it, but the file can't be found on disk
	Extra      []string // exists on disk, but the manifest doesn't declare it
}

// Clean reports whether this re-check passed completely
func (d DigestDiff) Clean() bool {
	return len(d.Mismatched) == 0 && len(d.Missing) == 0 && len(d.Extra) == 0
}

// VerifyDigests checks the actual files under root against digests (in-package relative path -> sha256 hex).
//
// The keys of digests must already have passed validRelPath (ParseManifest has done that); this function
// doesn't recheck path safety and trusts that the caller has already refused path traversal.
//
// manifest.json and manifest.sig are the package's metadata and can't be covered by
// its own digest list (the manifest can't hash itself, and the sig can't be written into the manifest before signing),
// so these two file names are exempt from the Extra check. Their integrity is guaranteed indirectly by the chain "the signature covers the raw manifest
// bytes + digests cover every other file", not by being listed in digests
func VerifyDigests(root string, digests map[string]string) (DigestDiff, error) {
	var diff DigestDiff
	seen := make(map[string]bool, len(digests))

	for rel, want := range digests {
		seen[rel] = true
		full := filepath.Join(root, filepath.FromSlash(rel))
		got, err := sha256File(full)
		switch {
		case errors.Is(err, os.ErrNotExist):
			diff.Missing = append(diff.Missing, rel)
			continue
		case err != nil:
			return DigestDiff{}, fmt.Errorf("pkgregistry: hash %s: %w", rel, err)
		case !strings.EqualFold(got, want):
			diff.Mismatched = append(diff.Mismatched, rel)
		}
	}

	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		// Only regular files are allowed: symlinks/FIFOs/device nodes and other non-regular types are always refused. A symlink would
		// be followed outside the package, and a FIFO/device would block io.Copy forever (a W^X and
		// integrity precondition). Uses DirEntry.Type, which doesn't follow symlinks
		if !d.Type().IsRegular() {
			return fmt.Errorf("%w: %s (mode %v)", ErrIrregularFile, p, d.Type())
		}
		rel, rerr := filepath.Rel(root, p)
		if rerr != nil {
			return rerr
		}
		rel = filepath.ToSlash(rel)
		// Metadata isn't in its own digest table, otherwise the manifest couldn't form finite self-checking content
		if rel == ManifestFileName || rel == SignatureFileName {
			return nil
		}
		if !seen[rel] {
			diff.Extra = append(diff.Extra, rel)
		}
		return nil
	})
	if err != nil {
		return DigestDiff{}, fmt.Errorf("pkgregistry: walk %s: %w", root, err)
	}

	sort.Strings(diff.Mismatched)
	sort.Strings(diff.Missing)
	sort.Strings(diff.Extra)
	return diff, nil
}

func sha256File(path string) (string, error) {
	// Lstat first (not following symlinks) to confirm it's a regular file: a symlink would be followed to a target outside the package,
	// and a FIFO/device/socket would block io.Copy forever. os.Open
	// follows symlinks, so Lstat has to gatekeep before opening
	li, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !li.Mode().IsRegular() {
		return "", fmt.Errorf("%w: %s (mode %v)", ErrIrregularFile, path, li.Mode())
	}

	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()

	// After opening, confirm with Fstat once more that it's still a regular file: there's a TOCTOU window between Lstat and Open,
	// and this second check stops a swap like "Lstat saw a regular file, by Open it was replaced with a FIFO" before io.Copy.
	// Fully eliminating TOCTOU needs O_NOFOLLOW (not portable), left for Linux-side hardening
	info, err := f.Stat()
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("%w: %s (mode %v)", ErrIrregularFile, path, info.Mode())
	}

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
