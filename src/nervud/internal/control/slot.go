// This file is the lease slot state machine: issue, renewal, freshness, release, revocation and the pre-dispatch re-check.
//
// The points where the motion epoch increments follow this table strictly, exactly once per boundary:
//
//	issue from NONE                         increments (issueLocked)
//	HUMAN pre-empts AI                      increments once (voiding the old lease doesn't increment; the following issue does)
//	release / timeout / disconnect / deadman lapse   increments (dropLocked)
//	renewing the same lease                 doesn't increment
//	Safety trip                             not incremented in this package (gate.Trip already did; RevokeAll only clears slots)
package control

import (
	"fmt"
	"time"

	"github.com/nervus-os/nervud/internal/identity"
	"github.com/nervus-os/nervud/internal/motiongate"
)

// Request is one request for control.
//
// Class is the controlling-party category passed in by higher-level policy. Today's IPC policy lets wire callers choose HUMAN/AI
// and audits that choice separately; this package infers no identity and only applies the set pre-emption matrix and lease timing.
type Request struct {
	Conn  ConnID
	Class Class
	// Resource must be a stable public handle resolved from the catalog by the layer above, after confirming access_mode=EXCLUSIVE_CONTROL.
	// control deliberately doesn't import catalog; it only checks the handle is non-empty and well-formed, and isolates by handle.
	Resource           string
	ResourceGeneration uint64
	Owner              identity.Caller

	// TTL/Deadman of 0 means use the Policy default; when non-zero they can only be stricter (shorter) than Policy,
	// and over the limit is refused rather than silently shortened (see ErrPolicyViolation for why)
	TTL     time.Duration
	Deadman time.Duration

	// RequestedTTL is the wire's preferred value: when greater than 0 it takes min(RequestedTTL, Policy TTL),
	// without refusing just because the client's preference exceeds the limit. It can't be set together with the strict TTL.
	RequestedTTL time.Duration
}

