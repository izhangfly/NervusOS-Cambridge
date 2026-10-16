//go:build linux

// Package preflight is a one-off filesystem self-check at the very start of nervud's setup.
//
// It distinguishes two kinds of path and treats them oppositely:
//
//   - the read-only image area (/usr/libexec/nervus/nervud, /usr/lib/nervus/*, /usr/share/nervus/trust):
//     check only, never modify. A mismatch returns a fatal error, setup aborts and the kernel doesn't start.
//     These paths being right is part of image integrity: if /usr/share/nervus/trust is already
//     world-writable, the image has been damaged, and chmodding it back automatically would only hide signs of intrusion. The right
//     reaction is to refuse to start and raise the alarm, by the same logic as "a half-dead TCB is more dangerous than a restart".
//
//   - nervud's own writable area (/var/lib/nervus/*, /run/nervus/): check + set actively.
//     Missing means create, wrong permissions mean chmod back to the expected value. But if a path already exists and its owner isn't
//     nervud itself, an unprivileged process created it first (squatting); rather than chown it clean,
//     that's fatal (an explicit requirement for /run/nervus).
//
// Why a separate package rather than an authority operation: authority.Gate gathers run-time privileged operations that cross a trust
// boundary (committing staging as code, creating a private directory owned by some App UID). preflight is
// the kernel's one-off boot check of its own foundations, before any module Starts or any App connects,
// and crosses no trust boundary. Owner/permission reads go through sysprobe (pure observation), and create/fix through the standard library's os
//
//	(allowed by depguard); neither needs authority's narrow operation model.
package preflight

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"

	"github.com/nervus-os/nervud/internal/authority"
	"github.com/nervus-os/nervud/internal/sysprobe"
)

// ErrPreflight is the sentinel for any failed preflight check. Setup aborts start-up on it
var ErrPreflight = errors.New("preflight: filesystem self-check failed")

// entryKind is the file type a checked path should be
type entryKind uint8

const (
	kindDir entryKind = iota
	kindFile
)

// Rule is the check rule for one path
type Rule struct {
	Path string
	Kind entryKind
	// Perm is the expected low 12 permission bits (including setuid/setgid/sticky).
	// PermExact=true requires an exact match; false only requires group/other not to be writable (w bits 0)
	Perm      uint32
	PermExact bool
	// Writable=false: read-only area (check only, mismatch is fatal); true: writable area (create if missing, fix wrong permissions)
	Writable bool
}

// Config is the input of one preflight run
//
// OwnerUID/OwnerGID are parameters for testability: in production they're always nervud itself (usually root, i.e.
// os.Geteuid==0), and tests inject the current uid/gid with t.TempDir to run the whole path
type Config struct {
	Rules    []Rule
	OwnerUID uint32
	OwnerGID uint32
	Log      *slog.Logger
}

