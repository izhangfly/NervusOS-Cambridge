//go:build !linux

package simprocess

import (
	"errors"
	"os/exec"
)

type processIdentity struct {
	pid int
}

func configureProcessGroup(*exec.Cmd) {}

func managedSupported() bool { return false }

func identifyProcess(int) (processIdentity, error) {
	return processIdentity{}, errors.New("simprocess: managed mode requires Linux")
}

func signalProcessGroup(processIdentity, bool) error {
	return errors.New("simprocess: managed mode requires Linux")
}
