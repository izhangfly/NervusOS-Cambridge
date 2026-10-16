// This file unpacks a .nspkg (zstd-compressed tar) into the staging directory nervud designates.
//
// # This is pkgmanagerd's only "heavy lifting"
//
// Everything else is forwarding. And this step is the only part of the service with security responsibility: unpacking happens
// [before] handing over to nervud for review, so a malicious .nspkg with "../.." or absolute-path entries
// could write outside the target directory while being unpacked.
//
// The division of labour should be clear: nervud re-checks the content of every file in staging against manifest.Digests
// (VerifyDigests), so the trust anchor [for content] is nervud, not here. This file
// only guarantees that unpacking itself doesn't go out of bounds, which nervud can't do for us, because the escape happens before
// it ever sees the tree.
//
// The logic matches the kernel's cmd/nervusctl/unpack.go (proven in production).
// Copied deliberately rather than "improved": each of these guards answers a specific attack, and a rewrite would
// mean falling into the same holes again.
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

// maxEntryBytes caps the size of a single unpacked entry, stopping decompression-bomb-style huge entries.
// 64 MiB is plenty for normal App payloads; the real package size limits are enforced on the nervud/manifest side.
const maxEntryBytes = 64 << 20

// maxTotalEntries caps the number of entries in one .nspkg, stopping bombs made of huge numbers of small entries.
const maxTotalEntries = 10000

// Unpack unpacks nspkgPath into destDir (which must already exist).
//
// destDir must be the path nervud created with begin-staging and sent back: its owner and mode are controlled,
// and it's on the same filesystem as PackageRoot (so the renameat2 at install time can't fail across disks).
// Never pick a directory yourself, unpack into it, and hand it to nervud.
func Unpack(nspkgPath, destDir string) error {
	f, err := os.Open(nspkgPath)
	if err != nil {
		return fmt.Errorf("open nspkg: %w", err)
	}
	defer func() { _ = f.Close() }()

	zr, err := zstd.NewReader(f)
	if err != nil {
		return fmt.Errorf("zstd reader: %w", err)
	}
	defer zr.Close()

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
			// Refuse symlinks, hard links, devices, FIFOs and every other non-regular type: they can either be used
			// to escape (a symlink pointing outside the target directory) or have no legitimate use in a signed App package.
			return fmt.Errorf("archive entry %q has unsupported type %d", hdr.Name, hdr.Typeflag)
		}
	}
	return nil
}

// safeJoin joins an entry name onto base safely, refusing every escape: empty names, absolute paths,
// and names that still escape base after cleaning (including ".." folding).
func safeJoin(base, name string) (string, error) {
	if name == "" {
		return "", errors.New("archive entry has empty name")
	}
	if strings.HasPrefix(name, "/") {
		return "", fmt.Errorf("archive entry %q is absolute", name)
	}
	target := filepath.Join(base, name)
	cleaned := filepath.Clean(target)
	// Must be strictly inside base (base itself doesn't count). The prefix comparison [includes the separator],
	// or /a/staging-evil would pass a naive prefix check for /a/staging.
	if cleaned != base && !strings.HasPrefix(cleaned, base+string(os.PathSeparator)) {
		return "", fmt.Errorf("archive entry %q escapes destination", name)
	}
	return cleaned, nil
}

// writeRegular writes the current tar entry as a regular file.
func writeRegular(tr *tar.Reader, target string, hdr *tar.Header) error {
	if hdr.Size < 0 || hdr.Size > maxEntryBytes {
		return fmt.Errorf("archive entry %q too large (%d bytes)", hdr.Name, hdr.Size)
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	// Only the low 9 permission bits are kept, explicitly masking setuid/setgid/sticky: regular files with those bits have no
	// legitimate use in an App package, and setuid landing on disk is a privilege-escalation risk.
	//
	// O_EXCL: a duplicate entry name fails outright instead of overwriting. tar allows duplicate names,
	// and "the later one overwrites the earlier" would let content that passed the digest check be quietly replaced.
	mode := os.FileMode(hdr.Mode).Perm()
	out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	// CopyN's limit is a second safety net: even if hdr.Size lies, nothing beyond the limit gets written.
	if _, err := io.CopyN(out, tr, hdr.Size); err != nil && !errors.Is(err, io.EOF) {
		_ = out.Close()
		return fmt.Errorf("write %q: %w", target, err)
	}
	return out.Close()
}
