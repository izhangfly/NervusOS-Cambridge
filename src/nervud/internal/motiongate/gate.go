package motiongate

import "sync/atomic"

const (
	epochBits  = 56
	epochMask  = (uint64(1) << epochBits) - 1 // low 56 bits
	stateShift = epochBits                    // State is in the high 8 bits
)

// Gate packs the high-8-bit State and the low-56-bit epoch into one atomic value,
// so callers can't observe a state and generation that don't belong to the same moment.
//
// The zero-value Gate isn't usable (it decodes to StateInvalid/epoch 0); construct it with New. Fields are private
// and only read and written atomically through methods; never touch word directly.
type Gate struct {
	word atomic.Uint64
}

// New constructs a Gate in Normal with epoch=1. The epoch is never zero.
func New() *Gate {
	g := &Gate{}
	g.word.Store(pack(StateNormal, 1))
	return g
}

func pack(s State, epoch uint64) uint64 {
	return uint64(s)<<stateShift | epoch&epochMask
}

func unpack(word uint64) (State, uint64) {
	return State(word >> stateShift), word & epochMask
}

// incEpoch increments the epoch, keeping it within 56 bits and never landing on 0 (0 is an invalid generation).
//
// A 56-bit generation can't run out in practice (more than two thousand years even at one increment per microsecond); should it ever wrap,
// 0 is skipped explicitly, keeping the fail-closed invariant that the epoch is never zero.
func incEpoch(e uint64) uint64 {
	ne := (e + 1) & epochMask
	if ne == 0 {
		ne = 1
	}
	return ne
}

// Snapshot returns a consistent (state, epoch) with one atomic Load. Adapters/dispatch use it when re-checking motion
// preconditions, so they can't read the two fields in a torn state. Zero heap allocations.
func (g *Gate) Snapshot() (State, uint64) { return unpack(g.word.Load()) }

// State atomically reads the current state.
func (g *Gate) State() State { s, _ := unpack(g.word.Load()); return s }

// Epoch atomically reads the current motion epoch.
func (g *Gate) Epoch() uint64 { _, e := unpack(g.word.Load()); return e }

// Trip triggers Safety:
//
//   - currently not SafetyLatched -> set SafetyLatched and increment the epoch, returning true;
//   - currently SafetyLatched -> change nothing and return false (idempotent, no epoch churn).
//
// Triggering from OEMRecovery / RearmRequired also re-latches and increments the epoch, voiding leftover commands from the recovery
// phase (per the table: a Safety trigger increments immediately). The latch takes effect the moment this call returns,
// independent of any Lane scheduling. Lock-free, zero heap allocations, so it can be called directly on the Safety trigger
// hot path.
func (g *Gate) Trip() bool {
	for {
		old := g.word.Load()
		s, e := unpack(old)
		if s == StateSafetyLatched {
			return false
		}
		if g.word.CompareAndSwap(old, pack(StateSafetyLatched, incEpoch(e))) {
			return true
		}
	}
}

// BumpEpoch only increments the epoch, keeping the current State. In Normal, control uses the incremented value to allocate a new token
// for one Resource's lease boundary; other Resources' valid tokens don't need to equal the
// latest value here. Only tokens produced by Trip form a global revocation floor across Resources.
// Lock-free, zero heap allocations.
func (g *Gate) BumpEpoch() {
	for {
		old := g.word.Load()
		s, e := unpack(old)
		if g.word.CompareAndSwap(old, pack(s, incEpoch(e))) {
			return
		}
	}
}

// BumpEpochIfNormal increments the epoch only if the current state is Normal, returning the new epoch and whether it succeeded;
// when not Normal it changes nothing and returns the current epoch and false.
//
// It exists because the check + increment must be atomic. Reading State first and then calling BumpEpoch leaves
// a fatal window: a Trip squeezing in between the two steps would push an already-latched Gate forward one generation. Safety's
// stop delivery path and its state machine tracking path would then hold two adjacent epochs, and Provider reports would be judged stale
// and dropped, which then falsely triggers timeout escalation.
//
// Who uses it: control's lease lifecycle increments (issue / release / expire / disconnect). Safety's own increment goes through
// Trip, which must take effect unconditionally and can't be blocked by this method's precondition. Lock-free, zero heap allocations.
func (g *Gate) BumpEpochIfNormal() (uint64, bool) {
	for {
		old := g.word.Load()
		s, e := unpack(old)
		if s != StateNormal {
			return e, false
		}
		ne := incEpoch(e)
		if g.word.CompareAndSwap(old, pack(s, ne)) {
			return ne, true
		}
	}
}

// BeginRecovery: SafetyLatched -> OEMRecovery (epoch unchanged). Returns false from any other state.
// Entering the OEM recovery phase is still under safety semantics and doesn't release the latch.
func (g *Gate) BeginRecovery() bool { return g.cas1(StateSafetyLatched, StateOEMRecovery, false) }

// RequireRearm: SafetyLatched or OEMRecovery -> RearmRequired (epoch unchanged).
// Means an explicit re-arm is needed to get back to Normal. Returns false from any other state.
func (g *Gate) RequireRearm() bool {
	for {
		old := g.word.Load()
		s, e := unpack(old)
		if s != StateSafetyLatched && s != StateOEMRecovery {
			return false
		}
		if g.word.CompareAndSwap(old, pack(StateRearmRequired, e)) {
			return true
		}
	}
}

// Rearm: RearmRequired -> Normal and increment the epoch. After re-arm it starts from NONE,
// and the increment voids leftover commands from the recovery phase. Returns false from any other state.
func (g *Gate) Rearm() bool { return g.cas1(StateRearmRequired, StateNormal, true) }

// cas1 is a CAS transition from a single source state: only if the current state == from does it move to to (incrementing the epoch too when bump is set),
// returning true on success; if a concurrent Trip rewrote it in the meantime so the source state doesn't match, it returns false and the caller decides again
// based on the real current state.
func (g *Gate) cas1(from, to State, bump bool) bool {
	for {
		old := g.word.Load()
		s, e := unpack(old)
		if s != from {
			return false
		}
		ne := e
		if bump {
			ne = incEpoch(e)
		}
		if g.word.CompareAndSwap(old, pack(to, ne)) {
			return true
		}
	}
}
