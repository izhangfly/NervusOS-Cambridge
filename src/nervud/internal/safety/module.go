// This file plugs safety into the kernel.Module lifecycle, holds the two RT Lanes (Stop / Supervisor)
// and the normal-priority audit drain goroutine, and provides the Observer / Controller surfaces.
package safety

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

const (
	// auditRingSize is the audit ring capacity (a power of 2). Enough to absorb one stop storm's fixed events; when full, drops are counted.
	auditRingSize = 256
	// superTick is how often the Supervisor checks timeout budgets. 10ms gives about 10% resolution on 100ms-scale budgets;
	// when idle (not latched) onTick returns at once, at negligible cost.
	superTick = 10 * time.Millisecond
	// drainInterval is how often auditDrain polls the ring. Audit isn't real-time, and 20ms of delay is acceptable.
	drainInterval = 20 * time.Millisecond
	// stopDrainTimeout caps how long Stop waits for auditDrain to finish writing.
	stopDrainTimeout = 500 * time.Millisecond
	// laneExitTimeout caps how long Stop waits for the two RT Lanes to exit (see the ordering notes on Module.Stop).
	laneExitTimeout = 500 * time.Millisecond
)

// LaneSpawner is the narrow scheduler interface safety needs (consumer-defined; *scheduler.Scheduler satisfies it implicitly).
type LaneSpawner interface {
	SpawnDedicated(name string, policy scheduler.Policy, priority int, fn func(context.Context)) error
}

// LeaseRevoker is the narrow interface for cleaning up actuator lease objects / sending EndpointRevoked once control lands
// (consumer-defined). May be nil in v1: incrementing the motion epoch is already the main revocation mechanism.
type LeaseRevoker interface {
	RevokeAll(epoch uint64)
}

// Module plugs safety into kernel.Module + kernel.FatalReporter, and implements Observer / Controller.
type Module struct {
	spawner  LaneSpawner
	gate     *motiongate.Gate
	aud      audit.Recorder
	log      *slog.Logger
	contract Contract
	path     SafetyPath
	src      StopProgressSource

	// Trigger hot path (all preallocated).
	pendingReason atomic.Uint32 // the latest trigger reason, read by the Stop Lane
	phase         atomic.Uint64 // [current StopPhase | the halt epoch it belongs to]; see packPhase
	wake          chan struct{} // wakes the Stop Lane (cap 1)
	superWake     chan struct{} // wakes the Supervisor (cap 1)

	ring *auditRing  // MPSC: produced by the Stop Lane + Supervisor, consumed by auditDrain
	sup  *supervisor // only accessed inside the Supervisor goroutine

	// Lifecycle.
	stopCh   chan struct{}
	stopOnce sync.Once
	fatal    chan error // cap 1, FatalReporter

	// laneWG counts the two running RT Lanes; Stop uses it to order "the Lanes have exited" before the audit wrap-up.
	// Without that order, a Lane could still write to the ring after the last drain, and those events would never
	// reach disk, and they're exactly the kind most worth seeing during a stop
	laneWG sync.WaitGroup

	// drainStop is closed by Stop after every Lane has exited, driving auditDrain's last drain.
	// It's separate from stopCh precisely to express that ordering
	drainStop chan struct{}
	drainDone chan struct{}
}

// New constructs safety.Module. All dependencies are injected as narrow interfaces/values (wiring pattern as in pkgregistry/module.go).
// gate is built once by main.go with motiongate.New, and the same instance is injected into safety and control.
// revoker may be nil.
func New(
	spawner LaneSpawner,
	gate *motiongate.Gate,
	aud audit.Recorder,
	log *slog.Logger,
	contract Contract,
	path SafetyPath,
	src StopProgressSource,
	revoker LeaseRevoker,
) *Module {
	m := &Module{
		spawner:   spawner,
		gate:      gate,
		aud:       aud,
		log:       log,
		contract:  contract,
		path:      path,
		src:       src,
		wake:      make(chan struct{}, 1),
		superWake: make(chan struct{}, 1),
		ring:      newAuditRing(auditRingSize),
		stopCh:    make(chan struct{}),
		fatal:     make(chan error, 1),
		drainStop: make(chan struct{}),
		drainDone: make(chan struct{}),
	}
	m.phase.Store(packPhase(PhaseUnspecified, 0))
	m.sup = &supervisor{
		gate:     gate,
		ring:     m.ring,
		contract: contract,
		revoker:  revoker,
		phase:    &m.phase,
		pending:  &m.pendingReason,
	}
	return m
}

