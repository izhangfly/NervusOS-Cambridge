//go:build linux

// This file is the third place in the repository allowed to touch the Linux syscall interface directly
//
//	(the other two: internal/authority's ops.go and internal/scheduler's sched.go)
//
// The depguard rule in .golangci.yml lets golang.org/x/sys/unix through on that basis.
//
// Note this package is only allowed syscall / golang.org/x/sys, not os/exec:
// starting a process is an exercise of power and must always go through authority.
package sysprobe

import (
	"fmt"
	"net"

	"golang.org/x/sys/unix"
)

// PeerCred reads the kernel-trusted credentials (PID / UID / GID) of the UDS peer.
//
// SO_PEERCRED semantics
//
// The credentials are snapshotted by the kernel at the moment the peer calls connect(2), not collected
// live on every read. That's the property we want: if the peer later calls setuid, re-skins itself with exec or gets injected,
// the identity read here doesn't change, so it's naturally immune to post-handshake re-skinning races.
//
// The cost is that the PID can go stale: after the peer exits, the kernel may recycle its PID for an unrelated
// new process. So the PID is only a lead to be checked; the Component ID must be cross-checked against the PID, the cgroup and the launch
// record, and the check must be done as soon as possible after the connection is set up. Never
// take this PID to /proc long after the connection was made and trust the answer.
//
// The UID is the basis for identity decisions: it's stable, unaffected by PID recycling, and maps one to one to a Package.
// After getting the UID, the caller should first confirm with authority.Invariants.CheckUID that it's
// in the App range, then look it up in the Package Registry.
//
// # Concurrency
//
// Stateless and doesn't modify any attribute of c, so it can be called concurrently on any goroutine.
func PeerCred(c *net.UnixConn) (Ucred, error) {
	if c == nil {
		return Ucred{}, fmt.Errorf("sysprobe: peer cred: nil unix conn")
	}

	// Use SyscallConn.Control rather than c.File:
	//
	// File dups the fd and switches the original connection to blocking mode, after which the netpoller stops managing
	// the connection and SetReadDeadline / SetWriteDeadline stop working. For the control plane that's fatal:
	// sec 10.11 requires a finite deadline on every Frame read and write, and slowloris protection depends entirely on it.
	// A function that only reads the peer's identity quietly killing the whole connection's timeouts is the hardest
	// kind of bug to find; peercred_linux_test.go has a regression lock watching for exactly this.
	//
	// Control only borrows the fd during the callback and guarantees it isn't closed or reused before the callback returns.
	raw, err := c.SyscallConn()
	if err != nil {
		return Ucred{}, fmt.Errorf("sysprobe: peer cred: syscall conn: %w", err)
	}

	var (
		cred    *unix.Ucred
		credErr error
	)
	// The two layers of error must be checked separately: Control itself failing (fd already closed, etc.) and getsockopt
	// failing inside the callback are different things. Checking only one treats the other as success and returns a zero Ucred,
	// which hands UID 0 to the layer above.
	if ctlErr := raw.Control(func(fd uintptr) {
		cred, credErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); ctlErr != nil {
		return Ucred{}, fmt.Errorf("sysprobe: peer cred: control fd: %w", ctlErr)
	}
	if credErr != nil {
		return Ucred{}, fmt.Errorf("sysprobe: peer cred: getsockopt SO_PEERCRED: %w", credErr)
	}

	return Ucred{PID: cred.Pid, UID: cred.Uid, GID: cred.Gid}, nil
}
