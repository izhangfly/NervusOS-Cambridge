// This file is the loop of the Control Lane (SCHED_RR / scheduler.PrioControl = 40): it only does lease
// expiry detection and revocation accounting, and sends no commands.
//
// Why expiry detection takes a real-time Lane: the deadman is a watchdog that must take control away the moment a person lets go;
// if ordinary load delays noticing by tens of milliseconds, the robot moves that much further. Once command dispatch lands it will reuse
// this same Lane (that's what the comment on scheduler.PrioControl reserves it for).
//
// Note this Lane isn't bound by the Safety Stop Lane's zero-heap-allocation hard rule: that rule targets the emergency stop delivery path,
// this Lane isn't on that path, and priority 40 is below PREEMPT_RT's threaded IRQ at 50, so by
// internal/scheduler's reasoning it may wait for I/O and can write audit right here.
package control

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/nervus-os/nervud/internal/audit"
)

// runLane is the Control Lane's main loop: it waits only on the ticker and the stop signal, doing no arbitrary I/O
func (m *Module) runLane(ctx context.Context) {
	ticker := time.NewTicker(laneTick)
	defer ticker.Stop()

	for {
		select {
		case <-m.stopCh:
			return
		case <-ctx.Done():
			return
		case t := <-ticker.C:
			m.onTick(t)
		}
	}
}

// onTick checks once for expiries and revocations waiting to be accounted for.
//
// When idle (no leases, no pending revocations) it takes no lock at all: it reads one immutable slot snapshot and does an atomic Load per slot.
// mu can be held by normal-priority control-plane paths, and this Lane grabbing it every 10ms would be pure priority inversion.
func (m *Module) onTick(now time.Time) {
	if m.revPending.Load() != 0 {
		m.drainRevoked()
	}

	for _, slot := range m.slots.Load().all {
		l := slot.cur.Load()
		if l == nil {
			continue
		}
		cause := m.leaseState(slot, l, now)
		if cause == nil {
			continue
		}
		// Safety has latched or the epoch has been revoked: that boundary belongs to safety, which has already incremented the epoch,
		// and RevokeAll will collect the lease. Don't rush to handle it here, or the epoch would be incremented once too often.
		if errors.Is(cause, ErrSafetyLatched) || errors.Is(cause, ErrStaleEpoch) {
			continue
		}

		// Only take the lock if it really expired, and re-check after taking it. Another slot's boundary may have replaced this slot with a new pointer,
		// and Refresh may have just refreshed this slot's freshness; neither case may be revoked by mistake.
		m.mu.Lock()
		cur := slot.cur.Load()
		if cur == l {
			cause = m.leaseState(slot, l, now)
			if cause != nil &&
				!errors.Is(cause, ErrSafetyLatched) && !errors.Is(cause, ErrStaleEpoch) {
				m.dropLocked(slot, l, actionFor(cause), cause)
			}
		}
		m.mu.Unlock()
	}
}

// actionFor maps the lapse reason to an audit Action, so deadman lapses and plain lease expiries can be told apart
// in offline analysis. Their product meanings are completely different: one is a lost link / person, the other a normal time limit
func actionFor(cause error) string {
	if errors.Is(cause, ErrDeadmanExpired) {
		return actionDeadmanExpired
	}
	return actionExpired
}

// drainRevoked writes the revocation audit RevokeAll handed over, draining every slot's pending position.
//
// RevokeAll runs on the Safety Supervisor Lane (FIFO 90) and only does atomic writes; moving string formatting and
// audit writing here keeps a high-priority safety path from waiting on normal-priority audit infrastructure.
//
// The Control Lane, Acquire (the ordering barrier before the next lease is published) and Stop can all consume; revDrainMu
// serialises them. The lock exists only on the normal-priority drain path; RevokeAll doesn't take it.
func (m *Module) drainRevoked() {
	m.revDrainMu.Lock()
	defer m.revDrainMu.Unlock()

	for _, slot := range m.slots.Load().all {
		l := slot.revoked.Swap(nil)
		if l == nil {
			continue
		}
		epoch := slot.revokedEpoch.Load()
		m.notifyLeaseEnded(l)
		m.aud.Record(context.Background(), audit.Event{
			Action:  "control." + actionRevoked,
			Subject: l.Owner.String(),
			Denied:  true,
			Err:     errSafetyRevoked,
			Detail: fmt.Sprintf("lease=%s class=%s resource=%s epoch=%d halt_epoch=%d",
				l.ID, l.Class, l.Resource, l.Epoch, epoch),
		})
		// pending covers the whole observer+audit boundary; when Acquire sees it non-zero it waits for revDrainMu,
		// so a late old ControlLeaseEnded can't hit state derived from a new lease on the same conn/resource.
		m.revPending.Add(^uint64(0))
	}
}
