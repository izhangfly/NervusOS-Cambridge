package authority

import (
	"fmt"
	"path"
	"strings"
)

// Invariants are system-wide hard constraints that nobody has the power to waive.
//
// Division of labour: policy answers "is this allowed, now, for this subject" and is configurable,
// changing with the deployment; Invariants answer "what can never be true in this system", are not configurable and accept
// no exemption. Example: whether an App may create a data directory is policy;
// that the directory must be under DataRoot and its owner UID must not be 0 are Invariants.
//
// Every field is frozen at New and read-only at run time; there is no exported setter.
type Invariants struct {
	DataRoot    string // root of private App data, e.g. /var/lib/nervus/data
	PackageRoot string // root of installed Packages, e.g. /var/lib/nervus/packages
	MinAppUID   uint32 // lower bound of App UIDs/GIDs; anything below is reserved for the system
	MaxAppUID   uint32
}

// DefaultInvariants are the fixed values of the production image. Deliberately not read from a configuration file.
func DefaultInvariants() *Invariants {
	return &Invariants{
		DataRoot:    "/var/lib/nervus/data",
		PackageRoot: "/var/lib/nervus/packages",
		MinAppUID:   20000, // clear of the distribution's system and ordinary user ranges
		MaxAppUID:   59999,
	}
}

// CheckUID rejects root and any App identity outside the reserved range.
//
// UID 0 is Linux's special root identity, and App UID 0 is always forbidden. Apart from UID 0,
// a UID's number says nothing about its privilege, so this is not "bigger is safer", only range separation.
// GIDs use the same range and this same check.
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

// CheckContained checks that path p is strictly inside path root (root itself does not count as inside).
func (inv *Invariants) CheckContained(p, root string) error {
	_, err := containedRel(p, root)
	return err
}

// containedRel checks that p is strictly inside root and returns its slash-separated path relative to root.
// The rule has three lines of defence, and all three are needed:
//  1. The path must be absolute: a relative path's meaning depends on the process cwd, which a systemd unit
//     or even a run-time chdir can change, handing the security boundary to outside state.
//  2. Compare prefixes after Clean, with the prefix ending in "/": otherwise /var/lib/nervus/data-evil
//     passes a naive prefix check for /var/lib/nervus/data. Clean also folds "..",
//     and a path that escapes root after folding fails the prefix comparison too.
//
// Note: this function is pure string work and cannot stop a symlink escape. The real guarantee must
// come from the kernel: the execution path resolves with openat2(RESOLVE_BENEATH|RESOLVE_NO_SYMLINKS),
// see ops_linux.go. The string check is only the first, fail-fast line, not the final guarantee.
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
