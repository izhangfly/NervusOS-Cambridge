//go:build linux

package preflight

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/nervus-os/nervud/internal/authority"
	"github.com/nervus-os/nervud/internal/sysprobe"
)

func selfOwner() (uint32, uint32) {
	return uint32(os.Geteuid()), uint32(os.Getegid())
}

func baseCfg(rules ...Rule) Config {
	uid, gid := selfOwner()
	return Config{Rules: rules, OwnerUID: uid, OwnerGID: gid}
}

func TestWritableDirCreatedWhenMissing(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "registry")
	cfg := baseCfg(Rule{Path: target, Kind: kindDir, Perm: 0o700, PermExact: true, Writable: true})

	if err := Run(cfg); err != nil {
		t.Fatalf("Run: %v", err)
	}
	st, err := sysprobe.LstatPath(target)
	if err != nil {
		t.Fatalf("lstat after create: %v", err)
	}
	if !st.IsDir || st.Perm != 0o700 {
		t.Fatalf("want dir 0700, got isDir=%v perm=%#o", st.IsDir, st.Perm)
	}
}

func TestWritableDirPermCorrected(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	cfg := baseCfg(Rule{Path: dir, Kind: kindDir, Perm: 0o755, PermExact: true, Writable: true})

	if err := Run(cfg); err != nil {
		t.Fatalf("Run: %v", err)
	}
	st, _ := sysprobe.LstatPath(dir)
	if st.Perm != 0o755 {
		t.Fatalf("perm not corrected: got %#o want 0755", st.Perm)
	}
}

func TestStickyBitPreserved(t *testing.T) {
	root := t.TempDir()
	runDir := filepath.Join(root, "nervus")
	cfg := baseCfg(Rule{Path: runDir, Kind: kindDir, Perm: 0o1755, PermExact: true, Writable: true})

	if err := Run(cfg); err != nil {
		t.Fatalf("Run: %v", err)
	}
	st, _ := sysprobe.LstatPath(runDir)
	if st.Perm != 0o1755 {
		t.Fatalf("sticky bit lost: got %#o want 01755", st.Perm)
	}
}

func TestReadOnlyMissingIsFatal(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope")
	cfg := baseCfg(Rule{Path: missing, Kind: kindDir, Perm: 0o755, PermExact: true, Writable: false})

	err := Run(cfg)
	if !errors.Is(err, ErrPreflight) {
		t.Fatalf("want ErrPreflight for missing read-only path, got %v", err)
	}
}

func TestReadOnlyWrongPermIsFatalNotCorrected(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	cfg := baseCfg(Rule{Path: dir, Kind: kindDir, Perm: 0o755, PermExact: true, Writable: false})

	if err := Run(cfg); !errors.Is(err, ErrPreflight) {
		t.Fatalf("want ErrPreflight, got %v", err)
	}
	st, _ := sysprobe.LstatPath(dir)
	if st.Perm != 0o777 {
		t.Fatalf("read-only path was modified: got %#o, must stay 0777", st.Perm)
	}
}

func TestForeignOwnerIsFatal(t *testing.T) {
	dir := t.TempDir()
	uid, gid := selfOwner()
	cfg := Config{
		OwnerUID: uid + 1, OwnerGID: gid,
		Rules: []Rule{{Path: dir, Kind: kindDir, Perm: 0o755, PermExact: true, Writable: true}},
	}
	if err := Run(cfg); !errors.Is(err, ErrPreflight) {
		t.Fatalf("want ErrPreflight for foreign-owned writable path, got %v", err)
	}
}

func TestSymlinkRejected(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(root, "real")
	if err := os.Mkdir(real, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	cfg := baseCfg(Rule{Path: link, Kind: kindDir, Perm: 0o755, PermExact: true, Writable: true})
	if err := Run(cfg); !errors.Is(err, ErrPreflight) {
		t.Fatalf("want ErrPreflight for symlinked path, got %v", err)
	}
}

func TestFileKindMismatchIsFatal(t *testing.T) {
	root := t.TempDir()
	f := filepath.Join(root, "x")
	if err := os.WriteFile(f, []byte("hi"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg := baseCfg(Rule{Path: f, Kind: kindDir, Perm: 0o755, PermExact: true, Writable: true})
	if err := Run(cfg); !errors.Is(err, ErrPreflight) {
		t.Fatalf("want ErrPreflight for kind mismatch, got %v", err)
	}
}

func TestNonExactPermOnlyChecksGroupOtherWrite(t *testing.T) {
	root := t.TempDir()
	f := filepath.Join(root, "bin")
	if err := os.WriteFile(f, []byte("x"), 0o555); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg := baseCfg(Rule{Path: f, Kind: kindFile, Perm: 0, PermExact: false, Writable: false})
	if err := Run(cfg); err != nil {
		t.Fatalf("0555 should pass non-exact check: %v", err)
	}

	if err := os.Chmod(f, 0o757); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	if err := Run(cfg); !errors.Is(err, ErrPreflight) {
		t.Fatalf("0757 (other-writable) must fail non-exact read-only check, got %v", err)
	}
}

// The user documents area must be exactly 01770, and must be PermExact.
//
// This isn't a style assertion; it's the security premise of the whole run-time grant mechanism: whether a package can write is decided
// by the u:<uid>:rwx ACL entry on the directory (see authority/acl_linux.go), and ACLs only have a say
// when the other bits are empty. This used to be 01777: with that value any package with the mount
// could read and write, grants and revocations all became no-ops, and no test went red.
//
// The sticky bit can't be dropped either: without it, any authorised package could delete other packages' and the user's files.
func TestDefaultConfigUserDataRootIsExactly01770(t *testing.T) {
	inv := authority.DefaultInvariants()
	if inv.UserDataRoot == "" {
		t.Fatal("DefaultInvariants didn't set UserDataRoot")
	}

	var found bool
	for _, r := range DefaultConfig(nil).Rules {
		if r.Path != inv.UserDataRoot {
			continue
		}
		found = true
		if r.Perm != 0o1770 {
			t.Errorf("UserDataRoot perm = %#o, want 01770", r.Perm)
		}
		if !r.PermExact {
			t.Error("UserDataRoot must be PermExact: a non-exact match only blocks the group/other w bits, " +
				"and would let 01777 through")
		}
		if r.Perm&0o007 != 0 {
			t.Errorf("UserDataRoot other bits = %#o; they must be empty, or the ACL means nothing", r.Perm&0o007)
		}
		if r.Perm&0o1000 == 0 {
			t.Error("UserDataRoot is missing the sticky bit")
		}
		if !r.Writable {
			t.Error("UserDataRoot should be in the writable area, or permission drift would only be reported, not fixed")
		}
	}
	if !found {
		t.Fatalf("the production rule table has no UserDataRoot (%s)", inv.UserDataRoot)
	}
}
