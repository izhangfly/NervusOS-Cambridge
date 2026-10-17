//go:build linux

package simprocess

import (
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

type processIdentity struct {
	pid        int
	startTicks string
}

func configureProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
}

func managedSupported() bool { return true }

func identifyProcess(pid int) (processIdentity, error) {
	start, err := procStartTicks(pid)
	if err != nil {
		return processIdentity{}, err
	}
	return processIdentity{pid: pid, startTicks: start}, nil
}

func signalProcessGroup(identity processIdentity, force bool) error {
	current, err := procStartTicks(identity.pid)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return os.ErrProcessDone
		}
		return err
	}
	if current != identity.startTicks {
		return errors.New("simprocess: process identity changed")
	}
	signal := unix.SIGTERM
	if force {
		signal = unix.SIGKILL
	}
	if err := unix.Kill(-identity.pid, signal); err != nil {
		if errors.Is(err, unix.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	return nil
}

func procStartTicks(pid int) (string, error) {
	wire, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return "", err
	}
	line := string(wire)
	closeParen := strings.LastIndexByte(line, ')')
	if closeParen < 0 || closeParen+2 >= len(line) {
		return "", errors.New("simprocess: invalid proc stat")
	}
	// Fields after comm begin at stat field 3; starttime is field 22.
	fields := strings.Fields(line[closeParen+2:])
	if len(fields) <= 19 {
		return "", errors.New("simprocess: incomplete proc stat")
	}
	return fields[19], nil
}
