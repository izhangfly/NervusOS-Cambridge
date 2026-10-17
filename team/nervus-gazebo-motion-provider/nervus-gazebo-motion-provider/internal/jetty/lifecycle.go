package jetty

import (
	"context"
	"time"

	"github.com/nervus-os/nervus-gazebo-provider/internal/gztransport"
	"github.com/nervus-os/nervus-gazebo-provider/internal/simulation"
)

func (b *Backend) Start(
	ctx context.Context,
	options simulation.StartOptions,
) (simulation.Status, error) {
	if b.config.Mode == simulation.ModeAttach {
		return simulation.Status{}, simulation.NewError(
			simulation.ReasonProcessExternallyManaged, "jetty.start", false,
		)
	}
	world := options.World
	if world == "" {
		world = b.currentTargetWorld()
	}
	if faults := b.checkWSLg(ctx); len(faults) > 0 {
		return simulation.Status{}, simulation.NewError(
			simulation.ReasonWSLgNotReady, "jetty.start", true,
		)
	}
	snapshot, err := b.process.Start(ctx, world)
	if err != nil {
		return simulation.Status{}, err
	}
	b.prepareTarget(world, snapshot)
	return b.Status(ctx)
}

func (b *Backend) Stop(ctx context.Context) (simulation.Status, error) {
	if b.config.Mode == simulation.ModeAttach {
		return simulation.Status{}, simulation.NewError(
			simulation.ReasonProcessExternallyManaged, "jetty.stop", false,
		)
	}
	snapshot, err := b.process.Stop(ctx)
	if err != nil {
		return simulation.Status{}, err
	}
	b.applyProcessSnapshot(snapshot, true)
	return b.Status(ctx)
}

func (b *Backend) LoadWorld(ctx context.Context, world string) (simulation.Status, error) {
	if b.config.Mode == simulation.ModeAttach {
		return simulation.Status{}, simulation.NewError(
			simulation.ReasonProcessExternallyManaged, "jetty.load_world", false,
		)
	}
	if !containsWorld(b.process.AllowedWorlds(), world) {
		return simulation.Status{}, simulation.NewError(
			simulation.ReasonWorldNotFound, "jetty.load_world", false,
		)
	}
	if faults := b.checkWSLg(ctx); len(faults) > 0 {
		return simulation.Status{}, simulation.NewError(
			simulation.ReasonWSLgNotReady, "jetty.load_world", true,
		)
	}
	current := b.process.Snapshot()
	if current.State == simulation.ProcessRunning || current.State == simulation.ProcessStarting ||
		current.State == simulation.ProcessStopping {
		if _, err := b.process.Stop(ctx); err != nil {
			return simulation.Status{}, err
		}
	}
	b.prepareTarget(world, b.process.Snapshot())
	snapshot, err := b.process.Start(ctx, world)
	if err != nil {
		return simulation.Status{}, err
	}
	b.prepareTarget(world, snapshot)
	return b.Status(ctx)
}

func (b *Backend) watchProcess() {
	defer b.wg.Done()
	for {
		select {
		case <-b.ctx.Done():
			return
		case event, ok := <-b.process.Events():
			if !ok {
				return
			}
			current := b.process.Snapshot()
			b.applyProcessSnapshot(current, current.State != simulation.ProcessRunning)
			if event.HasExit {
				event.Exit.WorldGeneration = b.worldGeneration()
				b.emitFault(simulation.Event{
					Kind: simulation.EventProcessExited, ProcessExit: event.Exit,
				})
			}
		}
	}
}

func (b *Backend) applyProcessSnapshot(snapshot simulation.ProcessSnapshot, resetTransport bool) {
	b.mu.Lock()
	b.status.Process = snapshot.State
	b.status.ProcessOwned = snapshot.Owned
	b.status.ManagedProcessID = snapshot.ProcessID
	b.status.HasLastExitCode = snapshot.HasLastExitCode
	b.status.LastExitCode = snapshot.LastExitCode
	if snapshot.State == simulation.ProcessExited {
		b.status.LastFault = simulation.ReasonProcessExited
	}
	var ids []gztransport.SubscriptionID
	if resetTransport && snapshot.State != simulation.ProcessRunning {
		ids = b.disconnectLocked(simulation.TransportDisconnected)
		b.cancelCycleLocked()
	}
	b.notifyLocked()
	status := b.status
	b.mu.Unlock()
	b.cleanupSubscriptions(ids)
	b.emitStatus(status)
	b.wakeProcess()
}