// DefaultConfig is the production image's fixed rule set (+ path table)
//
// PackageRoot/DataRoot come from authority.DefaultInvariants, so authority and this package don't each hold
// their own path constants and drift; the other fixed paths are hard-coded
func DefaultConfig(log *slog.Logger) Config {
	inv := authority.DefaultInvariants()
	return Config{
		OwnerUID: uint32(os.Geteuid()),
		OwnerGID: uint32(os.Getegid()),
		Log:      log,
		Rules: []Rule{
			// ---- read-only image area: check only, mismatch is fatal ----
			// The nervud binary: owned by root, not group/other writable (exact 0o755 isn't required;
			// 0o755 or the stricter 0o555 are allowed, only writability is blocked)
			{Path: "/usr/libexec/nervus/nervud", Kind: kindFile, Perm: 0, PermExact: false, Writable: false},
			{Path: "/usr/lib/nervus/system-packages", Kind: kindDir, Perm: 0o755, PermExact: true, Writable: false},
			{Path: "/usr/lib/nervus/jre", Kind: kindDir, Perm: 0o755, PermExact: true, Writable: false},
			{Path: "/usr/share/nervus/trust", Kind: kindDir, Perm: 0o755, PermExact: true, Writable: false},

			// ---- nervud's own writable area: check + set actively (parents first, children after) ----
			{Path: "/var/lib/nervus", Kind: kindDir, Perm: 0o755, PermExact: true, Writable: true},
			{Path: "/var/lib/nervus/registry", Kind: kindDir, Perm: 0o700, PermExact: true, Writable: true},
			{Path: "/var/lib/nervus/packages", Kind: kindDir, Perm: 0o755, PermExact: true, Writable: true},
			// Dynamic install staging root: where the install service unpacks a .nspkg, which nervud then
			// commits into PackageRoot with renameat2 (both under /var/lib/nervus = the same filesystem).
			//
			// 0711, not 0700: the install service runs under its own UID and must be able to pass through this root into
			// the stage-* directory nervud assigned it. With 0700 it couldn't even pass through,
			// and installs would fail with permission denied at the unpack step.
			//
			// "Intermediate install states can't be read by other processes" still holds, backed by something stronger:
			// 0711 stops others from listing what's in the root, and each stage-* subdirectory is 0700
			// and owned by the install service UID of that install (see the chown in admin.handleBeginStaging).
			// Even guessing the directory name doesn't get you in.
			//
			// This must stay consistent with the Chmod in admin.Server.Start. If the two disagreed about the same
			// directory's permissions, preflight fixes the writable area in place, so the two would overwrite each other on every
			// boot: it would work, but nobody could say what the final value is.
			{Path: "/var/lib/nervus/staging", Kind: kindDir, Perm: 0o711, PermExact: true, Writable: true},
			{Path: inv.PackageRoot, Kind: kindDir, Perm: 0o755, PermExact: true, Writable: true},
			{Path: inv.DataRoot, Kind: kindDir, Perm: 0o755, PermExact: true, Writable: true},
			// The user documents area shared across packages. 01770 = sticky + all other bits empty.
			//
			// [The other bits must be empty]. Who can enter this directory is decided by the POSIX ACL on it:
			// once the user agrees to perm.storage.user in Settings, nervud adds an entry u:<uid>:rwx for that package's UID
			// (see authority/acl_linux.go), and revoking deletes it. ACLs are evaluated at
			// open(2), so grants and revocations take effect at once for processes already running, with no
			// app restart.
			//
			// This used to be 01777. With that value the other bits are rwx, so any package with the mount could
			// read and write regardless of what the ACL said; in other words 01777 turned the whole run-time
			// grant mechanism into a no-op. The two must change together.
			//
			// Packages run under their own UID/GID (GID = e.UID in buildStartReq) and share no
			// supplementary groups, so the group bits don't do anything here either; what carries the sharing is
			// the set of named-user entries in the ACL, not the group bits.
			//
			// The sticky bit can't be dropped: without it, any authorised package could delete other packages'
			//  (and the user's) files.
			{Path: inv.UserDataRoot, Kind: kindDir, Perm: 0o1770, PermExact: true, Writable: true},
			{Path: "/var/lib/nervus/jvm-cache", Kind: kindDir, Perm: 0o755, PermExact: true, Writable: true},
			// The audit directory. 0700: the audit holds package ids, uids and refusal reasons, and its readers are
			// ops, not the Apps on the machine.
			//
			// Unlike the other writable areas, this directory is itself part of a security control: it's
			// the only place that answers afterwards "who was refused what, and when". Loosening its permissions would let
			// the audited read their own audit.
			{Path: "/var/lib/nervus/audit", Kind: kindDir, Perm: 0o700, PermExact: true, Writable: true},
			// The two roots of the shared area between services. 0755 rather than UserDataRoot's 01770:
			// these roots are only written by nervud (provisionAll creates per-package subdirectories under them),
			// and packages only write into their own subdirectory. Opening the roots for writing would let any package create directories in them,
			// bypassing the "one directory per package, owner = writer" structure.
			//
			// For the subdirectories' owners and permissions see pkgregistry.provisionAll.
			{Path: inv.SharedPersistRoot, Kind: kindDir, Perm: 0o755, PermExact: true, Writable: true},
			// /run is tmpfs and doesn't support chattr +i; the equivalent protection is a sticky parent: 01755 stops
			// non-root processes from creating or unlinking entries in it, so the sockets can't be deleted or replaced
			{Path: "/run/nervus", Kind: kindDir, Perm: 0o1755, PermExact: true, Writable: true},
			// The shared area's tmpfs root. Under /run/nervus = it follows tmpfs and disappears on reboot.
			// High-throughput run-time exchange (camera intermediate data and the like) must land here, not on disk:
			// writing tens of MiB/s continuously to eMMC burns through the flash's life.
			{Path: inv.SharedRuntimeRoot, Kind: kindDir, Perm: 0o755, PermExact: true, Writable: true},
		},
	}
}

// Run applies every rule in turn. If any read-only-area rule doesn't match, or any writable-area path is illegally occupied,
// it returns ErrPreflight (fatal); missing writable-area paths and permission drift are fixed in place and allowed through
func Run(cfg Config) error {
	for _, r := range cfg.Rules {
		if err := cfg.apply(r); err != nil {
			return err
		}
	}
	if cfg.Log != nil {
		cfg.Log.Info("preflight: filesystem self-check passed", "rules", len(cfg.Rules))
	}
	return nil
}

