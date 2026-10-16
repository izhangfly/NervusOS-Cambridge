// This file is the body of the Operation Manager: the kernel.Module lifecycle, the outward operations
// (Create/Get/Cancel/Subscribe), the consumer-defined narrow interfaces (ResourceValidator/
// LeaseValidator), subscription fan-out, the background convergence goroutine, and the non-blocking Safety
// takeover delivery SafetySupersede. The state machine itself is in operation.go, and the Provider reporting seam in
// reporter.go.
package operation

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	ipcv1 "github.com/nervus-os/nervus-ipc/protocol/ipcv1"

	"github.com/nervus-os/nervud/internal/audit"
	"github.com/nervus-os/nervud/internal/identity"
)

// ResourceValidator is the narrow interface on the resource side (consumer-defined; *resource.Module satisfies it
// implicitly). Create uses it to check that a resource_handle is a known handle in the current robot Profile.
type ResourceValidator interface {
	Valid(handle string) bool
}

// LeaseValidator is the narrow interface on the control side (consumer-defined). Motion operations
// (leaseID != 0) use it at Create to check: is the given leaseID currently authorised for motion on the resource,
// and is the epoch fresh (= equal to the active motion epoch)? Anything doubtful returns false
// (fail-closed).
//
// # Why the first parameter is conn
//
// leaseID is a connection-scoped wire handle, not control's internal [16]byte ID:
// the same number on two different connections is two completely unrelated leases. Without conn, this interface couldn't
// express a meaningful query; the implementer could only take a number and guess whose it is.
//
// The mapping (wire handle -> control.ID) lives in ipc's conn, so ipc provides the implementation,
// injected by main.go at wiring. This package still knows neither control nor ipc.
type LeaseValidator interface {
	ValidLease(conn ConnHandle, leaseID, epoch uint64, resource string) bool
}

// deadlineScanInterval is how often the background goroutine scans for expired deadlines. Operations
// aren't a hot path, and 100ms resolution is plenty for second-scale deadlines; when idle one scan only walks a few running
// operations, so the cost is negligible.
const deadlineScanInterval = 100 * time.Millisecond

// subBuffer is the buffer depth of each subscription channel. Fan-out uses non-blocking sends and drops when full
// (no backpressure: a slow consumer must not hold up the state machine, still less Safety convergence).
const subBuffer = 16

// maxDeadlineHorizon is the longest deadline Create allows. It stops an absurdly distant deadline
// from effectively making an operation never time out. Arm trajectories and navigation are all within minutes.
const maxDeadlineHorizon = 30 * time.Minute

// terminalRetention is how long terminal operations stay in m.ops. Terminal records can't be deleted at once
// (an SDK reconnect or a late Get/Subscribe still needs to read the outcome), but they can't pile up forever either, or once IPC is
// connected they become a memory DoS channel. Once expired, the background scan reclaims them (with their terminal payload).
const terminalRetention = 5 * time.Minute

// subscription is one subscription. closed marks an idempotent close; every send on and close of ch happens
// under Manager.mu, so there's no send-on-closed race.
type subscription struct {
	ch     chan Event
	closed bool
}

// Manager is nervud's Operation Manager, and also implements kernel.Module.
//
// One mu protects ops and subs; v1 numbers are small, and a single lock keeps state transitions and subscription publishing consistent. Terminal transitions are
// compare-and-set under mu: written only if the current state isn't terminal and the transition is legal, and concurrent
// Succeed/Fail/Cancelled calls get a single winner decided by mu.
type Manager struct {
	res   ResourceValidator
	lease LeaseValidator
	aud   audit.Recorder
	log   *slog.Logger

	// now is for injecting a controllable clock in tests; always time.Now in production.
	now func() time.Time

	mu   sync.Mutex
	ops  map[uint64]*Operation
	subs map[uint64][]*subscription

	// observer is the wire-side event bypass; see SetEventObserver.
	observer func(Event)

	// nextID allocates operation_ids monotonically from 1 (0 is the invalid sentinel), never reused.
	nextID atomic.Uint64

	// supersedeEpoch is the "takeover boundary" Safety delivers: every running motion operation with
	// MotionEpoch < it should be converged to FAILED. SafetySupersede does only one
	// atomic max write + one non-blocking wake and never touches mu, so it never blocks the Safety Lane.
	// The actual convergence happens on the background goroutine, so the Safety caller isn't blocked.
	supersedeEpoch atomic.Uint64

	// wake makes the background goroutine run a convergence round at once (buffered 1 + non-blocking send).
	wake chan struct{}

	stopCh   chan struct{}
	stopOnce sync.Once
	doneCh   chan struct{} // background goroutine exit signal
}

