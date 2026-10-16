// This file plugs control into the kernel.Module lifecycle, holds the per-Resource lease slots and the
// Control Lane, and centralises all audit writes. The lease state machine itself is in slot.go, and the expiry loop in lane.go
package control

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nervus-os/nervud/internal/audit"
	"github.com/nervus-os/nervud/internal/motiongate"
	"github.com/nervus-os/nervud/internal/scheduler"
)

// laneTick is how often the Control Lane checks for deadline/deadman expiry.
//
// 10ms is about 3% resolution against HUMAN's 300ms deadman, which is enough; when idle (no leases) onTick
// only scans the immutable slot snapshot and does an atomic Load per slot, without taking control's change lock.
const laneTick = 10 * time.Millisecond

// maxResourceSlots is a hard upper bound on every Safety and Control Lane
// scan. Slots intentionally survive catalog removal so RevokeAll can retain a
// lock-free immutable snapshot; without a bound, repeated handle churn could
// make the real-time path grow for the lifetime of the process.
const maxResourceSlots = 1024

// laneExitTimeout caps how long Stop waits for the Control Lane to exit (see the ordering notes on Module.Stop)
const laneExitTimeout = 500 * time.Millisecond

// LaneSpawner is the narrow scheduler interface control needs (consumer-defined; *scheduler.Scheduler
// satisfies it implicitly). It has the same shape as safety.LaneSpawner but each defines its own: each module declaring only
// the one method it needs constrains the dependency surface better than sharing an interface
type LaneSpawner interface {
	SpawnDedicated(name string, policy scheduler.Policy, priority int, fn func(context.Context)) error
}

// LeaseObserver receives the terminal boundary of an effective ControlLease.
//
// The callback runs only after the lease has been removed from the authoritative
// slot. It may run on the Control Lane, a control-plane caller, or Stop, so the
// implementation must be concurrency-safe and return promptly. RevokeAll never
// invokes it on the Safety Supervisor Lane; that path is deferred to
// drainRevoked so the RT path remains lock-free, allocation-free, and
// non-blocking.
type LeaseObserver interface {
	ControlLeaseEnded(conn ConnID, resource string, leaseID ID)
}

type leaseObserverHolder struct {
	observer LeaseObserver
}

// resourceSlot is a Resource's exclusive_control slot. Once created a slot is never deleted,
// so slots snapshots and Safety's RevokeAll can hold its stable pointer for a long time.
type resourceSlot struct {
	resource string
	cur      atomic.Pointer[Lease]
	fresh    atomic.Int64

	// The Safety path only publishes revoked leases here; audit and observer notifications are drained by a normal-priority path.
	// Acquire drains before publishing on the same Resource again, so each slot needs only one pending position.
	revoked      atomic.Pointer[Lease]
	revokedEpoch atomic.Uint64
}

// slotIndex is the immutable Resource -> slot index. Adding a Resource copies on write under mu;
// readers and RevokeAll only read the snapshot atomically and never touch concurrent writes to a Go map.
type slotIndex struct {
	byResource map[string]*resourceSlot
	all        []*resourceSlot
}

// Module plugs control into kernel.Module + kernel.FatalReporter, and implements safety's
// LeaseRevoker (RevokeAll)
type Module struct {
	spawner LaneSpawner
	gate    *motiongate.Gate
	aud     audit.Recorder
	log     *slog.Logger
	policy  Policy

	// slots isolates exclusive_control by resolved stable Resource handle. The index is immutable;
	// each slot's cur/fresh are independent, so different Resources can be held in parallel and deadmen don't keep each other alive.
	slots atomic.Pointer[slotIndex]

	// mu only serialises control's own change paths. RevokeAll never takes it: it runs on safety's
	// Supervisor Lane (FIFO 90), and waiting for a normal-priority lock holder would be priority inversion
	mu sync.Mutex

	// base is the monotonic clock baseline shared by all slots; each slot's fresh stores nanoseconds since base.
	base time.Time

	// RevokeAll wraps the whole lock-free slot clearing in an odd/even seqlock. Acquire may only publish a new lease
	// when it reads the same even value, closing the race of regaining the same Resource while Safety is publishing revocations to notify.
	revokeSeq   atomic.Uint64
	safetyFloor atomic.Uint64
	revPending  atomic.Uint64
	revDrainMu  sync.Mutex

	observer atomic.Pointer[leaseObserverHolder]

	// laneWG counts running Control Lanes; Stop uses it to order "the Lane has exited" before the final revocation / drain,
	// so Stop can't fail to revoke after the Lane exits, and drain and the Lane can't consume the ring concurrently
	laneWG sync.WaitGroup

	stopCh   chan struct{}
	stopOnce sync.Once
	fatal    chan error
}

