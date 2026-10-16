// This file is the leases' time budget. HUMAN and AI get separate values because their failure modes are fundamentally different:
// people let go, lose their network and walk away; Agents don't let go but can get stuck in a model or the network
package control

import (
	"fmt"
	"time"
)

// ClassPolicy is the lease budget for one category of controlling party
type ClassPolicy struct {
	// TTL is the upper limit of validity given at issue and at each renewal. The requester may ask for less, never more
	TTL time.Duration

	// Deadman is the upper limit of the command freshness window. Again only shorter, never longer: longer = weaker.
	//
	// When it's 0 and RequireDeadman is false, this category doesn't require a deadman by default
	Deadman time.Duration

	// When RequireDeadman is true, this category's leases must carry a deadman, and the requester can't ask for 0
	RequireDeadman bool
}

// Policy is this module's whole time budget
type Policy struct {
	Human ClassPolicy
	AI    ClassPolicy
}

// DefaultPolicy returns a conservative set of fail-closed placeholder defaults.
//
// HUMAN: TTL 2s + deadman 300ms + deadman required. Phone remote control renews through input and heartbeats,
// and once the person lets go or the network drops, control must be lost within a few hundred milliseconds, back to NONE.
//
// AI: TTL 1s, no deadman required. An Agent is a local process and doesn't let go, so a deadman means little;
// but it can get stuck in model inference, cloud requests or deadlocks, so a shorter TTL forces it to renew explicitly to prove it's alive.
//
// Handled the same way as safety.DefaultContract: these numbers are conservative placeholders, not platform promises.
// Real hardware must override them from measurements of the remote-control link (round-trip latency, jitter, packet loss)
func DefaultPolicy() Policy {
	return Policy{
		Human: ClassPolicy{TTL: 2 * time.Second, Deadman: 300 * time.Millisecond, RequireDeadman: true},
		AI:    ClassPolicy{TTL: 1 * time.Second},
	}
}

// Validate does structural checks: TTL must be positive; categories requiring a deadman must give a positive deadman;
// the deadman must not be longer than the TTL (otherwise the TTL expires first and the deadman never fires, as if not configured)
func (p Policy) Validate() error {
	if err := p.Human.validate("human"); err != nil {
		return err
	}
	return p.AI.validate("ai")
}

func (c ClassPolicy) validate(name string) error {
	if c.TTL <= 0 {
		return fmt.Errorf("control: %s ttl must be positive", name)
	}
	if c.Deadman < 0 {
		return fmt.Errorf("control: %s deadman must not be negative", name)
	}
	if c.RequireDeadman && c.Deadman <= 0 {
		return fmt.Errorf("control: %s requires deadman but none is configured", name)
	}
	if c.Deadman > c.TTL {
		return fmt.Errorf("control: %s deadman must not exceed ttl", name)
	}
	return nil
}

// forClass takes one category's budget. Class was already confirmed legal when the Request was checked, so there are only two branches
func (p Policy) forClass(c Class) ClassPolicy {
	if c == ClassHuman {
		return p.Human
	}
	return p.AI
}

// resolve combines the TTL/deadman the requester gave with the Policy limits into the final values.
//
// Fixed semantics: 0 = use the Policy default; non-zero = the requester's choice, but only stricter (shorter); over the limit is refused.
// "Only stricter" lets the SDK shorten the window on a weak network, while the system's safety floor can never be loosened
func (c ClassPolicy) resolve(ttl, deadman time.Duration) (time.Duration, time.Duration, error) {
	if ttl < 0 || deadman < 0 {
		return 0, 0, fmt.Errorf("%w: negative duration", ErrInvalidRequest)
	}
	if ttl == 0 {
		ttl = c.TTL
	}
	if ttl > c.TTL {
		return 0, 0, fmt.Errorf("%w: ttl %s exceeds limit %s", ErrPolicyViolation, ttl, c.TTL)
	}

	if deadman == 0 {
		deadman = c.Deadman
	}
	if c.Deadman > 0 && deadman > c.Deadman {
		return 0, 0, fmt.Errorf("%w: deadman %s exceeds limit %s", ErrPolicyViolation, deadman, c.Deadman)
	}
	if c.RequireDeadman && deadman <= 0 {
		return 0, 0, fmt.Errorf("%w: this controller class requires a deadman", ErrPolicyViolation)
	}
	if deadman > ttl {
		return 0, 0, fmt.Errorf("%w: deadman %s exceeds ttl %s", ErrPolicyViolation, deadman, ttl)
	}
	return ttl, deadman, nil
}