// Acquire requests control.
//
// Safety must be decided before HUMAN/AI, otherwise a usable lease could still be issued after latching
func (m *Module) Acquire(req Request) (Lease, error) {
	ttl, deadman, err := m.resolveRequest(req)
	if err != nil {
		m.recordDenied(req, err)
		return Lease{}, err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	// The observer notifications for Safety's revocations must come before the next lease is published. Otherwise, when the same conn/resource
	// quickly re-arms and regains control, the delayed RevokeControl would hit streams derived from the new lease.
	// drainRevoked is serialised with the Control Lane by its own consumer lock; this is still the ordinary control plane,
	// not RevokeAll's RT path.
	if m.revPending.Load() != 0 {
		m.drainRevoked()
	}

	// Refuse new leases after shutdown. Once Stop closes stopCh the expiry Lane exits, and if a lease could still be issued, no
	// Lane would clear its slot on expiry / revoke the epoch, so it could keep authorising motion through Check until the process exits.
	// The check happens under mu, ordered against Stop's final revocation: a Stop that closed stopCh first is bound to refuse a later
	// Acquire; a lease issued by an Acquire that got mu first is collected by the subsequent Stop's revocation
	if m.stopping() {
		m.recordDenied(req, ErrShuttingDown)
		return Lease{}, ErrShuttingDown
	}

	// Take now after acquiring mu: taking it outside the lock would let a late old call overwrite a newer deadline with an earlier time
	now := time.Now()

	// Never issue when Safety isn't NORMAL. While latched, control can only be recovered through OEM Recovery / re-arm,
	// not by requesting again (after a re-arm it still starts from NONE)
	if st, _ := m.gate.Snapshot(); st != motiongate.StateNormal {
		m.recordDenied(req, ErrSafetyLatched)
		return Lease{}, ErrSafetyLatched
	}

	slot := m.slotLocked(req.Resource)
	if slot == nil {
		m.recordDenied(req, ErrResourceCapacity)
		return Lease{}, ErrResourceCapacity
	}
	if old := slot.cur.Load(); old != nil {
		if old.ResourceGeneration != req.ResourceGeneration {
			// The public handle was republished with different semantics. Treat the
			// old lease as revoked before applying class/preemption policy to the
			// new definition.
			m.dropLocked(slot, old, actionRevoked, errResourceRevoked)
		} else if invalid := m.leaseState(slot, old, now); invalid != nil {
			// The held lease has already lapsed; the Control Lane just hasn't collected it yet. That's a
			// revocation boundary in itself, so increment once; the issue that follows increments again, and both are legitimate
			m.dropLocked(slot, old, actionFor(invalid), invalid)
		} else {
			switch {
			case old.Conn == req.Conn && old.Class == req.Class:
				// A repeated request from the same connection and class = an idempotent renewal, not a pre-emption, and doesn't increment the epoch.
				// SDK resends/reconnects shouldn't make the robot's epoch jitter for nothing
				return m.renewLocked(slot, old, now)

			case canPreempt(req.Class, old.Class):
				// The only legal pre-emption: HUMAN over AI. The old lease is voided on the spot but not incremented here:
				// the whole pre-emption may increment only once, and that once is left for the issue below
				//
				// RevokeAll can clear slots concurrently without taking mu, so this still needs a CAS. Only the path that really collects
				// old does the accounting and notifies observers; a failed CAS means the Safety path owns that
				// revocation boundary, and drainRevoked will notify later, so it mustn't be sent twice.
				if slot.cur.CompareAndSwap(old, nil) {
					m.notifyLeaseEnded(old)
					m.recordLease(actionPreempted, old, nil)
				}

			case old.Class == ClassHuman:
				// A person is remote-controlling. The layer above can ask the user whether to give way: the decision to give way
				// belongs to the person, not the requester, so this only refuses and does no automatic takeover
				m.recordDenied(req, ErrHeldByHuman)
				return Lease{}, ErrHeldByHuman

			default:
				m.recordDenied(req, ErrHeldByAI)
				return Lease{}, ErrHeldByAI
			}
		}
	}

	return m.issueLocked(slot, req, now, ttl, deadman)
}

// resolveRequest checks the request and combines the TTL/deadman with the Policy limits into the final values
func (m *Module) resolveRequest(req Request) (time.Duration, time.Duration, error) {
	if req.Conn == 0 {
		return 0, 0, fmt.Errorf("%w: connection id is zero", ErrInvalidRequest)
	}
	if !req.Class.Valid() {
		// Including a client trying to request NONE: NONE isn't a requestable category
		return 0, 0, fmt.Errorf("%w: controller class %d is not HUMAN or AI", ErrInvalidRequest, req.Class)
	}
	if req.Resource == "" {
		return 0, 0, fmt.Errorf("%w: %q", ErrUnknownResource, req.Resource)
	}
	if req.ResourceGeneration == 0 {
		return 0, 0, fmt.Errorf("%w: resource generation is zero", ErrInvalidRequest)
	}
	classPolicy := m.policy.forClass(req.Class)
	if req.RequestedTTL < 0 {
		return 0, 0, fmt.Errorf("%w: negative requested ttl", ErrInvalidRequest)
	}
	if req.RequestedTTL > 0 {
		if req.TTL != 0 {
			return 0, 0, fmt.Errorf("%w: ttl and requested ttl are mutually exclusive", ErrInvalidRequest)
		}
		ttl := min(req.RequestedTTL, classPolicy.TTL)
		deadman := req.Deadman
		if deadman == 0 && classPolicy.Deadman > ttl {
			deadman = ttl
		}
		return classPolicy.resolve(ttl, deadman)
	}
	return classPolicy.resolve(req.TTL, req.Deadman)
}

// issueLocked issues a new lease. The caller must hold mu
func (m *Module) issueLocked(slot *resourceSlot, req Request, now time.Time, ttl, deadman time.Duration) (Lease, error) {
	// "Increment only when Normal" must be atomic. If a Trip slipped in between reading State == Normal and BumpEpoch,
	// the old code would push an already latched Gate forward one more generation, so Safety's delivery path
	//  (deliverHalt reads Epoch) and tracking path (the Supervisor's haltEpoch) would each hold adjacent epochs,
	// the Provider's valid reports would be ignored as stale, and a false timeout escalation would follow. BumpEpochIfNormal
	// makes check + increment one CAS: latched means refuse, never advance.
	epoch, ok := m.advanceEpochLocked()
	if !ok {
		m.recordDenied(req, ErrSafetyLatched)
		return Lease{}, ErrSafetyLatched
	}

	// RevokeAll may publish the previous lease's end event only after Acquire's first drain.
	// The new lease must wait for the old event to finish, so a late RevokeControl can't hit new streams on the same conn/resource.
	if m.revPending.Load() != 0 {
		m.drainRevoked()
	}
	seq := m.revokeSeq.Load()
	if seq&1 != 0 || slot.revoked.Load() != nil {
		m.recordDenied(req, ErrSafetyLatched)
		return Lease{}, ErrSafetyLatched
	}

	l := Lease{
		ID:                 newID(),
		Conn:               req.Conn,
		Class:              req.Class,
		Resource:           req.Resource,
		ResourceGeneration: req.ResourceGeneration,
		Owner:              req.Owner,
		IssuedAt:           now,
		Deadline:           now.Add(ttl),
		TTL:                ttl,
		Epoch:              epoch,
		Deadman:            deadman,
	}
	// Set freshness before publishing: otherwise a Check right after publishing would read the stale
	// fresh left by the previous lease and wrongly judge the new lease's deadman as lapsed
	m.markFresh(slot, now)
	slot.cur.Store(&l)

	// A Safety trip doesn't take mu, so issuing and Trip can interleave. Re-check after publishing,
	// so a lease that was invalid at birth doesn't stay in the slot
	if st, ep := m.gate.Snapshot(); st != motiongate.StateNormal || ep != epoch ||
		m.revokeSeq.Load() != seq {
		slot.cur.CompareAndSwap(&l, nil)
		cause := ErrSafetyLatched
		if st == motiongate.StateNormal {
			cause = ErrStaleEpoch
		}
		m.recordDenied(req, cause)
		return Lease{}, cause
	}

	m.recordLease(actionGranted, &l, nil)
	return l, nil
}

// Renew renews: extends the deadline and refreshes freshness, without incrementing the epoch (the lease identity doesn't change)
func (m *Module) Renew(id ID, conn ConnID) (time.Time, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Take now after acquiring mu: taking it outside the lock would let a late old Renew overwrite a newer deadline with an earlier time
	now := time.Now()

	slot, l, err := m.lookup(id, conn, now)
	if err != nil {
		return time.Time{}, err
	}
	renewed, err := m.renewLocked(slot, l, now)
	if err != nil {
		return time.Time{}, err
	}
	return renewed.Deadline, nil
}

// renewLocked replaces the old lease whole with a new immutable value (ID unchanged). The caller must hold mu
func (m *Module) renewLocked(slot *resourceSlot, l *Lease, now time.Time) (Lease, error) {
	next := *l
	next.Deadline = now.Add(l.TTL)

	if !slot.cur.CompareAndSwap(l, &next) {
		// It was cleared by RevokeAll in the meantime (Safety tripped). No retry: there's no control any more
		return Lease{}, ErrSafetyLatched
	}
	// A heartbeat both extends and proves liveness, so renewing also refreshes the deadman freshness
	m.markFresh(slot, now)
	m.recordLease(actionRenewed, &next, nil)
	return next, nil
}

// Refresh only refreshes command freshness, without touching the deadline.
//
// Lock-free and allocation-free: every motion command calls it to prove the input is still fresh
func (m *Module) Refresh(id ID, conn ConnID) error {
	now := time.Now()
	slot, l, err := m.lookup(id, conn, now)
	if err != nil {
		return err
	}
	m.markFresh(slot, now)
	// A normal revocation can remove the lease between lookup and markFresh.
	// Re-check the identity so a stale command never reports a successful
	// refresh. A replacement lease is issued after now, and markFresh is an
	// atomic max, so this stale timestamp cannot extend the replacement's
	// deadman window.
	if !slotHolds(slot, l) {
		return ErrControlNotHeld
	}
	return nil
}

// Release gives up control voluntarily: revoke the lease, increment the epoch, back to NONE.
//
// It deliberately doesn't re-check that the lease is still valid: releasing a lease that just expired should succeed rather than error, because the caller's intent
// has been achieved. dropLocked's CAS guarantees no double increment
func (m *Module) Release(id ID, conn ConnID) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	slot, l := m.findLease(id, conn)
	if l == nil {
		return ErrControlNotHeld
	}
	m.dropLocked(slot, l, actionReleased, nil)
	return nil
}

