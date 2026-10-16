package authority

import "errors"

var (
	// ErrPolicyDenied: refused by a policy decision.
	//
	// There's no Policy step on the Authority path (callers are in the same TCB; see the notes on do in authority.go),
	// so this error has no producer yet; it's kept for the Policy seam marked in do.
	// internal/permission should reuse the same meaning when refusing App requests.
	ErrPolicyDenied = errors.New("authority: policy denied")

	// ErrInvariantViolated: a low-level invariant was violated.
	// Errors of this kind can't be exempted by any policy: even if policy says yes, it's refused
	ErrInvariantViolated = errors.New("authority: invariant violated")

	// ErrUnsupportedPlatform: this privileged operation is only implemented on Linux.
	//
	// Unlike scheduler's allowDegrade, there's no degrade switch here: failing to set scheduling priority
	// only degrades real-time behaviour, while a privileged operation silently becoming a no-op would make upper layers think a directory was created or a process stopped,
	// a dangerous false success. Hitting this error on a development machine (Windows/macOS) should be treated as a wiring error
	ErrUnsupportedPlatform = errors.New("authority: operation requires linux")
)

// ErrAlreadyExists: the target already exists.
//
// A sentinel of its own rather than having callers check unix.EEXIST: that would drag x/sys/unix into
// business packages such as pkgregistry, while .golangci.yml's depguard explicitly allows only this package to touch it.
// "Already exists" is a normal result for idempotent callers, and they need to recognise it.
var ErrAlreadyExists = errors.New("authority: target already exists")
