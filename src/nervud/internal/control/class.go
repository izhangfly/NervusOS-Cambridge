// This file defines the controlling-party category (Class), the effective control source (Source) and the single legal pre-emption matrix
package control

// Class is the category of the party holding a ControlLease (the wire's controller_class)
//
// There are only two, HUMAN and AI. NONE isn't a Class: it means there's no valid lease right now, which in the implementation
// is simply a nil lease, so clients can't request it. SAFETY isn't a Class either: it's an independent
// Gate/latch and can't share a lease implementation with HUMAN/AI.
//
// Where the Class comes from is decided by higher-level policy; today IPC lets the caller choose it and audits the choice, and this package infers no identity.
type Class uint8

const (
	// ClassUnspecified is the zero-value sentinel and never a legal request.
	//
	// The same reasoning as identity.TrustProfile and safety.ReasonCode's handling of 0: a missing value must
	// fail closed, never silently get some real category because the caller didn't set it
	ClassUnspecified Class = 0

	// ClassHuman means a person is closing the control loop through a joystick, keys or live remote control
	ClassHuman Class = 1

	// ClassAI: an Agent is observing, planning and executing autonomously. The criterion is who closes the execution control loop,
	// not which interface the command first came from: after the user says on their phone "go and look at the kitchen", the part the Agent
	// executes autonomously is AI, not HUMAN
	ClassAI Class = 2
)

// Valid reports whether c is a defined category
func (c Class) Valid() bool { return c == ClassHuman || c == ClassAI }

func (c Class) String() string {
	switch c {
	case ClassHuman:
		return "HUMAN"
	case ClassAI:
		return "AI"
	default:
		return "UNSPECIFIED"
	}
}

// Source is the four-valued projection of who is closing the actuator control loop right now.
//
// Unlike Class, this type's zero value is a legal conclusion: SourceNone means there's no valid lease right now,
// the normal state after boot, a voluntary release, a timeout, a disconnect or a re-arm, not a missing value.
//
// The two treat 0 in deliberately opposite ways: Class is something the requester passes in (missing must be refused), Source is
// a conclusion the kernel computes (NONE is a real conclusion)
type Source uint8

const (
	SourceNone   Source = 0
	SourceAI     Source = 1
	SourceHuman  Source = 2
	SourceSafety Source = 3
)

func (s Source) String() string {
	switch s {
	case SourceAI:
		return "AI"
	case SourceHuman:
		return "HUMAN"
	case SourceSafety:
		return "SAFETY"
	default:
		return "NONE"
	}
}

// canPreempt reports whether a requester of category want can pre-empt a lease held by category held.
//
// The only legal pre-emption is HUMAN pre-empting AI (HUMAN pre-empts AI, or one controller atomically
// replacing another increments the epoch only once). Nothing else can be pre-empted:
//
//	AI over HUMAN, never: having AI take control while a person is remote-controlling is unacceptable
//	same class over same class, always refused in v1 (fail closed). When two phones both want HUMAN,
//	 the latecomer gets a distinguishable ErrHeldByHuman, and the layer above asks the current holder to give way.
//	 The decision to give way belongs to the person, not the requester. Allowing same-level takeover first needs the product side to decide
//	 who is entitled to give way, which belongs to the remote session / pairing layer
//
// Note that a repeated request on the same connection doesn't come here: that's an idempotent renewal, not a pre-emption (see Module.Acquire)
func canPreempt(want, held Class) bool {
	return want == ClassHuman && held == ClassAI
}