// New constructs control.Module.
//
// gate must be the same instance created by motiongate.New in main.go. safety and control
// share it so they can atomically observe the same latch state and epoch
func New(spawner LaneSpawner, gate *motiongate.Gate, aud audit.Recorder, log *slog.Logger, policy Policy) *Module {
	base := &resourceSlot{resource: ResourceBaseMain}
	index := &slotIndex{
		byResource: map[string]*resourceSlot{ResourceBaseMain: base},
		all:        []*resourceSlot{base},
	}
	m := &Module{
		spawner: spawner,
		gate:    gate,
		aud:     aud,
		log:     log,
		policy:  policy,
		base:    time.Now(),
		stopCh:  make(chan struct{}),
		fatal:   make(chan error, 1),
	}
	m.slots.Store(index)
	return m
}

func (m *Module) Name() string { return "control" }

// SetLeaseObserver installs the lifecycle observer used to invalidate data-plane
// state derived from a ControlLease. Passing nil removes the observer. Replacing
// it is atomic and is safe before or during Module operation.
func (m *Module) SetLeaseObserver(observer LeaseObserver) {
	if observer == nil {
		m.observer.Store(nil)
		return
	}
	m.observer.Store(&leaseObserverHolder{observer: observer})
}

func (m *Module) notifyLeaseEnded(l *Lease) {
	if holder := m.observer.Load(); holder != nil {
		holder.observer.ControlLeaseEnded(l.Conn, l.Resource, l.ID)
	}
}

// Start validates Policy, then starts the Control Lane.
//
// SpawnDedicated is a synchronous handshake. In production, if the RT priority can't be set, Start must return an error,
// because running the Control Lane on a thread with no timing guarantee would make a false safety promise
func (m *Module) Start(_ context.Context) error {
	if err := m.policy.Validate(); err != nil {
		return fmt.Errorf("control: invalid policy: %w", err)
	}
	// laneWG.Add must happen-before the Done in laneFn, so Add before spawning and roll back on failure.
	// When SpawnDedicated returns nil, fn is guaranteed to run, so Add/Done pair up exactly
	m.laneWG.Add(1)
	if err := m.spawner.SpawnDedicated("control",
		scheduler.PolicyRR, scheduler.PrioControl,
		m.laneFn("control", m.runLane)); err != nil {
		m.laneWG.Done()
		return fmt.Errorf("control: start control lane: %w", err)
	}
	if m.log != nil {
		m.log.Info("control: ready", "prio", scheduler.PrioControl,
			"human_ttl", m.policy.Human.TTL, "human_deadman", m.policy.Human.Deadman,
			"ai_ttl", m.policy.AI.TTL)
	}
	return nil
}

