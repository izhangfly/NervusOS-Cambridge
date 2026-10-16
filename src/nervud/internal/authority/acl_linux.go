//go:build linux

// This file implements runtime grant and revocation of perm.storage.user: adding and removing a UID's entry in the
// POSIX ACL on the user documents directory.
//
// # Why an ACL rather than restarting the process
//
// Writable paths (ReadWritePaths) are burned into the mount namespace by systemd at spawn,
// and can't change once the process is up. So "runtime grant" used to mean stopping and restarting the whole package so the new process
// got the new namespace: the user flipped a switch and the app in use vanished and came back.
//
// An ACL is different: it's evaluated at open(2), takes effect immediately on running processes, and revocation is just as immediate.
// So the two gates can be separated by their own timescales:
//
//	mount gate (install time, static):    the manifest requested perm.storage.user and the install decision passed
//	                                      -> user-data goes into ReadWritePaths. Unchanged for the process's lifetime
//	access gate (run time, immediate):    does the user agree -> add/remove the u:<uid>:rwx entry in the ACL
//
// This also makes the line at the top of runtime.go, "an immediate-revocation ability Android doesn't have", true:
// before this, the "immediate" was achieved by killing processes.
//
// # The directory mode must be 01770
//
// An ACL only means something when the mode bits keep people out. 01777 gives other rwx, so any package that got
// the mount could write, whatever the ACL says. See preflight's declaration for UserDataRoot:
// the two must change together, or this file does nothing.
//
// # Only the access ACL, no default ACL
//
// A default ACL would make new files and subdirectories inherit these entries, which would let every authorised package
// write files other packages created, wider than today (creator owns + umask, others read only). This change is about
// "when a grant takes effect" and shouldn't change sharing semantics along the way, so only the access ACL is set: new files and subdirectories
// get exactly today's ownership and mode.
package authority

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"sort"

	"golang.org/x/sys/unix"
)

// The POSIX ACL xattr name and binary layout. The structure is in Linux's
// include/uapi/linux/posix_acl_xattr.h:
//
//	header { __le32 a_version }
//	entry  { __le16 e_tag; __le16 e_perm; __le32 e_id }
//
// The fields are __le*, independent of host byte order, so LittleEndian is used explicitly below.
const (
	aclAccessXattr = "system.posix_acl_access"
	aclVersion     = 2

	aclHeaderSize = 4
	aclEntrySize  = 8
)

// ACL entry tags. The values are fixed by the uapi and can't change
const (
	aclTagUserObj  uint16 = 0x01
	aclTagUser     uint16 = 0x02
	aclTagGroupObj uint16 = 0x04
	aclTagGroup    uint16 = 0x08
	aclTagMask     uint16 = 0x10
	aclTagOther    uint16 = 0x20
)

// aclUndefinedID is the id of entries with no subject, such as USER_OBJ / GROUP_OBJ / MASK / OTHER
const aclUndefinedID uint32 = 0xFFFFFFFF

// Permission bits
const (
	aclPermX uint16 = 1
	aclPermW uint16 = 2
	aclPermR uint16 = 4

	aclPermRWX = aclPermR | aclPermW | aclPermX
)

// aclEntry is one decoded ACL record
type aclEntry struct {
	tag  uint16
	perm uint16
	id   uint32
}

// parseACL decodes an access ACL. Empty input means "no ACL" and returns nil rather than an error:
// that's the normal state of most directories
func parseACL(raw []byte) ([]aclEntry, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	if len(raw) < aclHeaderSize || (len(raw)-aclHeaderSize)%aclEntrySize != 0 {
		return nil, fmt.Errorf("%w: acl blob has %d bytes", ErrInvariantViolated, len(raw))
	}
	if v := binary.LittleEndian.Uint32(raw[:aclHeaderSize]); v != aclVersion {
		return nil, fmt.Errorf("%w: acl version %d is not %d", ErrInvariantViolated, v, aclVersion)
	}
	n := (len(raw) - aclHeaderSize) / aclEntrySize
	out := make([]aclEntry, 0, n)
	for i := 0; i < n; i++ {
		b := raw[aclHeaderSize+i*aclEntrySize:]
		out = append(out, aclEntry{
			tag:  binary.LittleEndian.Uint16(b[0:2]),
			perm: binary.LittleEndian.Uint16(b[2:4]),
			id:   binary.LittleEndian.Uint32(b[4:8]),
		})
	}
	return out, nil
}

