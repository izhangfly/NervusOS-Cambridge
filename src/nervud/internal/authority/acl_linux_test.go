//go:build linux

package authority

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"testing"

	"golang.org/x/sys/unix"
)

func newACLGate(t *testing.T, userDataRoot string) *Gate {
	t.Helper()
	g, err := New(Config{
		Auditor: &fakeRecorder{},
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		Invariants: &Invariants{
			DataRoot:     userDataRoot,
			PackageRoot:  userDataRoot,
			UserDataRoot: userDataRoot,
			MinAppUID:    20000,
			MaxAppUID:    59999,
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return g
}

// With the wrong encoding order setxattr returns EINVAL straight away, and that error doesn't say it's an ordering problem.
// The kernel requires USER_OBJ, USER..., GROUP_OBJ, GROUP..., MASK, OTHER
func TestMarshalACLOrdersEntriesForKernel(t *testing.T) {
	unsorted := []aclEntry{
		{tag: aclTagOther, perm: 0, id: aclUndefinedID},
		{tag: aclTagUser, perm: aclPermRWX, id: 20005},
		{tag: aclTagMask, perm: aclPermRWX, id: aclUndefinedID},
		{tag: aclTagUser, perm: aclPermRWX, id: 20001},
		{tag: aclTagUserObj, perm: aclPermRWX, id: aclUndefinedID},
		{tag: aclTagGroupObj, perm: aclPermRWX, id: aclUndefinedID},
	}

	round, err := parseACL(marshalACL(unsorted))
	if err != nil {
		t.Fatalf("parseACL: %v", err)
	}
	want := []aclEntry{
		{tag: aclTagUserObj, perm: aclPermRWX, id: aclUndefinedID},
		{tag: aclTagUser, perm: aclPermRWX, id: 20001},
		{tag: aclTagUser, perm: aclPermRWX, id: 20005},
		{tag: aclTagGroupObj, perm: aclPermRWX, id: aclUndefinedID},
		{tag: aclTagMask, perm: aclPermRWX, id: aclUndefinedID},
		{tag: aclTagOther, perm: 0, id: aclUndefinedID},
	}
	if len(round) != len(want) {
		t.Fatalf("entry count = %d, want %d", len(round), len(want))
	}
	for i := range want {
		if round[i] != want[i] {
			t.Errorf("entry %d = %+v, want %+v", i, round[i], want[i])
		}
	}
}

// Empty input means "this directory has no ACL yet", the normal state, not an error
func TestParseACLTreatsAbsentAsEmpty(t *testing.T) {
	entries, err := parseACL(nil)
	if err != nil || entries != nil {
		t.Fatalf("parseACL(nil) = %v, %v; want nil, nil", entries, err)
	}
	if _, err := parseACL([]byte{1, 2, 3}); err == nil {
		t.Error("a blob with an invalid length was accepted")
	}
}

// The three baseline entries must come from the directory's current mode bits. Hard-coding them would change the owner permissions too
func TestBaseACLFromModeMirrorsPermissionBits(t *testing.T) {
	got := baseACLFromMode(0o750)
	want := []aclEntry{
		{tag: aclTagUserObj, perm: aclPermRWX, id: aclUndefinedID},
		{tag: aclTagGroupObj, perm: aclPermR | aclPermX, id: aclUndefinedID},
		{tag: aclTagOther, perm: 0, id: aclUndefinedID},
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("entry %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// With named entries there must be a MASK (otherwise setxattr returns EINVAL); once named entries are all removed,
// MASK must disappear too, or it would push GROUP_OBJ under the mask
func TestWithNamedUserMaintainsMask(t *testing.T) {
	base := baseACLFromMode(0o770)

	granted := withNamedUser(base, 20000, true)
	if !hasNamedEntry(granted) {
		t.Fatal("no named entry after granting")
	}
	var maskCount int
	for _, e := range granted {
		if e.tag == aclTagMask {
			maskCount++
		}
	}
	if maskCount != 1 {
		t.Errorf("MASK entry count after granting = %d, want 1", maskCount)
	}

	revoked := withNamedUser(granted, 20000, false)
	if hasNamedEntry(revoked) {
		t.Error("named entries remain after revoking")
	}
	for _, e := range revoked {
		if e.tag == aclTagMask {
			t.Error("named entries are all gone but MASK is still there")
		}
	}

	// Granting twice mustn't pile up two records for the same uid
	twice := withNamedUser(withNamedUser(base, 20000, true), 20000, true)
	var named int
	for _, e := range twice {
		if e.tag == aclTagUser && e.id == 20000 {
			named++
		}
	}
	if named != 1 {
		t.Errorf("entries for the same uid after granting twice = %d, want 1", named)
	}
}

// Through the real kernel: this file's binary layout was written by hand from the uapi, and only the kernel accepting it and reading it back
// proves it right. Skip rather than fail when the filesystem doesn't support ACLs: that's an environment difference
func TestSetUserDataAccessAgainstKernel(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0o1770); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	g := newACLGate(t, root)
	ctx := context.Background()

	if err := g.SetUserDataAccess(ctx, SubjectKernel(),
		SetUserDataAccessRequest{UID: 20000, Allowed: true}); err != nil {
		if errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EOPNOTSUPP) {
			t.Skipf("the filesystem doesn't support POSIX ACLs: %v", err)
		}
		t.Fatalf("grant: %v", err)
	}

	raw, err := getxattrAll(root, aclAccessXattr)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	entries, err := parseACL(raw)
	if err != nil {
		t.Fatalf("parse the read-back result: %v", err)
	}
	found := false
	for _, e := range entries {
		if e.tag == aclTagUser && e.id == 20000 && e.perm == aclPermRWX {
			found = true
		}
	}
	if !found {
		t.Fatalf("the ACL the kernel accepted has no u:20000:rwx: %+v", entries)
	}

	// After revoking, the xattr should disappear entirely, back to pure mode bits
	if err := g.SetUserDataAccess(ctx, SubjectKernel(),
		SetUserDataAccessRequest{UID: 20000, Allowed: false}); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	raw, err = getxattrAll(root, aclAccessXattr)
	if err != nil {
		t.Fatalf("read back after revoking: %v", err)
	}
	if len(raw) != 0 {
		t.Errorf("ACL still present after revoking: %d bytes", len(raw))
	}
}

// The UID range check can't be bypassed: once root or a distribution service is written into the user documents area's ACL,
// nothing would ever remove that record again
func TestSetUserDataAccessRejectsNonAppUID(t *testing.T) {
	g := newACLGate(t, t.TempDir())
	for _, uid := range []uint32{0, 1, 19999, 60000} {
		err := g.SetUserDataAccess(context.Background(), SubjectKernel(),
			SetUserDataAccessRequest{UID: uid, Allowed: true})
		if !errors.Is(err, ErrInvariantViolated) {
			t.Errorf("uid %d was accepted: %v", uid, err)
		}
	}
}
