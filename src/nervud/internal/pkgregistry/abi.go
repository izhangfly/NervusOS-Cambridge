// This file is install-time host ABI matching: do the package's declared
// supported_abis include the current device's ABI token? A mismatch refuses the install; containers can't fix
// a CPU instruction set / ABI mismatch (an Agent decision).
//
// v1 simplification: only a coarse token mapped from runtime.GOARCH is matched as a set. Finer compatibility boundaries such as endianness, libc,
// the ARM float ABI, minimum ISA and the C++ runtime
// belong to v2's multi-architecture artefact selection and aren't covered here; but the token vocabulary itself (refusing Android
// NDK names and bare CPU names) is already strictly enforced in manifest.validate
package pkgregistry

import (
	"errors"
	"fmt"
	"runtime"
)

// ErrABIMismatch: the package's declared supported_abis don't include the current host's ABI token
var ErrABIMismatch = errors.New("pkgregistry: package does not support this host ABI")

// hostABIToken returns the current device's canonical NSOS ABI token.
//
// An empty string means this platform isn't in the v1 supported set (such as Windows/darwin development machines); then
// checkHostABI lets it through, so non-Linux development environments don't judge every install test as an ABI mismatch.
// On real devices GOARCH always falls into one of the three below
func hostABIToken() string {
	switch runtime.GOARCH {
	case "arm64":
		return ABILinuxArm64
	case "arm":
		return ABILinuxArmv7
	case "amd64":
		return ABILinuxX86_64
	default:
		return ""
	}
}

// checkHostABI checks that supported includes the current host's ABI token
func checkHostABI(supported []string) error {
	host := hostABIToken()
	if host == "" {
		return nil // not a target platform (development machine): no host matching
	}
	for _, abi := range supported {
		if abi == host {
			return nil
		}
	}
	return fmt.Errorf("%w: host %q, package supports %v", ErrABIMismatch, host, supported)
}