// New constructs the Operation Manager.
//
// res/lease are injected as narrow interfaces (wired in main.go). lease may be nil: then every motion
// operation (leaseID != 0) is refused up front (fail-closed). aud must not be nil.
func New(res ResourceValidator, lease LeaseValidator, aud audit.Recorder, log *slog.Logger) *Manager {
	return &Manager{
		res:    res,
		lease:  lease,
		aud:    aud,
		log:    log,
		now:    time.Now,
		ops:    make(map[uint64]*Operation),
		subs:   make(map[uint64][]*subscription),
		wake:   make(chan struct{}, 1),
		stopCh: make(chan struct{}),
		doneCh: make(chan struct{}),
	}
}

// SetLeaseValidator fills in the lease validator at wiring.
//
// # Why it can't be passed to New
//
// The validator's implementation lives in ipc (which holds both the wire handle mapping and the control module), and ipc
// must be built after operation, because operation has to be ready before IPC opens. Construction order turns this
// dependency into a cycle, and one back-fill at wiring breaks it.
//
// Until then lease is nil, and motion operations all fail closed. That's exactly what we want:
// allowing motion before wiring has finished is far more dangerous than refusing it.
func (m *Manager) SetLeaseValidator(v LeaseValidator) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lease = v
}

// leaseValidator returns the current validator. validateCreate runs outside m.mu, while
// SetLeaseValidator writes under the lock, so reading it directly without this getter would be a data race.
func (m *Manager) leaseValidator() LeaseValidator {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lease
}

// SetEventObserver installs the wire-side event bypass. Called once at wiring, never swapped at run time.
//
// # Why it's needed instead of ipc using Subscribe
//
// Subscribe gives an in-process chan, one per subscription. The wire side's fan-out has its own machinery
// (delivery_class backpressure, SubscriptionClosed, cleanup on disconnect), which lives in
// internal/subscription. Having ipc start a goroutine per wire subscription to bridge
// the two mechanisms would record "who is watching this operation" in two places, and once they fall out of step
// the symptom is a subscriber that never gets events, or gets events that should have stopped long ago.
//
// There's only one bypass, and the wire side dispatches by operation_id itself.
//
// # It must not block, and must not call back into this package
//
// It's called under m.mu. Blocking would hold up the state machine and Safety convergence; calling back in would deadlock.
//
// The lock order is always operation.mu -> subscription.mu, never the reverse (subscription doesn't know
// operation).
func (m *Manager) SetEventObserver(fn func(Event)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.observer = fn
}

func (m *Manager) Name() string { return "operation" }

// Start starts the background convergence goroutine, which handles both deadline expiry scans and Safety supersede
// convergence. The loop exits only on the stopCh closed by Stop,
// not on Start(ctx)'s ctx; otherwise it would be woken in parallel with stopAll by the same signal,
// shutdown order would no longer be decided by reverse Stop alone, and cleanup (converging unfinished operations, audit) could be cut off.
func (m *Manager) Start(_ context.Context) error {
	go m.loop()
	if m.log != nil {
		m.log.Info("operation: ready")
	}
	return nil
}

// Stop stops the background goroutine and converges unfinished operations to FAILED by policy.
// No running operations survive shutdown: a new host session starts empty, consistent with motion epoch semantics.
func (m *Manager) Stop(_ context.Context) error {
	m.stopOnce.Do(func() { close(m.stopCh) })
	<-m.doneCh // wait for the background goroutine to exit; after that access is exclusive, with no concurrent convergence

	m.mu.Lock()
	defer m.mu.Unlock()
	for _, op := range m.ops {
		if op.State.Terminal() {
			continue
		}
		// Shutdown convergence: unfinished operations all go to FAILED(UNAVAILABLE). For motion, the related
		// control is revoked by control in its own Stop (revoking leases + incrementing the epoch); this package doesn't touch the gate.
		m.terminateLocked(op, StateFailed, ipcv1.StatusCode_STATUS_CODE_UNAVAILABLE, nil,
			actionShutdown, ErrShuttingDown)
	}
	return nil
}

