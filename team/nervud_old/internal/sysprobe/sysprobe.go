// See doc.go for the package description and admission rules.
//
// sysprobe is a leaf package: it imports no sibling module (ipc/identity import it, not the other way round).
// Dependency arrows always point at sysprobe, so an import cycle is structurally impossible.
package sysprobe

import "errors"

// Ucred is the kernel-trusted credentials of a UDS peer, the Linux struct ucred.
//
// These three values come from the kernel, not from any byte the peer sent: a client cannot forge them.
// They are the physical basis of architecture sec 10.2's "nervud verifies claims rather than believing them": a client may
// state its Component ID in Hello, but that is only a lead to check; the UID is what the identity decision rests on.
//
// The field types follow Linux's struct ucred (pid_t signed, uid_t/gid_t unsigned) instead of
// a more convenient int: the UID lines up directly with the uint32 parameter of authority.Invariants.CheckUID,
// and one conversion fewer is one fewer chance of a sign error.
type Ucred struct {
	PID int32
	UID uint32
	GID uint32
}

var ErrUnsupportedPlatform = errors.New("sysprobe: operation requires linux")
