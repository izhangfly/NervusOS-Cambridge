package safety

import "context"

// runStopLane is the Emergency Stop Lane (FIFO 95) loop. It normally blocks on wake; once woken it
// delivers the fixed stop signal once and goes straight back to blocking.
//
// Exit: it mainly listens to the module's stopCh (closed by Stop -> exits cleanly when modules stop in reverse, per the kernel.Module
// contract that loops exit only on Stop); ctx (the Scheduler's own ctx) is only a backstop, cancelled at sched.Shutdown,
// later than the module's Stop. Exiting through ctx while stopCh isn't closed is judged abnormal by laneFn and reported as Fatal.
func (m *Module) runStopLane(ctx context.Context) {
	for {
		select {
		case <-m.stopCh:
			return
		case <-ctx.Done():
			return
		case <-m.wake:
			m.deliverHalt()
		}
	}
}

// deliverHalt is the Stop Lane's hot path: read the current epoch and the latest reason, deliver the fixed stop signal over the Provider's high-priority
// Safety Path, write one fixed event code to the audit ring, then return.
//
// Zero heap allocations: Epoch/Load are atomic reads, NopPath.SendHalt doesn't allocate, ring.push doesn't allocate. No
// log formatting / Protobuf encoding / ordinary I/O here.
func (m *Module) deliverHalt() {
	epoch := m.gate.Epoch()
	reason := ReasonCode(m.pendingReason.Load())
	if err := m.path.SendHalt(epoch, reason); err != nil {
		m.ring.push(eventRecord{kind: evDeliveryFault, reason: reason, phase: PhaseSent, epoch: epoch})
		return
	}
	m.ring.push(eventRecord{kind: evHaltDelivered, reason: reason, phase: PhaseSent, epoch: epoch})
}
