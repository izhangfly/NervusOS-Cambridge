//go:build linux

// sysprobe is a leaf package: it imports no sibling module (ipc/identity import it, not the other way round).
// Dependency arrows always point at sysprobe, so an import cycle is structurally impossible.
package sysprobe

// Ucred is the kernel-trusted credentials of the UDS peer, matching Linux's struct ucred.
//
// These three values come from the kernel, not from any bytes the peer sends, so the client can't forge them.
// nervud must verify identity claims instead of trusting bytes supplied by the peer. A client
// may report a Component ID in Hello, but that value is only a lookup hint; UID remains the
// authority used for identity decisions.
//
// Field types follow Linux struct ucred (pid_t signed, uid_t/gid_t unsigned), with no conversion
// to int for convenience: the UID lines up directly with the uint32 parameter of authority.Invariants.CheckUID,
// and one less type conversion is one less chance of a sign error.
type Ucred struct {
	PID int32
	UID uint32
	GID uint32
}
