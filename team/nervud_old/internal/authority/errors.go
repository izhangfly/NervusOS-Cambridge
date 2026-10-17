package authority

import "errors"

var (
	// ErrPolicyDenied: the policy decision said no.
	//
	// There is no Policy step on the Authority path (the callers are all inside the TCB; see do() in authority.go),
	// so nothing produces this error yet. It is kept for the Policy seam marked in do().
	// internal/permission should reuse the same meaning when it denies an App request.
	ErrPolicyDenied = errors.New("authority: policy denied")

	// ErrInvariantViolated: a low-level invariant was broken.
	// No policy can waive these errors: even if the policy says yes, the answer is still no.
	ErrInvariantViolated = errors.New("authority: invariant violated")

	// ErrUnsupportedPlatform: this privileged operation is implemented on Linux only.
	//
	// Unlike the scheduler's allowDegrade, there is no fallback switch here: failing to set a scheduling priority
	// only degrades real-time behaviour, but a privileged operation that silently does nothing makes the caller believe the directory exists or the process stopped:
	// a dangerous false success. On development machines (Windows/macOS) this error means the assembly is wrong.
	ErrUnsupportedPlatform = errors.New("authority: operation requires linux")
)
