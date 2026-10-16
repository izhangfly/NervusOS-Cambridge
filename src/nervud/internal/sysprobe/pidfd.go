//go:build linux

// This file provides the two pidfd operations Component identity checks need from the
// kernel: SO_PEERPIDFD for a stable reference (pidfd) to the peer process, and reading the cgroup from a PID.
//
// Both meet sysprobe's purely-observational admission rule: read-only, no system state changes, no capabilities held.
// The interpretation "cgroup path -> systemd unit -> Component" is policy and lives in ipc, not here.
package sysprobe

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// PeerPIDFD uses SO_PEERPIDFD to get a pidfd for the UDS peer process (Linux 6.5+).
//
// Unlike SO_PEERCRED's bare PID, a pidfd is a stable reference to the peer process: as long as it's held, even if the peer's
// PID is recycled by the kernel for another process, the pidfd still points only at the original process (or clearly reports it has exited). That
// rules out, at the root, the race of checking an unrelated process after PID recycling. The caller closes the fd once done.
//
// Kernels older than 6.5 lack the option. The error returned is a sibling of errors.Is(err, ErrUnsupportedPlatform):
// a recognisable error, so the caller can fall back to SO_PEERCRED + a /proc owner re-check.
func PeerPIDFD(c *net.UnixConn) (int, error) {
	if c == nil {
		return -1, fmt.Errorf("sysprobe: peer pidfd: nil unix conn")
	}
	raw, err := c.SyscallConn()
	if err != nil {
		return -1, fmt.Errorf("sysprobe: peer pidfd: syscall conn: %w", err)
	}
	var (
		pidfd  int
		optErr error
	)
	if ctlErr := raw.Control(func(fd uintptr) {
		pidfd, optErr = unix.GetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_PEERPIDFD)
	}); ctlErr != nil {
		return -1, fmt.Errorf("sysprobe: peer pidfd: control fd: %w", ctlErr)
	}
	if optErr != nil {
		return -1, fmt.Errorf("sysprobe: peer pidfd: getsockopt SO_PEERPIDFD: %w", optErr)
	}
	return pidfd, nil
}

// PIDFromPIDFD reads the PID a pidfd currently points at (from the Pid: line of /proc/self/fdinfo/<fd>).
//
// Unlike the PID straight from SO_PEERCRED, this PID can't be reused as long as the pidfd is open and the process is alive,
// so reading /proc/<pid> with it is safe. If the process has exited, the Pid line is -1.
func PIDFromPIDFD(pidfd int) (int32, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/self/fdinfo/%d", pidfd))
	if err != nil {
		return 0, fmt.Errorf("sysprobe: read fdinfo: %w", err)
	}
	sc := bufio.NewScanner(strings.NewReader(string(data)))
	for sc.Scan() {
		line := sc.Text()
		if rest, ok := strings.CutPrefix(line, "Pid:"); ok {
			v, perr := strconv.ParseInt(strings.TrimSpace(rest), 10, 32)
			if perr != nil {
				return 0, fmt.Errorf("sysprobe: parse fdinfo Pid: %w", perr)
			}
			if v <= 0 {
				return 0, fmt.Errorf("sysprobe: pidfd refers to a dead process")
			}
			return int32(v), nil
		}
	}
	return 0, fmt.Errorf("sysprobe: fdinfo has no Pid line")
}

// PeerCgroupViaPIDFD reads the peer process's cgroup path through the stable SO_PEERPIDFD reference, managing the
// pidfd's lifetime inside the function (closed right after reading). That way ipc needn't import syscall/x-sys to
// close a bare fd (depguard only lets sysprobe touch syscall).
//
// On kernels older than 6.5 (no SO_PEERPIDFD) or if the peer has exited it returns an error, and the caller falls back to
// the SO_PEERCRED PID + ProcOwnerUID re-check path.
func PeerCgroupViaPIDFD(c *net.UnixConn) (string, error) {
	pidfd, err := PeerPIDFD(c)
	if err != nil {
		return "", err
	}
	defer func() { _ = unix.Close(pidfd) }()
	pid, err := PIDFromPIDFD(pidfd)
	if err != nil {
		return "", err
	}
	return CgroupPath(pid)
}

// CgroupPath reads the cgroup v2 (unified) line from /proc/<pid>/cgroup and returns its path.
//
// A cgroup v2 line looks like "0::/system.slice/nervus-<pkg>-<comp>.service". It returns the path after
// "0::". With no unified line (a pure v1 system) it returns an error; the nervud target image uses v2.
func CgroupPath(pid int32) (string, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cgroup", pid))
	if err != nil {
		return "", fmt.Errorf("sysprobe: read cgroup: %w", err)
	}
	sc := bufio.NewScanner(strings.NewReader(string(data)))
	for sc.Scan() {
		line := sc.Text()
		// The unified line: hierarchy ID 0, empty controller -> "0::<path>"
		if rest, ok := strings.CutPrefix(line, "0::"); ok {
			return rest, nil
		}
	}
	return "", fmt.Errorf("sysprobe: no cgroup v2 unified line for pid %d", pid)
}

// ProcOwnerUID reads the owner UID (real UID) of /proc/<pid>. The SO_PEERCRED fallback path uses it for a
// second check: that the owner of /proc/<pid> still equals the SO_PEERCRED UID (the fallback).
func ProcOwnerUID(pid int32) (uint32, error) {
	var st unix.Stat_t
	if err := unix.Stat(fmt.Sprintf("/proc/%d", pid), &st); err != nil {
		return 0, fmt.Errorf("sysprobe: stat /proc/%d: %w", pid, err)
	}
	return st.Uid, nil
}