func (m *Module) Name() string { return "safety" }

// Start validates the Contract, then starts the two RT Lanes and the audit drain goroutine.
//
// SpawnDedicated is a synchronous handshake. In production, if the RT priority can't be set, Start must return an error,
// because a Safety Lane with no timing guarantee can't claim to be ready. That's why this module registers before IPC.
func (m *Module) Start(_ context.Context) error {
	if err := m.contract.Validate(); err != nil {
		return fmt.Errorf("safety: invalid contract: %w", err)
	}
	// laneWG.Add must happen-before the matching Done (in laneFn), so Add before spawning;
	// roll the count back if the spawn fails. When SpawnDedicated returns nil, fn is guaranteed to be called,
	// so Add and Done pair up exactly
	m.laneWG.Add(1)
	if err := m.spawner.SpawnDedicated("safety-stop",
		scheduler.PolicyFIFO, scheduler.PrioSafetyLatch,
		m.laneFn("safety-stop", m.runStopLane)); err != nil {
		m.laneWG.Done()
		return fmt.Errorf("safety: start stop lane: %w", err)
	}
	m.laneWG.Add(1)
	if err := m.spawner.SpawnDedicated("safety-supervisor",
		scheduler.PolicyFIFO, scheduler.PrioSafety,
		m.laneFn("safety-supervisor", m.runSupervisor)); err != nil {
		m.laneWG.Done()
		return fmt.Errorf("safety: start supervisor lane: %w", err)
	}
	go m.runAuditDrain()
	if m.log != nil {
		m.log.Info("safety: armed", "stop_prio", scheduler.PrioSafetyLatch, "supervisor_prio", scheduler.PrioSafety)
	}
	return nil
}

// Stop finishes in two beats: first let the two RT Lanes exit, then let auditDrain do its last drain.
//
// The order matters. The Scheduler's join of the Lanes happens after every module's Stop
// (sched.Shutdown); relying on that alone, this module's audit drain would have long finished while the Stop Lane might still be
// delivering and writing to the ring, and those events, with nobody left to read them, would simply be lost. So it waits for
// its own Lanes here, and only closes drainStop after they've exited (or it gave up on a timeout).
//
// Both waits have limits: one stuck Lane mustn't drag down the whole shutdown sequence (the kernel also has a 5s
// per-module Stop limit as a backstop). On timeout it gives up and warns, falling back to the old behaviour of possibly losing a few records.
func (m *Module) Stop(_ context.Context) error {
	m.stopOnce.Do(func() {
		close(m.stopCh)
		m.waitLanes(laneExitTimeout)
		close(m.drainStop)
	})
	select {
	case <-m.drainDone:
	case <-time.After(stopDrainTimeout):
	}
	return nil
}

// waitLanes waits up to d for this module's RT Lanes to exit
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
			m.log.Warn("safety: lanes did not exit before audit drain closed; some events may be lost",
				"timeout", d)
		}
	}
}

// Fatal implements kernel.FatalReporter: if either Lane exits while stopCh isn't closed (an anomaly, or the shutdown order was disrupted),
// a fatal error is reported, triggering the kernel's reverse shutdown, a non-zero exit and a systemd restart (with the MCU heartbeat as the backstop).
func (m *Module) Fatal() <-chan error { return m.fatal }