// RevokeConn revokes the leases one connection holds on every Resource: called when the connection drops, the phone goes to the background,
// or a remote session lapses (the whole batch increments the epoch once).
func (m *Module) RevokeConn(conn ConnID) {
	if conn == 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	m.dropMatchingLocked(func(l *Lease) bool { return l.Conn == conn },
		actionRevoked, errConnectionClosed)
}

// RevokeResource invalidates the lease for one catalog handle. Catalog removal
// and same-handle definition replacement must call this before that handle can
// authorize a newly resolved route; otherwise an old lease could outlive the
// resource generation that justified it.
//
// The slot remains in the immutable directory. Handles are admitted only by
// the trusted catalog, and retaining the slot keeps RevokeAll's Safety read
// side lock-free with stable pointers.
func (m *Module) RevokeResource(resource string, generation uint64) {
	if resource == "" || generation == 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	slot := m.slot(resource)
	if slot == nil {
		return
	}
	if l := slot.cur.Load(); l != nil && l.ResourceGeneration == generation {
		m.dropLocked(slot, l, actionRevoked, errResourceRevoked)
	}
}

// RevokeByPackage revokes every actuator lease held under a Package: called by the layer above when the package is uninstalled or its motion-group
// permission is revoked by the user (the narrow interface permission.LeaseRevoker / pkgregistry.LeaseRevoker, which
// *Module satisfies implicitly). This lets a permission revocation take away the Package's actuator leases at once,
// instead of waiting for existing leases to expire naturally.
//
// Ownership is judged by the trusted identity Owner.PackageID recorded at issue (filled in after the IPC request pipeline's decision;
// see the Owner notes in lease.go), never by self-reported values. It walks every Resource slot and revokes all the leases the package
// currently holds at once.
//
// It follows doc.go's three hard rules, but this path isn't on the emergency stop / Safety Supervisor Lane: it's called by
// normal-priority uninstall/revocation flows, so like RevokeConn/Release it takes mu and goes through the normal change
// path (not RevokeAll's lock-free RT path; don't apply one to the other). Revocation goes through dropLocked:
//   - increments the epoch with BumpEpochIfNormal (not "read State, then Bump"; no extra increment when the Gate is latched)
//   - doesn't latch Safety (revocation only returns to NONE, no Trip)
//   - idempotent: an empty pkgID, no lease, or a current lease not belonging to the package all return nil directly (no lease to revoke
//     is exactly the end state the caller wants: the package no longer has control)
//
// Returning error satisfies the narrow interface signature above; the in-memory revocation path always returns nil today.
func (m *Module) RevokeByPackage(pkgID string) error {
	if pkgID == "" {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	m.dropMatchingLocked(func(l *Lease) bool { return l.Owner.PackageID == pkgID },
		actionRevoked, errPackageRevoked)
	return nil
}

// RevokeAll revokes every actuator lease, satisfying safety's LeaseRevoker.
//
// It's called synchronously by the Safety Supervisor Lane (FIFO 90) after latching, so it has three hard constraints:
//
//   - Doesn't take mu. Go's mutex has no priority inheritance, so waiting for a normal-priority lock holder is
//     priority inversion on FIFO 90. Only atomic operations here
//   - Doesn't increment the epoch. Each boundary may increment only once, and gate.Trip already did
//   - Zero heap allocations. Never new: scan the immutable slot snapshot and write the revoked lease into each slot's preallocated position
//
// Single producer (only the Supervisor goroutine calls this function). Audit and observer notifications are left to
// the Control Lane afterwards: they format strings / call external code, which mustn't happen on this synchronous path.
func (m *Module) RevokeAll(epoch uint64) {
	m.revokeSeq.Add(1) // odd: Acquire may not publish
	m.markSafetyFloor(epoch)
	index := m.slots.Load()
	for _, slot := range index.all {
		old := slot.cur.Swap(nil)
		if old == nil {
			continue
		}
		slot.revokedEpoch.Store(epoch)
		// Acquire may not publish while revoked is non-empty, so a failed CAS can only mean an internal invariant
		// has been broken. Keeping the old event is safer than overwriting it; normal operation never takes this branch.
		m.revPending.Add(1)
		if slot.revoked.CompareAndSwap(nil, old) {
			continue
		}
		m.revPending.Add(^uint64(0))
	}
	m.revokeSeq.Add(1) // even: all revocation events published
}

func (m *Module) markSafetyFloor(epoch uint64) {
	for {
		floor := m.safetyFloor.Load()
		if epoch <= floor || m.safetyFloor.CompareAndSwap(floor, epoch) {
			return
		}
	}
}

// Check is the pre-dispatch control re-check, returning the current motion epoch to put in the ExecutionContext.
//
// Lock-free and allocation-free: every motion command goes through it once (guarded by an AllocsPerRun test).
// It re-checks the preconditions that belong to this module: a valid lease + Safety NORMAL + epoch not expired +
// deadline not reached + deadman fresh. The sequence and a single command's deadline belong to the calling side
func (m *Module) Check(id ID, conn ConnID) (uint64, error) {
	_, l, err := m.lookup(id, conn, time.Now())
	if err != nil {
		return 0, err
	}
	return l.Epoch, nil
}

// CheckLease re-checks by lease handle and returns its immutable projection.
//
// The difference from CheckResource is the direction of the query: that one starts from "connection + resource" and answers
// "does this connection hold this resource right now"; this one starts from "the lease handle" and answers
// "is this handle still valid, and which resource does it hold".
//
// Creating an operation goes this way: the caller gives a lease handle, and the kernel must confirm it
// really covers the resource this operation will bind to. A valid handle pointing at another resource is
// "using the left arm's lease to move the right arm", and both are legitimate handles.
func (m *Module) CheckLease(id ID, conn ConnID) (LeaseProof, error) {
	_, l, err := m.lookup(id, conn, time.Now())
	if err != nil {
		return LeaseProof{}, err
	}
	return LeaseProof{
		ID:                 l.ID,
		Class:              l.Class,
		Resource:           l.Resource,
		ResourceGeneration: l.ResourceGeneration,
		Deadline:           l.Deadline,
		Epoch:              l.Epoch,
	}, nil
}

// CheckResource is the Method Gate query: it proves that conn currently owns a
// valid lease for exactly resource and returns the immutable fields IPC must
// project into ExecutionContext. The second lookup closes the race where the
// slot changes after the first atomic load.
func (m *Module) CheckResource(
	conn ConnID,
	resource string,
	resourceGeneration uint64,
) (LeaseProof, error) {
	slot := m.slot(resource)
	if slot == nil {
		return LeaseProof{}, ErrControlNotHeld
	}
	l := slot.cur.Load()
	if l == nil || l.Conn != conn || l.Resource != resource ||
		l.ResourceGeneration != resourceGeneration {
		return LeaseProof{}, ErrControlNotHeld
	}
	if err := m.validateCurrent(slot, l, time.Now()); err != nil {
		return LeaseProof{}, err
	}
	return LeaseProof{
		ID:                 l.ID,
		Class:              l.Class,
		Resource:           l.Resource,
		ResourceGeneration: l.ResourceGeneration,
		Deadline:           l.Deadline,
		Epoch:              l.Epoch,
	}, nil
}

// ControlSnapshot returns a consistent read-only control-plane snapshot of the legacy base.main. Other Resources use
// ControlSnapshotFor; the parameterless method is kept so health's existing narrow interface doesn't change.
func (m *Module) ControlSnapshot() Snapshot {
	return m.ControlSnapshotFor(ResourceBaseMain)
}

// ControlSnapshotFor returns a consistent read-only snapshot of the given Resource.
//
// The Gate is sampled before and after reading the slot, and it only returns when both match, so a Trip/re-arm in between can't
// report a stale NORMAL/Held. Lease.Epoch is the resource token and may be smaller than Snapshot.Epoch.
func (m *Module) ControlSnapshotFor(resource string) Snapshot {
	m.mu.Lock()
	defer m.mu.Unlock()

	for {
		seq := m.revokeSeq.Load()
		if seq&1 != 0 {
			continue
		}
		st, ep := m.gate.Snapshot()
		s := Snapshot{State: st, Epoch: ep}
		slot := m.slot(resource)
		var l *Lease
		if slot != nil {
			l = slot.cur.Load()
		}
		stAfter, epAfter := m.gate.Snapshot()
		if st != stAfter || ep != epAfter || m.revokeSeq.Load() != seq {
			continue
		}

		if st != motiongate.StateNormal {
			s.Source = SourceSafety
			return s
		}
		if l == nil || m.leaseStateAt(slot, l, st, ep, time.Now()) != nil {
			s.Source = SourceNone
			return s
		}
		s.Held = true
		s.Lease = *l
		if l.Class == ClassHuman {
			s.Source = SourceHuman
		} else {
			s.Source = SourceAI
		}
		return s
	}
}

// lookup takes the lease matching (id, conn) that is still valid right now.
//
// Lock-free and allocation-free: it only reads atomics, and every error returned is a preallocated sentinel
func (m *Module) lookup(id ID, conn ConnID, now time.Time) (*resourceSlot, *Lease, error) {
	slot, l := m.findLease(id, conn)
	if l == nil {
		return nil, nil, ErrControlNotHeld
	}
	if err := m.validateCurrent(slot, l, now); err != nil {
		return nil, nil, err
	}
	return slot, l, nil
}

// validateCurrent checks both lease state and slot membership. The second slot
// read closes the common race where a normal release/revoke removes l after
// findLease loaded it but before validation completed. A renewal replaces the
// immutable pointer while preserving lease identity, so membership compares
// the stable ID/connection/resource tuple rather than pointer identity.
func (m *Module) validateCurrent(slot *resourceSlot, l *Lease, now time.Time) error {
	if err := m.leaseState(slot, l, now); err != nil {
		return err
	}
	if !slotHolds(slot, l) {
		return ErrControlNotHeld
	}
	return nil
}

func slotHolds(slot *resourceSlot, lease *Lease) bool {
	if slot == nil || lease == nil {
		return false
	}
	current := slot.cur.Load()
	return current != nil && current.ID == lease.ID && current.Conn == lease.Conn &&
		current.Resource == lease.Resource
}

// leaseState re-checks whether a lease can still authorise motion right now; nil means valid. It reads the Gate once and hands over
// leaseStateAt.
func (m *Module) leaseState(slot *resourceSlot, l *Lease, now time.Time) error {
	st, ep := m.gate.Snapshot()
	return m.leaseStateAt(slot, l, st, ep, now)
}

// leaseStateAt re-checks lease validity with an already read (st, ep), so the caller can use one Gate read
// for other decisions too (see ControlSnapshot), avoiding torn state from reading the Gate twice in one piece of logic.
//
// The order is the priority: Safety > epoch > deadline > deadman. The most fundamental reason is reported first, so the caller can
// decide whether to request again or wait for a re-arm.
func (m *Module) leaseStateAt(slot *resourceSlot, l *Lease, st motiongate.State, _ uint64, now time.Time) error {
	if st != motiongate.StateNormal {
		return ErrSafetyLatched
	}
	// The motion epoch is minted by a global monotonic allocator, but ordinary boundaries only revoke the target Resource; other slots keep
	// their own older tokens. Only the Safety floor is a global revocation boundary across Resources.
	if l.Epoch <= m.safetyFloor.Load() {
		return ErrStaleEpoch
	}
	if now.After(l.Deadline) {
		return ErrLeaseExpired
	}
	if l.Deadman > 0 && now.Sub(m.base)-time.Duration(slot.fresh.Load()) > l.Deadman {
		return ErrDeadmanExpired
	}
	return nil
}

// markFresh records one fresh input in the given Resource slot, stored as nanoseconds relative to the monotonic baseline.
//
// It only ever increases (atomic max): under a monotonic clock later calls always have larger values, so this only stops a late old
// call writing freshness backwards. That's exactly the race to guard against: when the earlier of two concurrent Refreshes lands late,
// an unconditional Store would knock freshness from about 20ms back to 0, making a valid lease hit the deadman early. Allocation-free.
func (m *Module) markFresh(slot *resourceSlot, now time.Time) {
	v := int64(now.Sub(m.base))
	for {
		cur := slot.fresh.Load()
		if v <= cur {
			return
		}
		if slot.fresh.CompareAndSwap(cur, v) {
			return
		}
	}
}

// dropLocked takes down a lease and increments the epoch. The caller must hold mu.
//
// CAS rather than an unconditional Store(nil): a failed CAS means some other path already collected this lease, usually
// RevokeAll (safety's RT path, which doesn't take mu and so can interleave with this function). That boundary's epoch
// was already incremented by gate.Trip, and the Lane's revocation accounting path fills in the audit, so this must exit quietly, or it would
// record duplicate audit.
//
// It increments with BumpEpochIfNormal rather than an unconditional BumpEpoch: if the Gate is already latched by a Trip (and RevokeAll
// hasn't swapped this lease away yet, so the CAS still succeeds), an unconditional increment would push the latched Gate forward one more generation, putting
// Safety's delivery/tracking epochs out of step. When latched, that revocation boundary is already covered by the Trip's increment, so nothing is added here.
func (m *Module) dropLocked(slot *resourceSlot, l *Lease, action string, cause error) {
	if !slot.cur.CompareAndSwap(l, nil) {
		return
	}
	_, _ = m.advanceEpochLocked()
	m.notifyLeaseEnded(l)
	m.recordLease(action, l, cause)
}

// dropMatchingLocked atomically removes every matching slot and records this batch of ordinary revocations as one global motion
// epoch boundary. The caller must hold mu. Slots collected concurrently by Safety are notified by RevokeAll itself and not accounted twice.
func (m *Module) dropMatchingLocked(match func(*Lease) bool, action string, cause error) {
	index := m.slots.Load()
	dropped := make([]*Lease, 0)
	for _, slot := range index.all {
		l := slot.cur.Load()
		if l == nil || !match(l) || !slot.cur.CompareAndSwap(l, nil) {
			continue
		}
		dropped = append(dropped, l)
	}
	if len(dropped) == 0 {
		return
	}
	_, _ = m.advanceEpochLocked()
	for _, l := range dropped {
		m.notifyLeaseEnded(l)
		m.recordLease(action, l, cause)
	}
}

// advanceEpochLocked takes the next monotonic token from the global motion epoch allocator. Ordinary lease boundaries only
// change the target Resource and never silently rewrite other valid leases' tokens; revocation across Resources belongs to Safety alone.
func (m *Module) advanceEpochLocked() (uint64, bool) {
	return m.gate.BumpEpochIfNormal()
}

func (m *Module) findLease(id ID, conn ConnID) (*resourceSlot, *Lease) {
	for _, slot := range m.slots.Load().all {
		l := slot.cur.Load()
		if l != nil && l.ID == id && l.Conn == conn {
			return slot, l
		}
	}
	return nil, nil
}

func (m *Module) slot(resource string) *resourceSlot {
	return m.slots.Load().byResource[resource]
}

// slotLocked returns the existing slot, or publishes a new one copy-on-write under mu. Slots are never deleted, so
// the immutable snapshots RevokeAll reads, and the pointers in them, stay valid for the whole life of the process.
func (m *Module) slotLocked(resource string) *resourceSlot {
	current := m.slots.Load()
	if slot := current.byResource[resource]; slot != nil {
		return slot
	}
	if len(current.all) >= maxResourceSlots {
		return nil
	}
	slot := &resourceSlot{resource: resource}
	byResource := make(map[string]*resourceSlot, len(current.byResource)+1)
	for handle, existing := range current.byResource {
		byResource[handle] = existing
	}
	byResource[resource] = slot
	all := make([]*resourceSlot, len(current.all)+1)
	copy(all, current.all)
	all[len(current.all)] = slot
	m.slots.Store(&slotIndex{byResource: byResource, all: all})
	return slot
}

func (m *Module) current(resource string) *Lease {
	if slot := m.slot(resource); slot != nil {
		return slot.cur.Load()
	}
	return nil
}
