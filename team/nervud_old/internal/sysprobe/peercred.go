//go:build linux

// This file is the third place in the whole repository allowed to touch Linux syscall interfaces directly
// (the other two: ops.go in internal/authority and sched.go in internal/scheduler).
// The depguard rules in .golangci.yml allow golang.org/x/sys/unix here for that reason.
//
// Note that this package is allowed syscall / golang.org/x/sys only, never os/exec:
// starting a process is an exercise of power and can only ever go through authority.
package sysprobe

import (
	"fmt"
	"net"

	"golang.org/x/sys/unix"
)

// PeerCred reads the kernel-trusted credentials (PID / UID / GID) of a UDS peer.
//
// What SO_PEERCRED means
//
// The credentials are a snapshot the kernel takes at the moment the peer calls connect(2), not something collected
// on each read. That is exactly the property we want: whatever the peer does after connecting (setuid, exec into something else, being injected),
// the identity read here does not change, so it is immune by nature to identity swaps after the handshake.
//
// The price is that the PID goes stale: once the peer process exits, the kernel may recycle its PID for an unrelated
// new process. So the PID is only a lead to be checked: architecture sec 10.2 cross-checks the Component ID using PID + cgroup + the launch
// record, and that check must happen as soon as possible after the connection is made. Never
// take this PID to /proc long after the connection was established and trust what comes back.
//
// The UID is what identity decisions rest on: it is stable, unaffected by PID recycling and, per architecture sec 9, maps one-to-one
// to a Package. A caller holding the UID should first use authority.Invariants.CheckUID to confirm it is in
// the App range, then look it up in the Package Registry.
//
// # Concurrency
//
// Stateless, and changes no property of c, so it can be called concurrently from any goroutine.
func PeerCred(c *net.UnixConn) (Ucred, error) {
	if c == nil {
		return Ucred{}, fmt.Errorf("sysprobe: peer cred: nil unix conn")
	}

	// Use SyscallConn().Control, not c.File():
	//
	// File() dups the fd and switches the original connection to blocking mode; from then on the netpoller no longer manages
	// the connection, and SetReadDeadline / SetWriteDeadline stop working. For the control plane that is fatal:
	// sec 10.11 requires a finite deadline on every Frame read and write, and slowloris protection depends entirely on it.
	// A function that only reads the peer's identity quietly breaking the whole connection's timeouts is the hardest kind
	// of bug to find; peercred_linux_test.go has a regression lock watching exactly this.
	//
	// Control only borrows the fd for the duration of the callback and guarantees it is not closed or reused before the callback returns
	raw, err := c.SyscallConn()
	if err != nil {
		return Ucred{}, fmt.Errorf("sysprobe: peer cred: syscall conn: %w", err)
	}

	var (
		cred    *unix.Ucred
		credErr error
	)
	// The two error layers must be checked separately: Control itself failing (fd already closed, etc.) and getsockopt failing inside the callback
	// are different things; checking only one treats the other as success and returns a zero Ucred,
	// which amounts to handing UID 0 to the layer above.
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
