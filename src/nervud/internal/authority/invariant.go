package authority

import (
	"fmt"
	"path"
	"strings"
)

// Invariants are a set of system-level hard constraints that nobody may exempt.
//
// Division of labour: policy answers "right now, for this subject, is it allowed?", which is configurable
// and may vary by deployment; Invariants answer "what is never true in this system", which isn't configurable and accepts
// no exemption. For example: whether an App may create a data directory is policy;
// "the directory must be under DataRoot" and "the owner UID must not be 0" are Invariants.
//
// All fields are frozen at New and read-only at run time; there's no exported setter
type Invariants struct {
	DataRoot    string // root of Package private data, such as /var/lib/nervus/package-data
	PackageRoot string // root of dynamically installed Packages, such as /var/lib/nervus/packages
	// SystemPackageRoot is the read-only root of Packages built into the system image,
	// such as /usr/lib/nervus/system-packages.
	//
	// It must be separate from PackageRoot: the code of the two kinds of package lives in completely different places, and the layouts
	// differ too: dynamic installs are <root>/<pkg>/<version>/, the system image is <root>/<pkg>/
	//  (no version subdirectory; follows whole-image OTA, so several versions never coexist).
	//
	// The path containment check when starting processes must accept either of the two roots, otherwise system packages' executables
	// would be judged as "escaping PackageRoot" and refused.
	SystemPackageRoot string
	// UserDataRoot is the user documents area shared across Packages, such as /var/lib/nervus/user-data.
	//
	// The difference from DataRoot is fundamental: under DataRoot every package has a 0700 private directory and nobody
	// can see anybody else's; UserDataRoot is common ground, where the file manager, the file picker and every package that declared
	// perm.storage.user read and write the same files. The concept of "the user's documents" requires them
	// to see the same thing, which private directories can't express.
	//
	// The permission model is sticky (01777, the same semantics as /tmp): any package allowed in can create files
	// and read others' files, but only delete its own.
	//
	// [v1 trade-off] This means there's no per-package read protection, equivalent to shared external storage before Android's scoped storage.
	// It was chosen because v1 needs the most basic chain, "the file manager can manage it, the picker can pick from it, other
	// apps can open it", to work first, while per-package isolation needs one GID per package +
	// SupplementaryGroups or bind mounts, which is v2 work.
	//
	// Only packages that declared perm.storage.user get it (see service.readWritePaths);
	// packages that didn't can't even write to it under ProtectSystem=strict.
	UserDataRoot string

	// SharedRuntimeRoot / SharedPersistRoot are common areas for exchanging data between services;
	// under each, every package has a subdirectory owned by itself with mode 0755.
	//
	// The difference from UserDataRoot: that's "the user's documents", aimed at Apps and the file picker,
	// gated by the USER_CONSENT perm.storage.user; these two are where services exchange configuration, models
	// and caches, gated by perm.storage.shared. Mixing them would turn "a service wants to drop an intermediate file"
	// into "the user must agree to access their documents".
	//
	// 0755 rather than 0777: everyone can read and only the owner can write, which is exactly the semantics needed,
	// with no need to open write access to all. Fine-grained per-package write grants (ACLs) are left for v2.
	//
	// Boundary: this area only holds things "anyone with perm.storage.shared is entitled to see". Isolation of cross-package reads
	// here relies only on data directories being 0700 (see service.buildStartReq, whose
	// InaccessiblePaths lists only registry), so the 0755 shared area is readable system-wide.
	// Data that needs a finer permission bar (camera frames and the like) must go through Transfer's memory handles,
	// where the handle itself is the credential and there's no filesystem path to get around it.
	//
	// SharedRuntimeRoot is on tmpfs: runtime exchange, gone on reboot, and writing it doesn't wear the eMMC.
	// SharedPersistRoot is on disk: configuration and caches kept across reboots.
	SharedRuntimeRoot string
	SharedPersistRoot string

	MinAppUID uint32 // lower bound for App UIDs/GIDs; below it is reserved by the system
	MaxAppUID uint32
}