// loop is the only background goroutine: deadline scans + Safety supersede convergence.
// It exits only on stopCh.
func (m *Manager) loop() {
	defer close(m.doneCh)
	t := time.NewTicker(deadlineScanInterval)
	defer t.Stop()
	for {
		select {
		case <-m.stopCh:
			return
		case <-m.wake:
			m.convergeSupersede()
		case <-t.C:
			m.convergeSupersede()
			m.scanDeadlines()
			m.reapTerminal()
		}
	}
}

// -------------------------------------------------------------------------
// Outward operations
// -------------------------------------------------------------------------

// Create creates an operation (called by dispatch when it meets an operation-returning method).
//
// Checks: the resource is valid (ResourceValidator.Valid); motion (leaseID != 0)
// needs a valid ControlLease and a fresh epoch; the deadline is reasonable. On success it returns (id, ACCEPTED);
// on failure (0, the precondition code). A failed precondition issues no id and stores nothing, only an audit entry
// (equivalent to the state diagram's reject-before-start, but with no trackable handle).
//
// conn is the connection that owns this operation, for convergence on disconnect;
// internal system creations pass nil.
func (m *Manager) Create(conn, provider ConnHandle, caller identity.Caller, origin OriginBinding,
	resources []string, leaseID, epoch uint64, deadline time.Time) (uint64, ipcv1.StatusCode) {

	if code := m.validateCreate(conn, caller, resources, leaseID, epoch, deadline); code != ipcv1.StatusCode_STATUS_CODE_OK {
		m.recordRejected(caller, resources, leaseID, code)
		return 0, code
	}

	now := m.now()
	id := m.nextID.Add(1)
	// The type binding belongs to the Manager: deep-copy SchemaHash to cut the shared
	// backing array with the caller, otherwise the caller changing the slice later would tamper with the authoritative binding and race with subscriber reads.
	origin.SchemaHash = cloneBytes(origin.SchemaHash)
	op := &Operation{
		ID:           id,
		Origin:       origin,
		Caller:       caller,
		Resources:    cloneStrings(resources),
		LeaseID:      leaseID,
		MotionEpoch:  epoch,
		Deadline:     deadline,
		State:        StatePending,
		CreatedAt:    now,
		UpdatedAt:    now,
		conn:         conn,
		providerConn: provider,
	}

	m.mu.Lock()
	// No more accepting after shutdown: otherwise it would issue an orphan operation with no background goroutine left to handle its
	// deadline/convergence (the same reason control refuses new leases after shutdown).
	if m.stopping() {
		m.mu.Unlock()
		m.recordRejected(caller, resources, leaseID, ipcv1.StatusCode_STATUS_CODE_UNAVAILABLE)
		return 0, ipcv1.StatusCode_STATUS_CODE_UNAVAILABLE
	}
	m.ops[id] = op
	// The Created audit is written inside the lock: reading op after unlocking would race with Stop/ReleaseByConn/background convergence changing
	// op.State, and could record ShutdownConverged/ConnClosed before Created
	//  (order inverted). record only does one quick slog (async in production), so recording it under the lock matches the other transition audits.
	m.record(actionCreated, op, false, nil)
	m.mu.Unlock()

	return id, ipcv1.StatusCode_STATUS_CODE_ACCEPTED
}