// Stop stops the Lane and revokes leftover leases.
//
// No control survives shutdown: a new Host session must start from NONE without restoring old Leases.
// This actively revokes once and increments the epoch, so old commands already in Provider queues become invalid before the process exits,
// rather than relying on the epoch happening to differ at the next start.
//
// Order: close stopCh first so the Lane exits, wait for it to really exit (waitLanes), then do the final drain and revocation.
// This ordering guarantees "the Lane has stopped" comes before "Stop consumes the revocation ring", so the two never consume the same ring at once; it also guarantees
// no Lane is running expiry detection after shutdown. The Acquire side relies on stopCh to refuse new leases (see Acquire).
func (m *Module) Stop(_ context.Context) error {
	m.stopOnce.Do(func() {
		close(m.stopCh)
		m.waitLanes(laneExitTimeout)
	})

	m.mu.Lock()
	defer m.mu.Unlock()
	m.drainRevoked()
	m.dropMatchingLocked(func(*Lease) bool { return true }, actionRevoked, ErrShuttingDown)
	return nil
}

// waitLanes waits up to d for the Control Lane to exit. On timeout it gives up and warns: one stuck Lane mustn't
// drag down the whole shutdown sequence (the kernel has its own 5s per-module Stop limit as a backstop).
func (m *Module) waitLanes(d time.Duration) {
	done := make(chan struct{})
	go func() {
		m.laneWG.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(d):
		if m.log != nil {
			m.log.Warn("control: lane did not exit before shutdown drain", "timeout", d)
		}
	}
}

// stopping reports whether the module has entered shutdown (stopCh closed). Acquire uses it under mu to refuse new leases.
func (m *Module) stopping() bool {
	select {
	case <-m.stopCh:
		return true
	default:
		return false
	}
}

// Fatal implements kernel.FatalReporter: if the Control Lane exits while stopCh isn't closed, a fatal error is reported
func (m *Module) Fatal() <-chan error { return m.fatal }

// laneFn wraps the Lane body, recognising an exit without Stop as a structural fault and reporting Fatal
// (the same shape as safety.Module.laneFn)
func (m *Module) laneFn(name string, body func(context.Context)) func(context.Context) {
	return func(ctx context.Context) {
		// laneWG.Add happens in Start (after a successful spawn); only Done happens here. Adding inside the goroutine
		// would race Start -> Stop immediately: Stop's Wait could return before fn even starts
		defer m.laneWG.Done()

		body(ctx)
		select {
		case <-m.stopCh:
			return // normal: the module's Stop closed stopCh
		default:
		}
		select {
		case m.fatal <- fmt.Errorf("control: lane %q exited unexpectedly", name):
		default:
		}
	}
}

// Audit Action names. Defined in one place so nobody mistypes a string; offline rules match "control.<Action>"
const (
	actionGranted        = "LeaseGranted"
	actionRenewed        = "LeaseRenewed"
	actionReleased       = "LeaseReleased"
	actionPreempted      = "LeasePreempted"
	actionExpired        = "LeaseExpired"
	actionDeadmanExpired = "LeaseDeadmanExpired"
	actionRevoked        = "LeaseRevoked"
	actionDenied         = "LeaseDenied"
)

// recordLease records an audit entry about a specific lease.
//
// A non-nil cause sets Denied: offline rules can pick out abnormally ended control sessions at a glance (expiry,
// deadman lapse, revoked by Safety), separate from normal issue/renewal/release. Same as safety.recordEvent
func (m *Module) recordLease(action string, l *Lease, cause error) {
	m.aud.Record(context.Background(), audit.Event{
		Action:  "control." + action,
		Subject: l.Owner.String(),
		Denied:  cause != nil,
		Err:     cause,
		Detail: fmt.Sprintf("lease=%s class=%s resource=%s epoch=%d",
			l.ID, l.Class, l.Resource, l.Epoch),
	})
}

// recordDenied records a refused request. There's no lease yet, so it's attributed to the requester's own identity
func (m *Module) recordDenied(req Request, cause error) {
	m.aud.Record(context.Background(), audit.Event{
		Action:  "control." + actionDenied,
		Subject: req.Owner.String(),
		Denied:  true,
		Err:     cause,
		Detail: fmt.Sprintf("class=%s resource=%s conn=%d",
			req.Class, req.Resource, req.Conn),
	})
}