// laneFn wraps the Lane body, recognising an exit without Stop as a structural fault and reporting Fatal.
func (m *Module) laneFn(name string, body func(context.Context)) func(context.Context) {
	return func(ctx context.Context) {
		// laneWG's Add happens in Start, after SpawnDedicated succeeds (only then is fn guaranteed to run);
		// this only does Done. Adding inside the goroutine would race Start -> Stop immediately: Stop's
		// Wait could return before fn even starts, while the count is still 0
		defer m.laneWG.Done()

		body(ctx)
		select {
		case <-m.stopCh:
			return // normal: the module's Stop closed stopCh
		default:
		}
		select {
		case m.fatal <- fmt.Errorf("safety: lane %q exited unexpectedly", name):
		default:
		}
	}
}

// runSupervisor is the Supervisor Lane (FIFO 90) loop: it only waits on bounded channels/the ticker
// and does no arbitrary I/O. superWake comes from Trip/re-arm; reports from Providers; the ticker drives timeout checks.
func (m *Module) runSupervisor(ctx context.Context) {
	reports := m.src.Reports() // taken once; the stub returns a nil channel (that branch never fires)
	ticker := time.NewTicker(superTick)
	defer ticker.Stop()
	for {
		select {
		case <-m.stopCh:
			return
		case <-ctx.Done():
			return
		case <-m.superWake:
			m.sup.reconcile(time.Now())
		case r := <-reports:
			m.sup.onReport(r, time.Now())
		case t := <-ticker.C:
			m.sup.onTick(t)
		}
	}
}

// runAuditDrain turns the fixed event codes in the ring into audit records and stores them, at normal priority. It's poll-driven,
// so RT producers never touch a channel lock to wake it.
func (m *Module) runAuditDrain() {
	defer close(m.drainDone)
	ticker := time.NewTicker(drainInterval)
	defer ticker.Stop()
	for {
		m.drainAll()
		select {
		case <-m.drainStop:
			// drainStop is only closed after both Lanes have exited (or the wait gave up), so by now the ring holds
			// every event the Lanes wrote, and the last drain won't miss deliveries at the very end of the stop
			m.drainAll()
			m.reportDropped()
			return
		case <-ticker.C:
		}
	}
}

func (m *Module) drainAll() {
	for {
		rec, ok := m.ring.pop()
		if !ok {
			return
		}
		m.recordEvent(rec)
	}
}

func (m *Module) recordEvent(rec eventRecord) {
	ev := audit.Event{
		Action:  "safety." + rec.kind.String(),
		Subject: "kernel",
		Detail:  fmt.Sprintf("epoch=%d reason=%s phase=%s", rec.epoch, rec.reason, rec.phase),
	}
	switch rec.kind {
	case evDeliveryFault, evProviderAcceptTimeout, evDeviceAckTimeout, evStandstillTimeout:
		ev.Denied = true // mark safety events that didn't meet expectations, for offline filtering
	case evTripped:
	case evHaltDelivered:
	case evStopProgress:
	case evStandstillConfirmed:
	case evRearm:
	}
	m.aud.Record(context.Background(), ev)
}

func (m *Module) reportDropped() {
	if n := m.ring.droppedCount(); n > 0 {
		m.aud.Record(context.Background(), audit.Event{
			Action:  "safety.audit_dropped",
			Subject: "kernel",
			Denied:  true,
			Detail:  fmt.Sprintf("dropped=%d", n),
		})
	}
}

// SafetySnapshot implements Observer: a consistent read of the top-level state, epoch and current stop phase.
//
// The phase is only reported when it belongs to the same latch round as the current Gate epoch; otherwise it's left over from the previous round, and
// StopPhaseUnspecified is reported (this round's Supervisor hasn't started tracking yet). That way the (epoch, phase) observers see
// is always self-consistent.
func (m *Module) SafetySnapshot() Snapshot {
	st, ep := m.gate.Snapshot()
	ph, phEpoch := unpackPhase(m.phase.Load())
	if phEpoch != ep {
		ph = PhaseUnspecified
	}
	return Snapshot{State: st, Epoch: ep, StopPhase: ph}
}