func (cfg Config) apply(r Rule) error {
	st, err := sysprobe.LstatPath(r.Path)
	notExist := errors.Is(err, fs.ErrNotExist)

	switch {
	case err != nil && !notExist:
		// A real I/O error (insufficient permission, corruption): neither area can continue
		return fmt.Errorf("%w: stat %s: %v", ErrPreflight, r.Path, err)

	case notExist:
		if !r.Writable {
			// Missing from the read-only image area = an incomplete image; never create it
			return fmt.Errorf("%w: required read-only path %s is missing", ErrPreflight, r.Path)
		}
		return cfg.create(r)

	default:
		return cfg.verify(r, st)
	}
}

// verify checks the type/owner/permissions of an existing path; the writable area fixes permission mismatches in place
func (cfg Config) verify(r Rule, st sysprobe.PathStat) error {
	// Any area: the root location replaced by a symlink is a danger sign (read-only area = the image was tampered with;
	// writable area = being lured into writing elsewhere). lstat doesn't follow it; refuse plainly
	if st.IsSymlink {
		return fmt.Errorf("%w: %s is a symlink", ErrPreflight, r.Path)
	}
	if r.Kind == kindDir && !st.IsDir {
		return fmt.Errorf("%w: %s is not a directory", ErrPreflight, r.Path)
	}
	if r.Kind == kindFile && !st.IsRegular {
		return fmt.Errorf("%w: %s is not a regular file", ErrPreflight, r.Path)
	}
	if st.UID != cfg.OwnerUID {
		// Read-only area: the image ownership is wrong. Writable area: a non-nervud process created it first (squatting); don't chown
		// its content clean, just fail (an explicit requirement for /run/nervus, extended to every writable area)
		return fmt.Errorf("%w: %s owned by uid %d, want %d", ErrPreflight, r.Path, st.UID, cfg.OwnerUID)
	}

	if !cfg.permOK(r, st.Perm) {
		if !r.Writable {
			return fmt.Errorf("%w: read-only path %s has perm %#o (want %s)",
				ErrPreflight, r.Path, st.Perm, describePerm(r))
		}
		// Writable area: fix to the expected permissions in place
		if err := os.Chmod(r.Path, fileMode(r.Perm)); err != nil {
			return fmt.Errorf("%w: chmod %s: %v", ErrPreflight, r.Path, err)
		}
		if cfg.Log != nil {
			cfg.Log.Warn("preflight: corrected permissions on writable path",
				"path", r.Path, "was", fmt.Sprintf("%#o", st.Perm), "now", fmt.Sprintf("%#o", r.Perm))
		}
	}
	return nil
}

// create creates a missing writable-area directory and sets its owner/permissions
func (cfg Config) create(r Rule) error {
	// MkdirAll tolerates existing parents; the leaf's permissions are reduced by umask, so it's chmodded back explicitly afterwards.
	// Parents (if MkdirAll created them this time) are each chmodded to their expected values by their own rules,
	// because parents come before children in the rule table
	if err := os.MkdirAll(r.Path, 0o755); err != nil {
		return fmt.Errorf("%w: mkdir %s: %v", ErrPreflight, r.Path, err)
	}
	if err := os.Chmod(r.Path, fileMode(r.Perm)); err != nil {
		return fmt.Errorf("%w: chmod %s: %v", ErrPreflight, r.Path, err)
	}
	if err := os.Chown(r.Path, int(cfg.OwnerUID), int(cfg.OwnerGID)); err != nil {
		return fmt.Errorf("%w: chown %s: %v", ErrPreflight, r.Path, err)
	}
	if cfg.Log != nil {
		cfg.Log.Info("preflight: created writable path", "path", r.Path, "perm", fmt.Sprintf("%#o", r.Perm))
	}
	return nil
}

// permOK reports whether the actual permissions perm satisfy the rule
func (cfg Config) permOK(r Rule, perm uint32) bool {
	if r.PermExact {
		return perm == r.Perm
	}
	// Not exact: only requires group/other not to be writable
	return perm&0o022 == 0
}

func describePerm(r Rule) string {
	if r.PermExact {
		return fmt.Sprintf("%#o", r.Perm)
	}
	return "group/other not writable"
}

// fileMode converts raw unix permission bits (including 0o1000 sticky / 0o2000 setgid / 0o4000 setuid)
// to os.FileMode. os.Chmod uses high flag bits such as ModeSticky for sticky/setuid/setgid,
// not unix's low 0o7000 bits, so they must be mapped explicitly, or 01755 would be treated as 0755 and lose the sticky bit
func fileMode(raw uint32) os.FileMode {
	m := os.FileMode(raw & 0o777)
	if raw&0o1000 != 0 {
		m |= os.ModeSticky
	}
	if raw&0o2000 != 0 {
		m |= os.ModeSetgid
	}
	if raw&0o4000 != 0 {
		m |= os.ModeSetuid
	}
	return m
}
