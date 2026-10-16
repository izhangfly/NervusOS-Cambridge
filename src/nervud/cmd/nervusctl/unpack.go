// This file unpacks a .nspkg (zstd-compressed tar) into a target directory.
//
// Security responsibility (the only heavy lifting on the CLI side): unpacking happens before nervud reviews anything, so this
// must guard strictly against tar-slip / zip-slip. A malicious .nspkg with "../.." or absolute-path entries
// could write outside the target directory while being unpacked (worse still since the CLI runs as root). So each entry is checked:
// the name must be a clean relative path that doesn't escape the target directory, and only regular files and directories are accepted, refusing
// symlinks, hard links, devices and every other type that could be used to escape or escalate privileges.
//
// Note the division of labour: nervud re-checks the content of every file in staging against manifest.Digests
//
//	(VerifyDigests), so the trust anchor for content is nervud, not here; this file only guarantees that the unpacking
//
// step itself doesn't go out of bounds.
package main

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/klauspost/compress/zstd"
)

// maxEntryBytes caps the size of a single unpacked entry, stopping decompression-bomb-style huge entries. 64 MiB is
// plenty for normal App payloads; the real package size limits are enforced on the nervud/manifest side.
const maxEntryBytes = 64 << 20

// maxTotalEntries caps the number of entries in one .nspkg, stopping bombs made of huge numbers of small entries.
const maxTotalEntries = 10000

// unpackNspkg unpacks the .nspkg at nspkgPath into destDir (which must already exist). destDir is created
// by nervud with begin-staging and sent back, with controlled owner and permissions.
func unpackNspkg(nspkgPath, destDir string) error {
	f, err := os.Open(nspkgPath)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

	zr, err := zstd.NewReader(f)
	if err != nil {
		return fmt.Errorf("zstd reader: %w", err)
	}
	defer zr.Close()

	// The absolute, cleaned form of destDir, for the per-entry escape check.
	absDest, err := filepath.Abs(destDir)
	if err != nil {
		return err
	}
	absDest = filepath.Clean(absDest)

	tr := tar.NewReader(zr)
	count := 0
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("read tar: %w", err)
		}
		count++
		if count > maxTotalEntries {
			return fmt.Errorf("archive has too many entries (> %d)", maxTotalEntries)
		}

		target, err := safeJoin(absDest, hdr.Name)
		if err != nil {
			return err
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := writeRegular(tr, target, hdr); err != nil {
				return err
			}
		default:
			// Refuse symlinks, hard links, devices, FIFOs and every other non-regular type: they can either be used to escape
			//  (a symlink pointing outside the target directory) or have no legitimate use in a signed App package.
			return fmt.Errorf("archive entry %q has unsupported type %d", hdr.Name, hdr.Typeflag)
		}
	}
	return nil
}

// safeJoin joins entry name name onto base safely, refusing every escape: empty names, absolute paths,
// and names that still escape base after cleaning (including ".." folding). Returns the target's absolute path.
func safeJoin(base, name string) (string, error) {
	if name == "" {
		return "", errors.New("archive entry has empty name")
	}
	// tar names are '/'-separated; filepath.Clean behaves the same on linux. Absolute paths are refused outright.
	if strings.HasPrefix(name, "/") {
		return "", fmt.Errorf("archive entry %q is absolute", name)
	}
	target := filepath.Join(base, name)
	cleaned := filepath.Clean(target)
	// Must be strictly inside base (base itself doesn't count). The prefix comparison includes the separator, stopping
	// /a/staging-evil from passing a naive prefix check for /a/staging.
	if cleaned != base && !strings.HasPrefix(cleaned, base+string(os.PathSeparator)) {
		return "", fmt.Errorf("archive entry %q escapes destination", name)
	}
	return cleaned, nil
}

// writeRegular writes the current tar entry as a regular file, capping the entry size, creating any parent directories needed,
// and keeping the execute bits the manifest may rely on (the low 9 permission bits, without setuid/setgid/sticky).
func writeRegular(tr *tar.Reader, target string, hdr *tar.Header) error {
	if hdr.Size < 0 || hdr.Size > maxEntryBytes {
		return fmt.Errorf("archive entry %q too large (%d bytes)", hdr.Name, hdr.Size)
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	// Only the low 9 permission bits are kept; setuid/setgid/sticky are explicitly masked (regular files in a tar with those bits
	// have no legitimate use in an App package, and setuid landing on disk is a privilege-escalation risk).
	mode := os.FileMode(hdr.Mode).Perm()
	out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	// io.CopyN's limit is a second safety net: even if hdr.Size lies, nothing beyond the limit gets written.
	if _, err := io.CopyN(out, tr, hdr.Size); err != nil && !errors.Is(err, io.EOF) {
		_ = out.Close()
		return fmt.Errorf("write %q: %w", target, err)
	}
	return out.Close()
}