// RequestRecovery asks to enter the OEM recovery phase (SAFETY_LATCHED -> OEM_RECOVERY). Recovery actions
// can only request allowlisted actions through the dedicated Safety Gate and can't clear the latch; real allowlist dispatch waits for the Provider to land.
// Returns whether the transition succeeded.
func (m *Module) RequestRecovery() bool {
	ok := m.gate.BeginRecovery()
	wake(m.superWake)
	return ok
}

// RequireRearm requires an explicit re-arm before returning to NORMAL (-> REARM_REQUIRED).
func (m *Module) RequireRearm() bool {
	ok := m.gate.RequireRearm()
	wake(m.superWake)
	return ok
}

// Rearm performs an explicit re-arm (REARM_REQUIRED -> NORMAL and increment the epoch). Returns whether it succeeded.
//
// Here the kernel only re-checks the hard precondition that can't be bypassed: the stop progress must have settled, either the normal chain reached
// OUTPUT_DISABLED/STANDSTILL_CONFIRMED (output confirmed off) or it entered a terminal side branch such as DELIVERY_FAULT/
// STANDSTILL_TIMEOUT (which already needs human handling). Releasing the latch while the stop is still in progress
// (REQUESTED/SENT/PROVIDER_ACCEPTED/MCU_ACKED) would allow motion again without knowing whether the output
// has been switched off.
//
// Whether to re-arm right now is itself policy and isn't in the kernel: a system service decides after aggregating device state, risk and user
// policy, then calls this method. Mechanism in the kernel, policy in the service: the same division of labour as rest mode
// (an Agent can only request; after Policy checks, nervud's Authority performs a fixed, typed action).
func (m *Module) Rearm() bool {
	_, ep := m.gate.Snapshot()
	ph, phEpoch := unpackPhase(m.phase.Load())

	// The phase must belong to the current latch round (phEpoch == the current Gate epoch) and be settled. A phase left over from the previous
	// round is always treated as unsettled and refused. Otherwise this could happen: previous round OUTPUT_DISABLED -> Rearm ->
	// new Trip -> RequireRearm -> Rearm, where the last Rearm uses the old phase as its credential and releases the new latch
	// back to NORMAL, while this round's beginHalt/RevokeAll may not even have run. Binding the epoch is exactly what closes that.
	if phEpoch != ep || !rearmSettled(ph) {
		m.recordRearmRejected(ph, phEpoch, ep)
		return false
	}

	if !m.gate.Rearm() {
		// The hard precondition passed, but the state transition still failed: not currently REARM_REQUIRED, or a concurrent Trip rewrote
		// the state. "Refused re-arms must leave an audit" applies equally to this path
		m.recordRearmRejected(ph, phEpoch, ep)
		return false
	}
	wake(m.superWake)
	return true
}

// recordRearmRejected records a refused re-arm. It carries the phase and both epochs, so offline analysis can tell apart
// the three refusal causes: phase not settled, phase from an old round, and state transition failed.
func (m *Module) recordRearmRejected(ph StopPhase, phEpoch, gateEpoch uint64) {
	m.aud.Record(context.Background(), audit.Event{
		Action:  "safety.rearm_rejected",
		Subject: "kernel",
		Denied:  true,
		Detail:  fmt.Sprintf("phase=%s phase_epoch=%d gate_epoch=%d", ph, phEpoch, gateEpoch),
	})
}

// rearmSettled reports whether the stop progress has settled at a phase that allows re-arm (see the notes on Rearm).
//
// Note rank returns 0 for both the terminal side branches and UNSPECIFIED, so terminal states must be checked separately,
// not just by comparing ranks
func rearmSettled(p StopPhase) bool {
	return p.rank() >= PhaseOutputDisabled.rank() || p.isTerminalFault()
}