// validateCreate runs all of Create's preconditions and returns OK if they pass. It changes no state.
func (m *Manager) validateCreate(conn ConnHandle, caller identity.Caller, resources []string,
	leaseID, epoch uint64, deadline time.Time) ipcv1.StatusCode {

	// A motion operation must bind exactly the resource its lease covers. A
	// non-motion long task (leaseID == 0) may bind none: package install is
	// declared returns_operation in pkg_manager.proto, and its endpoint has no
	// physical resource. Rejecting those made INSTALL over IPC unreachable.
	// They have nothing for Safety to take over, which is fine: they don't move
	// anything. They still get a deadline, connection-close convergence and audit.
	if len(resources) == 0 && leaseID != 0 {
		return ipcv1.StatusCode_STATUS_CODE_INVALID_ARGUMENT
	}
	for _, h := range resources {
		if h == "" {
			return ipcv1.StatusCode_STATUS_CODE_INVALID_ARGUMENT
		}
		// An unknown/invalid resource is an unmet precondition (copying control's existing mapping of lease precondition failures to
		// FAILED_PRECONDITION).
		if m.res == nil || !m.res.Valid(h) {
			return ipcv1.StatusCode_STATUS_CODE_FAILED_PRECONDITION
		}
	}
	// The current contract carries one lease binding. A ControlLease is scoped
	// to exactly one Resource, so accepting several resources here would either
	// reuse one lease outside its scope or make validation depend on a fake
	// validator. Multi-resource motion requires repeated (resource, lease,
	// epoch) bindings in the future wire contract.
	if leaseID != 0 && len(resources) != 1 {
		return ipcv1.StatusCode_STATUS_CODE_INVALID_ARGUMENT
	}

	// Deadline sanity: it must be in the future, and not absurdly far.
	now := m.now()
	if !deadline.After(now) || deadline.After(now.Add(maxDeadlineHorizon)) {
		return ipcv1.StatusCode_STATUS_CODE_INVALID_ARGUMENT
	}

	// Motion is data-driven by leaseID != 0: it needs a valid lease and a fresh epoch,
	// bound to this operation's single resource (v1 single resource).
	if leaseID != 0 {
		validator := m.leaseValidator()
		if validator == nil {
			return ipcv1.StatusCode_STATUS_CODE_FAILED_PRECONDITION
		}
		if !validator.ValidLease(conn, leaseID, epoch, resources[0]) {
			return ipcv1.StatusCode_STATUS_CODE_FAILED_PRECONDITION
		}
	}
	_ = caller // dispatch already authenticated caller; this package only keeps it for visibility and audit
	return ipcv1.StatusCode_STATUS_CODE_OK
}

// Get snapshots an operation (including the Origin binding, for decoding after an SDK reconnect).
// Visible only to the creating caller or the system; other callers get a miss, so whether an operation exists isn't leaked.
func (m *Manager) Get(caller identity.Caller, id uint64) (Operation, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	op, ok := m.ops[id]
	if !ok || !canSee(caller, op) {
		return Operation{}, false
	}
	return op.clone(), true
}

// ProviderOperation looks up an operation by its executing connection.
//
// The ownership check on the reporting side (Accept/Progress/Complete) uses it: a Provider can only report on what nervud
// dispatched to it. Without this binding, any system service could report someone else's operation as
// failed, and the caller would see a "normal" failure, with even its detailed cause forged.
//
// "Doesn't exist" and "isn't yours" get the same answer: telling them apart would tell the caller "this id exists, it's just not
// yours", which is information in itself. Consistent with Get's indistinguishable projection.
func (m *Manager) ProviderOperation(provider ConnHandle, id uint64) (Operation, bool) {
	if provider == nil {
		// A nil executor means "carried by a built-in, no external Provider". Letting it match would let any
		// caller without a connection report. Fail closed.
		return Operation{}, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	op, ok := m.ops[id]
	if !ok || op.providerConn != provider {
		return Operation{}, false
	}
	return op.clone(), true
}

// Cancel requests cancellation (-> CANCEL_REQUESTED). Returning ACCEPTED != cancelled:
// it only means the cancel request was accepted; the real CANCELLED waits for the Provider's confirmation (reporter.Cancelled).
//
// Visibility: other callers get NOT_FOUND. For a finished operation, cancelling is a no-op that still returns
// ACCEPTED (the request was received but the state won't change, in line with "ACCEPTED != cancelled").
func (m *Manager) Cancel(caller identity.Caller, id uint64) ipcv1.StatusCode {
	m.mu.Lock()
	defer m.mu.Unlock()
	op, ok := m.ops[id]
	if !ok || !canSee(caller, op) {
		return ipcv1.StatusCode_STATUS_CODE_NOT_FOUND
	}
	switch {
	case op.State.Terminal():
		// Already finished: nothing to cancel, idempotent no-op.
	case op.State == StateCancelRequested:
		// Already being cancelled: idempotent no-op.
	case canTransition(op.State, StateCancelRequested):
		m.transitionLocked(op, StateCancelRequested)
		m.record(actionCancelRequested, op, false, nil)
	}
	return ipcv1.StatusCode_STATUS_CODE_ACCEPTED
}

// Subscribe subscribes to an operation's state/progress events. It returns (a read-only channel, a cancel function).
//
// The cancel function is idempotent: calling it removes this subscription from the fan-out and closes the channel. ipc calls it on disconnect
// to clean up, so subscriptions don't leak. The channel is also closed automatically when the operation reaches a terminal state.
// Another caller's, or a missing one: returns an already closed empty channel + a no-op cancel function (indistinguishable projection).
func (m *Manager) Subscribe(caller identity.Caller, id uint64) (<-chan Event, func()) {
	m.mu.Lock()
	defer m.mu.Unlock()
	op, ok := m.ops[id]
	if !ok || !canSee(caller, op) {
		closed := make(chan Event)
		close(closed)
		return closed, func() {}
	}
	// If the operation has already finished, there are no future events to wait for, so return an already closed empty channel without
	// registering it in m.subs. Otherwise the subscription would hang on the fan-out list forever, closed but never removed, and if the caller
	// never called cancel it would linger permanently; once IPC is connected, repeated Subscribes could inflate it into a memory DoS.
	if op.State.Terminal() {
		closed := make(chan Event)
		close(closed)
		return closed, func() {}
	}
	sub := &subscription{ch: make(chan Event, subBuffer)}
	m.subs[id] = append(m.subs[id], sub)
	cancel := func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		m.removeSubLocked(id, sub)
	}
	return sub.ch, cancel
}