func (b *Backend) prepareTarget(world string, snapshot simulation.ProcessSnapshot) {
	b.mu.Lock()
	b.targetWorld = world
	b.status.Process = snapshot.State
	b.status.ProcessOwned = snapshot.Owned
	b.status.ManagedProcessID = snapshot.ProcessID
	b.status.HasLastExitCode = snapshot.HasLastExitCode
	b.status.LastExitCode = snapshot.LastExitCode
	ids := b.disconnectLocked(simulation.TransportDiscovering)
	b.cancelCycleLocked()
	b.notifyLocked()
	status := b.status
	b.mu.Unlock()
	b.cleanupSubscriptions(ids)
	b.emitStatus(status)
	b.wakeProcess()
}

func (b *Backend) disconnectLocked(transport simulation.TransportState) []gztransport.SubscriptionID {
	ids := make([]gztransport.SubscriptionID, 0, len(b.subscriptions))
	for id := range b.subscriptions {
		ids = append(ids, id)
	}
	b.subscriptions = make(map[gztransport.SubscriptionID]subscription)
	b.status.Transport = transport
	b.status.Run = simulation.RunUnspecified
	b.status.Statistics = simulation.Statistics{}
	b.scene = sceneSnapshot{}
	b.jointStates = make(map[string]simulation.JointState)
	b.pendingFeedback = make(map[string]uint64)
	b.feedbackModels = make(map[string]bool)
	b.lastStatsAt = time.Time{}
	return ids
}

func (b *Backend) cancelCycleLocked() {
	if b.cycleCancel != nil {
		b.cycleCancel()
		b.cycleCancel = nil
	}
}

func (b *Backend) currentTargetWorld() string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.targetWorld
}

func (b *Backend) wakeProcess() {
	select {
	case b.processWake <- struct{}{}:
	default:
	}
}

func containsWorld(worlds []string, target string) bool {
	for _, world := range worlds {
		if world == target {
			return true
		}
	}
	return false
}

func (b *Backend) monitorWSLg() {
	defer b.wg.Done()
	ticker := time.NewTicker(b.config.WSLgCheckInterval)
	defer ticker.Stop()
	b.checkWSLg(b.ctx)
	for {
		select {
		case <-b.ctx.Done():
			return
		case <-ticker.C:
			b.checkWSLg(b.ctx)
		}
	}
}

func (b *Backend) checkWSLg(ctx context.Context) []simulation.WSLgFaultEvent {
	if b.wslg == nil {
		return nil
	}
	faults := b.wslg.Check(ctx)
	current := make(map[simulation.WSLgFaultKind]struct{}, len(faults))
	generation := b.worldGeneration()
	for index := range faults {
		faults[index].WorldGeneration = generation
		current[faults[index].Kind] = struct{}{}
	}
	b.mu.Lock()
	previous := b.activeWSLgFaults
	b.activeWSLgFaults = current
	if len(current) > 0 {
		b.status.LastFault = simulation.ReasonWSLgNotReady
	} else if b.status.LastFault == simulation.ReasonWSLgNotReady {
		if b.status.ReadyForControl() {
			b.status.LastFault = simulation.ReasonUnspecified
		} else {
			b.status.LastFault = simulation.ReasonProviderNotReady
		}
	}
	status := b.status
	b.mu.Unlock()
	if len(current) != len(previous) {
		b.emitStatus(status)
	}
	for _, fault := range faults {
		if _, exists := previous[fault.Kind]; exists {
			continue
		}
		b.emitFault(simulation.Event{Kind: simulation.EventWSLgFault, WSLgFault: fault})
	}
	return faults
}
