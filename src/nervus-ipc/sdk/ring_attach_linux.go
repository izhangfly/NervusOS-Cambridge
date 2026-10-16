//go:build linux

package sdk

import (
	"fmt"
	"net"
	"syscall"

	ipcv1 "github.com/nervus-os/nervus-ipc/protocol/ipcv1"
	"golang.org/x/sys/unix"
)

// SDK-side attachment for SHARED_MEMORY_RING: receive the fds, map the memory, build the Ring view.
//
// Linux only: memfd and SCM_RIGHTS have no cross-platform equivalent, and nervud only runs on
// Linux anyway. There's **no non-Linux fallback** here: a fake implementation that "compiles but fails on every call"
// would only turn "this needs Linux" from a compile-time fact into a run-time surprise.

// attachRing takes the two fds nervud sends and maps the ring.
//
// The order is fixed as memfd, eventfd (see transfer.proto). Receiving a number other than 2 is treated as a protocol
// violation: a mismatched count means the two sides' understanding of the ABI has diverged, and carrying on would only read the wrong memory.
func (c *TransferConn) attachRing(raw net.Conn, cfg *ipcv1.TransferRingConfig) error {
	geometry, err := ValidateRingConfig(cfg)
	if err != nil {
		return err
	}
	total, err := geometry.TotalBytes()
	if err != nil {
		return err
	}

	unixConn, ok := raw.(*net.UnixConn)
	if !ok {
		return fmt.Errorf("%w: shared memory ring requires a unix socket", ErrProtocol)
	}

	// Control message space for two fds. A little extra doesn't hurt; too little gets truncated by the kernel, and truncation shows up as
	// "one fd mysteriously missing", far harder to diagnose than a clear error
	oob := make([]byte, unix.CmsgSpace(2*4))
	payload := make([]byte, 1)
	_, oobn, _, _, err := unixConn.ReadMsgUnix(payload, oob)
	if err != nil {
		return fmt.Errorf("sdk: receive ring descriptors: %w", err)
	}

	fds, err := parseUnixRights(oob[:oobn])
	if err != nil {
		closeFDs(fds)
		return err
	}
	if len(fds) != 2 {
		closeFDs(fds)
		return fmt.Errorf("%w: expected exactly 2 ring descriptors, got %d",
			ErrProtocol, len(fds))
	}
	memFD, eventFD := fds[0], fds[1]

	// The mapping length comes from **the geometry sent over the control plane**, not the size from fstat: the latter is decided by the memfd,
	// which the peer can influence, and computing bounds from it would hand the bounds to untrusted input
	prot := unix.PROT_READ
	if c.role == ipcv1.TransferRole_TRANSFER_ROLE_PRODUCER ||
		c.role == ipcv1.TransferRole_TRANSFER_ROLE_PEER {
		prot |= unix.PROT_WRITE
	} else {
		// The consumer also writes consumer_cursor, so it still needs PROT_WRITE.
		// The branch is kept so "who can write what" stays explicit: the real isolation is in
		// Ring's role checks and the seqlock, not in page protection
		prot |= unix.PROT_WRITE
	}
	mem, err := unix.Mmap(memFD, 0, int(total), prot, unix.MAP_SHARED)
	if err != nil {
		closeFDs(fds)
		return fmt.Errorf("sdk: mmap ring: %w", err)
	}

	producer := c.role == ipcv1.TransferRole_TRANSFER_ROLE_PRODUCER ||
		c.role == ipcv1.TransferRole_TRANSFER_ROLE_PEER
	ring, err := NewRing(mem, geometry, producer)
	if err != nil {
		_ = unix.Munmap(mem)
		closeFDs(fds)
		return err
	}

	c.ring = ring
	c.ringMem = mem
	c.ringMemFD = memFD
	c.ringEventFD = eventFD
	return nil
}

// parseUnixRights takes the fds out of the control message.
func parseUnixRights(oob []byte) ([]int, error) {
	if len(oob) == 0 {
		return nil, fmt.Errorf("%w: no control message with ring descriptors", ErrProtocol)
	}
	msgs, err := unix.ParseSocketControlMessage(oob)
	if err != nil {
		return nil, fmt.Errorf("%w: parse ring control message: %v", ErrProtocol, err)
	}
	var out []int
	for _, msg := range msgs {
		if msg.Header.Level != unix.SOL_SOCKET || msg.Header.Type != unix.SCM_RIGHTS {
			continue
		}
		fds, err := unix.ParseUnixRights(&msg)
		if err != nil {
			closeFDs(out)
			return nil, fmt.Errorf("%w: parse ring rights: %v", ErrProtocol, err)
		}
		out = append(out, fds...)
	}
	return out, nil
}

func closeFDs(fds []int) {
	for _, fd := range fds {
		_ = syscall.Close(fd)
	}
}

// closeRing releases the mapping and both fds. Safe to call more than once.
func (c *TransferConn) closeRing() {
	if c.ringMem != nil {
		_ = unix.Munmap(c.ringMem)
		c.ringMem = nil
	}
	if c.ringMemFD > 0 {
		_ = syscall.Close(c.ringMemFD)
		c.ringMemFD = -1
	}
	if c.ringEventFD > 0 {
		_ = syscall.Close(c.ringEventFD)
		c.ringEventFD = -1
	}
	c.ring = nil
}

// notifyRing wakes the peer. Called by the producer after writing a frame.
func (c *TransferConn) notifyRing() error {
	if c.ringEventFD <= 0 {
		return nil
	}
	var one [8]byte
	one[7] = 1
	if _, err := unix.Write(c.ringEventFD, one[:]); err != nil &&
		err != unix.EAGAIN {
		return fmt.Errorf("sdk: notify ring peer: %w", err)
	}
	return nil
}

// waitRing blocks waiting for the peer's notification, or until timeoutMillis expires (negative means wait forever).
func (c *TransferConn) waitRing(timeoutMillis int) error {
	if c.ringEventFD <= 0 {
		return fmt.Errorf("%w: transfer has no ring event descriptor", ErrProtocol)
	}
	fds := []unix.PollFd{{Fd: int32(c.ringEventFD), Events: unix.POLLIN}}
	for {
		n, err := unix.Poll(fds, timeoutMillis)
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			return fmt.Errorf("sdk: wait ring: %w", err)
		}
		if n == 0 {
			return nil // timeout: the caller decides whether to retry or give up
		}
		var drain [8]byte
		_, _ = unix.Read(c.ringEventFD, drain[:])
		return nil
	}
}