// ReleaseByConn converges every unfinished operation under a connection to FAILED (for motion, the related
// control is revoked on the control side; this package doesn't touch the gate). Called by ipc on disconnect.
func (m *Manager) ReleaseByConn(conn ConnHandle) {
	if conn == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, op := range m.ops {
		if op.conn != conn || op.State.Terminal() {
			continue
		}
		m.terminateLocked(op, StateFailed, ipcv1.StatusCode_STATUS_CODE_UNAVAILABLE, nil,
			actionConnClosed, errConnClosed)
	}
}

// -------------------------------------------------------------------------
// Safety takeover
// -------------------------------------------------------------------------

// SafetySupersede is called by the control/safety side when Safety trips: it marks every running motion operation from before that epoch
// as FAILED (taken over).
//
// It never blocks the Safety Lane: the function does one atomic max write + one non-blocking wake delivery and
// returns, without taking mu, fanning out, or doing anything else that could block. The real state convergence happens in the background
// goroutine's convergeSupersede (using mu + non-blocking fan-out). So even with a slow
// subscriber that blocks, the Safety path isn't held up.
//
// epoch is the new motion epoch after Safety increments it: motion operations with MotionEpoch < epoch
// were all born before this revocation boundary and are all taken over.
func (m *Manager) SafetySupersede(epoch uint64) {
	// Atomic max: repeated trips only raise the boundary, and a late old value can't write it back down.
	for {
		cur := m.supersedeEpoch.Load()
		if epoch <= cur {
			break
		}
		if m.supersedeEpoch.CompareAndSwap(cur, epoch) {
			break
		}
	}
	// Non-blocking wake: if it's full (something already pending), skip it; the background goroutine will read the latest boundary.
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

// convergeSupersede converges running motion operations with MotionEpoch < supersedeEpoch to
// FAILED. It runs on the background goroutine, so it can safely take mu and fan out; it isn't on the Safety path.
func (m *Manager) convergeSupersede() {
	boundary := m.supersedeEpoch.Load()
	if boundary == 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, op := range m.ops {
		if op.State.Terminal() {
			continue
		}
		// Only motion operations (with a lease) born before the boundary are taken over. Non-motion ones
		//  (leaseID==0) aren't affected by the physical Safety boundary and aren't converged here.
		if op.LeaseID == 0 || op.MotionEpoch >= boundary {
			continue
		}
		m.terminateLocked(op, StateFailed, ipcv1.StatusCode_STATUS_CODE_FAILED_PRECONDITION, nil,
			actionSafetySuperseded, ErrSuperseded)
	}
}

// scanDeadlines converges unfinished operations past their deadline to FAILED(DEADLINE_EXCEEDED).
func (m *Manager) scanDeadlines() {
	now := m.now()
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, op := range m.ops {
		if op.State.Terminal() || op.Deadline.IsZero() || !now.After(op.Deadline) {
			continue
		}
		m.terminateLocked(op, StateFailed, ipcv1.StatusCode_STATUS_CODE_DEADLINE_EXCEEDED, nil,
			actionDeadlineExceeded, ErrDeadlineExceeded)
	}
}

// reapTerminal reclaims terminal operations older than terminalRetention, bounding memory use (DoS protection).
// Subscriptions of terminal operations were already closed and removed from m.subs in terminateLocked; this clears them again defensively.
func (m *Manager) reapTerminal() {
	now := m.now()
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, op := range m.ops {
		if op.State.Terminal() && now.Sub(op.UpdatedAt) > terminalRetention {
			delete(m.ops, id)
			delete(m.subs, id)
		}
	}
}

// -------------------------------------------------------------------------
// Internal: transitions, terminal writes, subscription fan-out, audit (all require holding mu)
// -------------------------------------------------------------------------

// transitionLocked applies one non-terminal transition and fans out an EventState. The caller must already have checked
// canTransition. Terminal transitions go through terminateLocked (with payload + CAS semantics).
func (m *Manager) transitionLocked(op *Operation, to State) {
	op.State = to
	op.UpdatedAt = m.now()
	m.emitLocked(op, Event{
		OperationID: op.ID,
		Kind:        EventState,
		State:       to,
		Origin:      op.Origin,
	})
}

// terminateLocked is the only entry point for writing a terminal state, with compare-and-set semantics: it writes only if the current state
// isn't terminal and the transition is legal. It sets the terminal payload, fans out the terminal event, closes and removes every subscription,
// and records audit. When already terminal or the transition is illegal it's a safe no-op (idempotent) and doesn't overwrite the existing terminal state.
func (m *Manager) terminateLocked(op *Operation, to State, status ipcv1.StatusCode,
	detail []byte, action string, cause error) bool {

	if op.State.Terminal() || !canTransition(op.State, to) {
		return false
	}
	op.State = to
	op.UpdatedAt = m.now()
	op.TerminalStatus = status
	// The terminal payload belongs to the Manager: deep-copy it to cut the backing array shared with the Provider caller,
	// so it can't be changed from outside after being written.
	switch to {
	case StateSucceeded:
		op.TerminalResult = cloneBytes(detail)
	case StateFailed:
		op.TerminalError = cloneBytes(detail)
	case StateUnspecified:
	case StatePending:
	case StateRunning:
	case StateCancelRequested:
	case StateCancelled:
	}
	m.emitLocked(op, Event{
		OperationID:    op.ID,
		Kind:           EventState,
		State:          to,
		Origin:         op.Origin,
		TerminalStatus: status,
		TerminalResult: op.TerminalResult,
		TerminalError:  op.TerminalError,
	})
	m.closeAllSubsLocked(op.ID)
	m.record(action, op, cause != nil, cause)
	return true
}

// emitLocked fans one event out to every subscriber of the operation without blocking. When a channel is full, the event is dropped
// (no backpressure; a slow consumer can catch up with Get at any time).
//
// Byte slices in the event are deep-copied once before delivery, so the copy subscribers get shares no backing array with the authoritative record's
// SchemaHash/result/error. If a subscriber breaks the contract and changes a slice, it only
// damages its own copy, without tampering with the record or racing with reads of it. One batch of subscribers shares
// this one copy, and receivers treat Event as read-only by convention.
func (m *Manager) emitLocked(op *Operation, ev Event) {
	subs := m.subs[op.ID]
	observer := m.observer
	if len(subs) == 0 && observer == nil {
		return
	}
	ev.Origin.SchemaHash = cloneBytes(ev.Origin.SchemaHash)
	ev.Payload = cloneBytes(ev.Payload)
	ev.TerminalResult = cloneBytes(ev.TerminalResult)
	ev.TerminalError = cloneBytes(ev.TerminalError)

	// The bypass goes first: subscribers on the wire get events through it, while in-process chan subscribers are
	// another path. The order has no meaning here, but fixing it keeps audit timing reproducible.
	if observer != nil {
		observer(ev)
	}

	for _, sub := range subs {
		if sub.closed {
			continue
		}
		select {
		case sub.ch <- ev:
		default:
			// Dropped: the subscription channel is full, and a slow consumer must not hold up the state machine / Safety convergence.
		}
	}
}

// closeAllSubsLocked closes and removes every subscription of the operation (no future events after a terminal state).
func (m *Manager) closeAllSubsLocked(id uint64) {
	for _, sub := range m.subs[id] {
		if !sub.closed {
			sub.closed = true
			close(sub.ch)
		}
	}
	delete(m.subs, id)
}

// removeSubLocked removes one subscription from the fan-out list and closes it (the cancel function Subscribe returns).
func (m *Manager) removeSubLocked(id uint64, target *subscription) {
	subs := m.subs[id]
	for i, sub := range subs {
		if sub != target {
			continue
		}
		if !sub.closed {
			sub.closed = true
			close(sub.ch)
		}
		m.subs[id] = append(subs[:i], subs[i+1:]...)
		if len(m.subs[id]) == 0 {
			delete(m.subs, id)
		}
		return
	}
}

// stopping reports whether the Manager has entered shutdown (stopCh closed). Call it holding mu or inside Create's
// critical section, for a view consistent with writes to ops.
func (m *Manager) stopping() bool {
	select {
	case <-m.stopCh:
		return true
	default:
		return false
	}
}

// canSee reports whether caller can see op: the system (empty PackageID = kernel) or the creator.
// Other callers never can, so whether someone else's operation exists isn't leaked.
func canSee(caller identity.Caller, op *Operation) bool {
	if caller.PackageID == "" {
		return true // system/kernel
	}
	return caller.PackageID == op.Caller.PackageID
}

// -------------------------------------------------------------------------
// Audit
// -------------------------------------------------------------------------

// Audit Action names. Offline rules match "operation.<Action>".
const (
	actionCreated          = "Created"
	actionCreateRejected   = "CreateRejected"
	actionCancelRequested  = "CancelRequested"
	actionAccepted         = "Accepted"
	actionSucceeded        = "Succeeded"
	actionFailed           = "Failed"
	actionCancelled        = "Cancelled"
	actionIllegal          = "IllegalTransition"
	actionSafetySuperseded = "SafetySuperseded"
	actionDeadlineExceeded = "DeadlineExceeded"
	actionConnClosed       = "ConnClosed"
	actionShutdown         = "ShutdownConverged"
)

// Sentinel reasons for internal system convergence (written into the audit Err, so offline analysis can tell abnormal endings apart).
// Safety takeover / deadline expiry reuse the public sentinels ErrSuperseded/ErrDeadlineExceeded
// (see operation.go), so audit and Provider returns share a source and one concept doesn't get two errors.
var errConnClosed = errStr("owning connection closed")

// errStr is a minimal error for internal convergence reasons. A value type avoids concerns about package-level mutable errors beyond
// gochecknoglobals, and gives the audit Err field stable text.
type errStr string

func (e errStr) Error() string { return string(e) }

// record writes an audit entry about a specific operation. denied lets offline rules pick out abnormal endings.
func (m *Manager) record(action string, op *Operation, denied bool, cause error) {
	m.aud.Record(context.Background(), audit.Event{
		Action:  "operation." + action,
		Subject: op.Caller.String(),
		Denied:  denied,
		Err:     cause,
		Detail: fmt.Sprintf("op=%d state=%s iface=%s method=%d lease=%d epoch=%d",
			op.ID, op.State, op.Origin.InterfaceID, op.Origin.MethodID, op.LeaseID, op.MotionEpoch),
	})
}

// recordRejected records a refused Create precondition. There's no operation record yet, so it's attributed to the requester.
func (m *Manager) recordRejected(caller identity.Caller, resources []string, leaseID uint64, code ipcv1.StatusCode) {
	m.aud.Record(context.Background(), audit.Event{
		Action:  "operation." + actionCreateRejected,
		Subject: caller.String(),
		Denied:  true,
		Err:     fmt.Errorf("create rejected: code=%d", int32(code)),
		Detail:  fmt.Sprintf("resources=%v lease=%d", resources, leaseID),
	})
}
