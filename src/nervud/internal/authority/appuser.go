// This file is the privileged operation "register an allocated App UID in the system user database".
//
// # Why the kernel has to do this
//
// systemd's User=, even when given a numeric UID, requires it to resolve through NSS to a real user;
// otherwise spawn fails at step USER with exit code 217/USER. And pkgregistry never registers the UIDs it
// allocates, so on a system without these users pre-created, no Package component
// could start.
//
// # Why it can't be handed to a system service
//
// Deadlock: systemd requires a package component's UID to resolve before starting it, and a system service is itself a package
// component. The "service that creates users" couldn't start itself. That's not a trade-off; it's a conclusion forced by
// the bootstrap order.
//
// # Why write the files directly rather than call useradd
//
//  1. Embedded images may not have shadow-utils. Depending on an external binary that may not exist
//     fails as "components won't start" rather than "build error", which is very expensive to debug.
//  2. UIDs are never reclaimed (the allocator is a monotonic high-water mark; see scan.go), so this only appends
//     and never edits or deletes. Append-only makes operating on the files directly much safer: there's no rewriting the whole file,
//     and so no chance of "crashed halfway through a rewrite" corrupting /etc/passwd.
//
// It still holds the exclusive lock on /etc/.pwd.lock per shadow-utils convention, for mutual exclusion with other user management
// tools on the system.
package authority

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

const (
	passwdPath = "/etc/passwd"
	groupPath  = "/etc/group"
	// pwdLockPath is the user database lock file by shadow-utils convention. Holding it gives mutual exclusion with other
	// user management tools on the system (useradd/usermod/vipw).
	pwdLockPath = "/etc/.pwd.lock"

	// appUserShell makes these accounts unable to log in. They're only identities systemd uses to set the UID,
	// and correspond to no person.
	appUserShell = "/usr/sbin/nologin"
	// appUserHome points at a path that doesn't exist: a package's private data directory isn't a home,
	// and making it the home would lead any library that finds its configuration through $HOME into the sandbox's writable area.
	appUserHome = "/nonexistent"
)

// ErrAppUserConflict: the target UID is already taken by a user that doesn't belong to nervus.
var ErrAppUserConflict = errors.New("authority: app uid already taken by a foreign user")

// EnsureAppUserRequest asks to register an allocated App UID in the system user database.
//
// Idempotent: does nothing when the entry already exists (and was created by us).
type EnsureAppUserRequest struct {
	UID uint32
	GID uint32
	// Name is the account name. The caller provides it, and this layer only checks its shape: the name goes into the first column of /etc/passwd,
	// and a colon or newline in it could forge extra entries.
	Name string
}

func (EnsureAppUserRequest) Kind() Kind { return KindEnsureAppUser }

func (r EnsureAppUserRequest) Detail() string {
	return fmt.Sprintf("%s uid=%d", r.Name, r.UID)
}

// Validate checks the invariants.
func (r EnsureAppUserRequest) Validate(inv *Invariants) error {
	if err := inv.CheckUID(r.UID); err != nil {
		return err
	}
	if err := inv.CheckUID(r.GID); err != nil {
		return err
	}
	if !validAppUserName(r.Name) {
		return fmt.Errorf("%w: invalid app user name %q", ErrInvariantViolated, r.Name)
	}
	return nil
}

// validAppUserName reports whether name is a safe account name.
//
// Colons and newlines are fatal: /etc/passwd is colon-separated with one entry per line. A name containing a newline
// could conjure up an entry with uid=0. The 32-character limit is useradd's convention, and also stops
// an overlong name from blowing up the line.
func validAppUserName(name string) bool {
	if len(name) == 0 || len(name) > 32 {
		return false
	}
	// It must have the nervus- prefix: that makes these accounts recognisable at a glance in /etc/passwd, and guarantees we
	// never touch an entry that isn't ours.
	if !strings.HasPrefix(name, "nervus-") {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-', c == '_':
		default:
			return false
		}
	}
	return true
}

// AppUserName gives the account name for a UID.
//
// The UID rather than the package_id is the suffix: package_id is a reversed domain name (com.example.app),
// contains dots and may exceed 32 characters, neither of which suits an account name. The UID is unique, fixed-length, and
// naturally matches the third column of /etc/passwd, so they line up at a glance when troubleshooting.
func AppUserName(uid uint32) string {
	return "nervus-app-" + strconv.FormatUint(uint64(uid), 10)
}

// EnsureAppUser registers an App UID in the system user database. Idempotent.
func (g *Gate) EnsureAppUser(ctx context.Context, subj Subject, req EnsureAppUserRequest) error {
	_, err := do(ctx, g, subj, req, g.osEnsureAppUser)
	return err
}

// existingUIDName looks up uid in path (passwd or group format) and returns its name.
//
// The first three columns of both files have the same layout (name:passwd:id:...), so they can share the parser.
func existingUIDName(path string, uid uint32) (string, bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false, fmt.Errorf("authority: read %s: %w", path, err)
	}
	want := strconv.FormatUint(uint64(uid), 10)
	for _, line := range strings.Split(string(data), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Split(line, ":")
		if len(f) < 3 {
			continue
		}
		if f[2] == want {
			return f[0], true, nil
		}
	}
	return "", false, nil
}

// appendLine appends one line with O_APPEND.
//
// O_APPEND makes the write atomic relative to the end of the file (the kernel guarantees no interleaving when a single write doesn't exceed PIPE_BUF),
// and together with the outer flock that's two guarantees stacked. It doesn't rewrite the whole file: crashing halfway through a rewrite
// means a corrupt /etc/passwd, a fault that can stop the system booting.
func appendLine(path, line string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		return fmt.Errorf("authority: open %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.WriteString(line); err != nil {
		return fmt.Errorf("authority: append to %s: %w", path, err)
	}
	return f.Sync()
}