// DefaultInvariants are the fixed values of the production image. They aren't read from a configuration file.
//
// DataRoot uses package-data rather than data: it matches the path table, and the name says more clearly
// that this is Package private data rather than some generic data directory
func DefaultInvariants() *Invariants {
	return &Invariants{
		DataRoot:          "/var/lib/nervus/package-data",
		PackageRoot:       "/var/lib/nervus/packages",
		SystemPackageRoot: "/usr/lib/nervus/system-packages",
		UserDataRoot:      "/var/lib/nervus/user-data",
		// /run is tmpfs: writing it doesn't hit disk or wear the eMMC. If high-throughput intermediate data such as camera frames
		// landed on the eMMC, continuous writes would burn through the flash's lifetime
		SharedRuntimeRoot: "/run/nervus/shared",
		SharedPersistRoot: "/var/lib/nervus/shared",
		MinAppUID:         20000, // stays clear of the distribution's system and ordinary user ranges
		MaxAppUID:         59999,
	}
}

// CheckUID refuses root and every App identity outside the reserved range.
//
// UID 0 is Linux's special root identity, and App UID 0 is always forbidden; apart from UID 0,
// the size of a UID says nothing about privilege, so this isn't "bigger is safer", just range isolation.
// GIDs reuse the same range and this check
func (inv *Invariants) CheckUID(uid uint32) error {
	if uid == 0 {
		return fmt.Errorf("%w: uid 0 is never permitted for app identity", ErrInvariantViolated)
	}
	if uid < inv.MinAppUID || uid > inv.MaxAppUID {
		return fmt.Errorf("%w: uid %d outside app range [%d,%d]",
			ErrInvariantViolated, uid, inv.MinAppUID, inv.MaxAppUID)
	}
	return nil
}

// CheckContainedInCodeRoot checks that p is inside either code root (the dynamic install root or the system image root).
//
// Process start-up path checks use it rather than CheckContained(p, PackageRoot): system image packages'
// executables live under SystemPackageRoot, and accepting only PackageRoot would judge them all as escapes.
//
// When neither root matches it returns the error for PackageRoot: that's the more common case,
// and the root it names for comparison is more likely the one the caller wanted.
func (inv *Invariants) CheckContainedInCodeRoot(p string) error {
	if inv.SystemPackageRoot != "" {
		if err := inv.CheckContained(p, inv.SystemPackageRoot); err == nil {
			return nil
		}
	}
	return inv.CheckContained(p, inv.PackageRoot)
}

// CheckContained checks that path p is strictly inside path root (root itself doesn't count as inside)
func (inv *Invariants) CheckContained(p, root string) error {
	_, err := containedRel(p, root)
	return err
}

// containedRel checks that p is strictly inside root and returns the slash-separated path relative to root.
// The check has three lines of defence, all required:
//
//  1. It must be an absolute path: a relative path's meaning depends on the process cwd, which a systemd unit
//     or even a runtime chdir can change, handing the security boundary to external state
//  2. Compare prefixes after Clean, and the prefix must end with "/": otherwise /var/lib/nervus/package-data-evil
//     would pass a naive prefix check against /var/lib/nervus/package-data; ".." is also folded by Clean, and
//     paths that escape root after folding fail the prefix comparison too
//
// Note: this function is pure string arithmetic and can't stop symlink escapes. The real guarantee has to come
// from the kernel: execution paths resolve with openat2(RESOLVE_BENEATH|RESOLVE_NO_SYMLINKS);
// see ops_linux.go. The string check is only a fast-failing first line, not the final guarantee
func containedRel(p, root string) (string, error) {
	if !path.IsAbs(p) {
		return "", fmt.Errorf("%w: path %q must be absolute", ErrInvariantViolated, p)
	}
	cleaned := path.Clean(p)
	prefix := path.Clean(root) + "/"
	if !strings.HasPrefix(cleaned, prefix) {
		return "", fmt.Errorf("%w: path %q escapes root %q", ErrInvariantViolated, cleaned, root)
	}
	return cleaned[len(prefix):], nil
}