// marshalACL encodes in the order the kernel requires: USER_OBJ, USER..., GROUP_OBJ, GROUP...,
// MASK, OTHER; ascending id within the same tag. With the wrong order setxattr returns EINVAL
func marshalACL(entries []aclEntry) []byte {
	sorted := append([]aclEntry(nil), entries...)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].tag != sorted[j].tag {
			return sorted[i].tag < sorted[j].tag
		}
		return sorted[i].id < sorted[j].id
	})

	buf := make([]byte, aclHeaderSize+len(sorted)*aclEntrySize)
	binary.LittleEndian.PutUint32(buf[:aclHeaderSize], aclVersion)
	for i, e := range sorted {
		b := buf[aclHeaderSize+i*aclEntrySize:]
		binary.LittleEndian.PutUint16(b[0:2], e.tag)
		binary.LittleEndian.PutUint16(b[2:4], e.perm)
		binary.LittleEndian.PutUint32(b[4:8], e.id)
	}
	return buf
}

// baseACLFromMode synthesises the three required entries from the directory's current mode bits.
//
// It's needed when the directory has no ACL yet: POSIX requires exactly one each of USER_OBJ / GROUP_OBJ / OTHER,
// and on a directory without an ACL their values are the mode bits themselves. Hard-coding them would change the directory's
// owner permissions
func baseACLFromMode(mode uint32) []aclEntry {
	return []aclEntry{
		{tag: aclTagUserObj, perm: uint16((mode >> 6) & 7), id: aclUndefinedID},
		{tag: aclTagGroupObj, perm: uint16((mode >> 3) & 7), id: aclUndefinedID},
		{tag: aclTagOther, perm: uint16(mode & 7), id: aclUndefinedID},
	}
}

// withNamedUser adds or removes a named-user entry, maintaining MASK according to the result.
//
// MASK means "the upper limit for named entries and GROUP_OBJ", and is required whenever there's any named entry;
// without it setxattr returns EINVAL. Conversely, once every named entry is removed, MASK must go
// too, otherwise a leftover one would push GROUP_OBJ under the mask
func withNamedUser(entries []aclEntry, uid uint32, allow bool) []aclEntry {
	out := make([]aclEntry, 0, len(entries)+2)
	for _, e := range entries {
		// First remove the target uid and MASK; rebuild below as needed
		if e.tag == aclTagUser && e.id == uid {
			continue
		}
		if e.tag == aclTagMask {
			continue
		}
		out = append(out, e)
	}
	if allow {
		out = append(out, aclEntry{tag: aclTagUser, perm: aclPermRWX, id: uid})
	}

	// If named entries remain, MASK must be added back. Use rwx: the mask is an upper limit, not a grant,
	// and lowering it would also cut GROUP_OBJ, a side effect this operation mustn't have
	for _, e := range out {
		if e.tag == aclTagUser || e.tag == aclTagGroup {
			return append(out, aclEntry{tag: aclTagMask, perm: aclPermRWX, id: aclUndefinedID})
		}
	}
	return out
}

// hasNamedEntry reports whether this ACL still has named user/group entries
func hasNamedEntry(entries []aclEntry) bool {
	for _, e := range entries {
		if e.tag == aclTagUser || e.tag == aclTagGroup {
			return true
		}
	}
	return false
}

// osSetUserDataAccess is the Linux implementation of SetUserDataAccess.
//
// Read-modify-write: fetch the existing ACL (synthesising a baseline from the mode bits if there's none), add/remove the target UID, write it back.
// A full overwrite rather than incremental: a POSIX ACL xattr can only be replaced whole anyway
func (g *Gate) osSetUserDataAccess(_ context.Context, req SetUserDataAccessRequest) (struct{}, error) {
	path := g.inv.UserDataRoot

	var st unix.Stat_t
	if err := unix.Lstat(path, &st); err != nil {
		return struct{}{}, fmt.Errorf("stat %s: %w", path, err)
	}
	// Don't follow symlinks: the user documents area is a real directory guaranteed by preflight; reaching a symlink means
	// someone has swapped the ground, and changing the ACL then would change it somewhere else
	if st.Mode&unix.S_IFMT != unix.S_IFDIR {
		return struct{}{}, fmt.Errorf("%w: %s is not a directory", ErrInvariantViolated, path)
	}

	raw, err := getxattrAll(path, aclAccessXattr)
	if err != nil {
		return struct{}{}, err
	}
	current, err := parseACL(raw)
	if err != nil {
		return struct{}{}, err
	}
	if len(current) == 0 {
		current = baseACLFromMode(st.Mode & 0o777)
	}

	next := withNamedUser(current, req.UID, req.Allowed)

	// When no named entries remain, remove the xattr entirely and go back to pure mode bits.
	// Leaving an ACL with "only the three baseline entries" isn't wrong, but it makes getfacl's output
	// differ from a machine that was never granted anything, one more difference to explain when troubleshooting
	if !hasNamedEntry(next) {
		if rerr := unix.Removexattr(path, aclAccessXattr); rerr != nil &&
			!errors.Is(rerr, unix.ENODATA) && !errors.Is(rerr, unix.ENOTSUP) {
			return struct{}{}, fmt.Errorf("removexattr %s: %w", path, rerr)
		}
		return struct{}{}, nil
	}

	if serr := unix.Setxattr(path, aclAccessXattr, marshalACL(next), 0); serr != nil {
		return struct{}{}, fmt.Errorf("setxattr %s: %w", path, serr)
	}
	return struct{}{}, nil
}

// osReconcileUserDataAccess is the Linux implementation of ReconcileUserDataAccess: it replaces the named-user entries in the ACL
// as a whole with the given set.
//
// It exists because the ACL and the grant state can drift. Grant state is in _grants.json and the ACL on the file
// system, each persisted separately. If a package is uninstalled while nervud isn't running, its grant records are cleared
// but the ACL entries stay behind; when the UID is later reused by a new package, that package gets write access to the user documents area
// for free, without any UI ever having shown it.
//
// So the invariant is set as "the ACL is a projection of the grant state": reconcile fully once at startup, then
// ProjectRuntimePermission maintains it incrementally. Full replacement rather than entry-by-entry comparison: the former is naturally idempotent,
// and also clears any entry of unknown origin
func (g *Gate) osReconcileUserDataAccess(_ context.Context, req ReconcileUserDataAccessRequest) (struct{}, error) {
	path := g.inv.UserDataRoot

	var st unix.Stat_t
	if err := unix.Lstat(path, &st); err != nil {
		return struct{}{}, fmt.Errorf("stat %s: %w", path, err)
	}
	if st.Mode&unix.S_IFMT != unix.S_IFDIR {
		return struct{}{}, fmt.Errorf("%w: %s is not a directory", ErrInvariantViolated, path)
	}

	raw, err := getxattrAll(path, aclAccessXattr)
	if err != nil {
		return struct{}{}, err
	}
	current, err := parseACL(raw)
	if err != nil {
		return struct{}{}, err
	}
	if len(current) == 0 {
		current = baseACLFromMode(st.Mode & 0o777)
	}

	// Keep only the three baseline entries; drop every named entry and rebuild from the given set
	next := make([]aclEntry, 0, len(req.UIDs)+4)
	for _, e := range current {
		if e.tag == aclTagUser || e.tag == aclTagGroup || e.tag == aclTagMask {
			continue
		}
		next = append(next, e)
	}
	seen := make(map[uint32]struct{}, len(req.UIDs))
	for _, uid := range req.UIDs {
		if _, dup := seen[uid]; dup {
			continue
		}
		seen[uid] = struct{}{}
		next = append(next, aclEntry{tag: aclTagUser, perm: aclPermRWX, id: uid})
	}

	if !hasNamedEntry(next) {
		if rerr := unix.Removexattr(path, aclAccessXattr); rerr != nil &&
			!errors.Is(rerr, unix.ENODATA) && !errors.Is(rerr, unix.ENOTSUP) {
			return struct{}{}, fmt.Errorf("removexattr %s: %w", path, rerr)
		}
		return struct{}{}, nil
	}
	next = append(next, aclEntry{tag: aclTagMask, perm: aclPermRWX, id: aclUndefinedID})

	if serr := unix.Setxattr(path, aclAccessXattr, marshalACL(next), 0); serr != nil {
		return struct{}{}, fmt.Errorf("setxattr %s: %w", path, serr)
	}
	return struct{}{}, nil
}

// getxattrAll reads the full value of an xattr. A missing attribute returns nil: for ACLs,
// "not yet" is the normal state, not an error
func getxattrAll(path, attr string) ([]byte, error) {
	size, err := unix.Getxattr(path, attr, nil)
	if err != nil {
		if errors.Is(err, unix.ENODATA) || errors.Is(err, unix.ENOTSUP) {
			return nil, nil
		}
		return nil, fmt.Errorf("getxattr %s %s: %w", path, attr, err)
	}
	if size == 0 {
		return nil, nil
	}
	buf := make([]byte, size)
	n, err := unix.Getxattr(path, attr, buf)
	if err != nil {
		if errors.Is(err, unix.ENODATA) {
			return nil, nil
		}
		return nil, fmt.Errorf("getxattr %s %s: %w", path, attr, err)
	}
	return buf[:n], nil
}
