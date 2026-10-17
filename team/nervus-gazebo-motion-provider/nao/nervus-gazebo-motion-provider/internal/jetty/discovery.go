package jetty

import (
	"context"
	"errors"
	"time"

	"github.com/nervus-os/nervus-gazebo-provider/internal/gztransport"
	"github.com/nervus-os/nervus-gazebo-provider/internal/simulation"
)

func (b *Backend) run() {
	defer b.wg.Done()
	for {
		if b.ctx.Err() != nil {
			return
		}
		if !b.processReadyForDiscovery() {
			select {
			case <-b.processWake:
				continue
			case <-b.ctx.Done():
				return
			}
		}
		world := b.currentTargetWorld()
		cycle, cycleCancel := context.WithCancel(b.ctx)
		b.mu.Lock()
		if b.targetWorld != world {
			b.mu.Unlock()
			cycleCancel()
			continue
		}
		b.cycleCancel = cycleCancel
		b.mu.Unlock()
		b.markDiscovering()
		attempt, cancel := context.WithTimeout(cycle, b.config.DiscoveryTimeout)
		err := b.discover(attempt, world)
		cancel()
		if err != nil {
			cycleCancel()
			b.clearCycleCancel()
			if b.ctx.Err() != nil {
				return
			}
			if errors.Is(err, context.Canceled) {
				continue
			}
			reason := reasonFromError(err)
			if b.markInitialFailure(reason) {
				b.emitFault(simulation.Event{
					Kind: simulation.EventTransportFault,
					TransportFault: simulation.TransportFaultEvent{
						Kind: transportFaultKind(err), Reason: reason, Retryable: true,
						WorldGeneration: b.worldGeneration(),
					},
				})
			}
			b.log.Debug("jetty: discovery attempt failed", "world", world, "err", err)
			if !b.waitRetry() {
				return
			}
			continue
		}

		err = b.eventLoop(cycle)
		cycleCancel()
		b.clearCycleCancel()
		if b.ctx.Err() != nil {
			return
		}
		if errors.Is(err, context.Canceled) || !b.processReadyForDiscovery() {
			continue
		}
		if err == nil {
			err = simulation.NewError(
				simulation.ReasonTransportDisconnected, "jetty.events_closed", true,
			)
		}
		reason := reasonFromError(err)
		ids := b.markRuntimeFailure(reason)
		b.cleanupSubscriptions(ids)
		b.emitFault(simulation.Event{
			Kind: simulation.EventTransportFault,
			TransportFault: simulation.TransportFaultEvent{
				Kind:            transportFaultKind(err),
				Reason:          reason,
				Retryable:       true,
				WorldGeneration: b.worldGeneration(),
			},
		})
		b.log.Warn("jetty: Gazebo Transport lost; rediscovering", "err", err)
		if !b.waitRetry() {
			return
		}
	}
}

func (b *Backend) discover(ctx context.Context, world string) (err error) {
	// Capture subscriber-only controller topics before TopicInfo-based waits.
	// Jetty Transport 15 may stop returning those remote subscriber records
	// from TopicList after another TopicInfo call on the same Node.
	motionTopics, topicsErr := b.listMotionTopics(ctx)
	if err := b.client.WaitForService(
		ctx,
		controlService(world),
		typeWorldControl,
		typeBoolean,
	); err != nil {
		return transportError(err, "jetty.discover_control", simulation.ReasonServiceNotFound)
	}
	if err := b.client.WaitForService(
		ctx,
		sceneService(world),
		typeEmpty,
		typeScene,
	); err != nil {
		return transportError(err, "jetty.discover_scene", simulation.ReasonServiceNotFound)
	}
	if err := b.client.WaitForTopic(ctx, statsTopic(world), typeWorldStatistics); err != nil {
		return transportError(err, "jetty.discover_stats", simulation.ReasonTopicNotFound)
	}

	snapshot, err := b.requestSceneForWorld(ctx, world)
	if err != nil {
		return err
	}
	if topicsErr != nil {
		b.log.Debug("jetty: motion topic discovery unavailable", "err", topicsErr)
	}
	b.discoverProfileBindings(ctx, &snapshot, motionTopics, world)

	localSubscriptions := make(map[gztransport.SubscriptionID]subscription)
	committed := false
	defer func() {
		if committed {
			return
		}
		for id := range localSubscriptions {
			cleanup, cancel := context.WithTimeout(context.Background(), time.Second)
			_ = b.client.Unsubscribe(cleanup, id)
			cancel()
		}
	}()

	statsID, err := b.client.Subscribe(
		ctx,
		statsTopic(world),
		typeWorldStatistics,
		gztransport.SubscriptionOptions{MaxMessagesPerSecond: 10, CoalesceLatest: true},
	)
	if err != nil {
		return transportError(err, "jetty.subscribe_stats", simulation.ReasonTopicNotFound)
	}
	localSubscriptions[statsID] = subscription{kind: subscriptionStats}

	for _, source := range snapshot.stateSources {
		jointID, subscribeErr := b.client.Subscribe(
			ctx,
			source.topic,
			source.messageType,
			gztransport.SubscriptionOptions{MaxMessagesPerSecond: 20, CoalesceLatest: true},
		)
		if subscribeErr == nil {
			localSubscriptions[jointID] = subscription{kind: subscriptionJointState, model: source.model}
			continue
		}
		if !source.optional {
			b.degradeModelBindings(&snapshot, source.model, "state_subscription_failed")
		}
	}

	var firstStats decodedStats
	for {
		event, nextErr := b.client.NextEvent(ctx)
		if nextErr != nil {
			return transportError(nextErr, "jetty.first_stats", simulation.ReasonTopicNotFound)
		}
		if localSubscriptions[event.SubscriptionID].kind != subscriptionStats {
			continue
		}
		firstStats, err = decodeStats(event.Data)
		if err != nil {
			return err
		}
		break
	}

	b.activate(world, snapshot, localSubscriptions, firstStats)
	committed = true
	return nil
}

func (b *Backend) eventLoop(ctx context.Context) error {
	stream, err := gztransport.NewEventStream(ctx, b.client, 64)
	if err != nil {
		return err
	}
	defer stream.Close()
	interval := b.config.StatsStaleTimeout / 2
	if interval < 100*time.Millisecond {
		interval = 100 * time.Millisecond
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err, ok := <-stream.Errors():
			if !ok {
				return nil
			}
			return transportError(err, "jetty.events", simulation.ReasonTransportDisconnected)
		case event, ok := <-stream.Events():
			if !ok {
				return nil
			}
			if err := b.handleTransportEvent(event); err != nil {
				return err
			}
		case <-ticker.C:
			b.mu.RLock()
			last := b.lastStatsAt
			b.mu.RUnlock()
			if last.IsZero() || time.Since(last) > b.config.StatsStaleTimeout {
				return simulation.NewError(
					simulation.ReasonTransportDisconnected,
					"jetty.stats_stale",
					true,
				)
			}
		}
	}
}

func (b *Backend) handleTransportEvent(event gztransport.Event) error {
	b.mu.RLock()
	subscription := b.subscriptions[event.SubscriptionID]
	b.mu.RUnlock()
	switch subscription.kind {
	case subscriptionStats:
		stats, err := decodeStats(event.Data)
		if err != nil {
			return err
		}
		b.applyStats(stats)
	case subscriptionJointState:
		if err := b.applyJointState(event, subscription.model); err != nil {
			return err
		}
	}
	return nil
}

func (b *Backend) activate(
	world string,
	snapshot sceneSnapshot,
	subscriptions map[gztransport.SubscriptionID]subscription,
	stats decodedStats,
) {
	b.mu.Lock()
	previousWorld := b.status.WorldName
	previousGeneration := b.status.WorldGeneration
	b.scene = snapshot
	b.subscriptions = subscriptions
	b.feedbackModels = make(map[string]bool)
	for _, subscription := range subscriptions {
		if subscription.kind == subscriptionJointState && subscription.model != "" {
			b.feedbackModels[subscription.model] = true
		}
	}
	process := b.process.Snapshot()
	if b.config.Mode == simulation.ModeAttach {
		b.status.Process = simulation.ProcessExternal
	} else {
		b.status.Process = process.State
	}
	b.status.ProcessOwned = process.Owned
	b.status.ManagedProcessID = process.ProcessID
	b.status.HasLastExitCode = process.HasLastExitCode
	b.status.LastExitCode = process.LastExitCode
	b.status.Transport = simulation.TransportReady
	b.status.Run = stats.run
	b.status.WorldName = world
	b.status.WorldGeneration++
	if b.status.WorldGeneration == 0 {
		b.status.WorldGeneration = 1
	}
	for index := range b.scene.bindings {
		b.scene.bindings[index].WorldGeneration = b.status.WorldGeneration
	}
	if len(b.activeWSLgFaults) == 0 {
		b.status.LastFault = simulation.ReasonUnspecified
	}
	b.transportFaultActive = false
	b.transportFaultReason = simulation.ReasonUnspecified
	b.status.Statistics = stats.statistics
	b.lastStatsAt = time.Now()
	b.statsSequence++
	b.notifyLocked()
	status := b.status
	b.mu.Unlock()
	b.emitStatus(status)
	b.emitFault(simulation.Event{
		Kind: simulation.EventWorldChanged,
		WorldChanged: simulation.WorldChangedEvent{
			PreviousWorld: previousWorld, PreviousGeneration: previousGeneration,
			CurrentWorld: world, CurrentGeneration: status.WorldGeneration,
		},
	})
}

func (b *Backend) markDiscovering() {
	b.mu.Lock()
	b.status.Transport = simulation.TransportDiscovering
	if len(b.activeWSLgFaults) == 0 && !b.transportFaultActive {
		b.status.LastFault = simulation.ReasonProviderNotReady
	}
	b.notifyLocked()
	status := b.status
	b.mu.Unlock()
	b.emitStatus(status)
}

func (b *Backend) markInitialFailure(reason simulation.Reason) bool {
	b.mu.Lock()
	report := !b.transportFaultActive || b.transportFaultReason != reason
	b.transportFaultActive = true
	b.transportFaultReason = reason
	process := b.process.Snapshot()
	b.status.Process = process.State
	b.status.ProcessOwned = process.Owned
	b.status.ManagedProcessID = process.ProcessID
	b.status.HasLastExitCode = process.HasLastExitCode
	b.status.LastExitCode = process.LastExitCode
	b.status.Transport = simulation.TransportDisconnected
	b.status.Run = simulation.RunUnspecified
	b.status.LastFault = reason
	b.scene = sceneSnapshot{}
	b.feedbackModels = make(map[string]bool)
	b.lastStatsAt = time.Time{}
	b.notifyLocked()
	status := b.status
	b.mu.Unlock()
	b.emitStatus(status)
	return report
}

func (b *Backend) markRuntimeFailure(reason simulation.Reason) []gztransport.SubscriptionID {
	b.mu.Lock()
	ids := make([]gztransport.SubscriptionID, 0, len(b.subscriptions))
	for id := range b.subscriptions {
		ids = append(ids, id)
	}
	b.subscriptions = make(map[gztransport.SubscriptionID]subscription)
	b.status.Transport = simulation.TransportDegraded
	b.status.Run = simulation.RunUnspecified
	b.status.LastFault = reason
	b.transportFaultActive = true
	b.transportFaultReason = reason
	b.scene = sceneSnapshot{}
	b.jointStates = make(map[string]simulation.JointState)
	b.pendingFeedback = make(map[string]uint64)
	b.feedbackModels = make(map[string]bool)
	b.lastStatsAt = time.Time{}
	b.notifyLocked()
	status := b.status
	b.mu.Unlock()
	b.emitStatus(status)
	return ids
}

func (b *Backend) processReadyForDiscovery() bool {
	if b.config.Mode == simulation.ModeAttach {
		return true
	}
	snapshot := b.process.Snapshot()
	return snapshot.State == simulation.ProcessRunning && snapshot.Owned
}

func (b *Backend) clearCycleCancel() {
	b.mu.Lock()
	b.cycleCancel = nil
	b.mu.Unlock()
}

func (b *Backend) waitRetry() bool {
	timer := time.NewTimer(b.config.DiscoveryRetryInterval)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-b.processWake:
		return true
	case <-b.ctx.Done():
		return false
	}
}

func (b *Backend) cleanupSubscriptions(ids []gztransport.SubscriptionID) {
	for _, id := range ids {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		err := b.client.Unsubscribe(ctx, id)
		cancel()
		if err != nil && !errors.Is(err, gztransport.ErrNotFound) &&
			!errors.Is(err, gztransport.ErrClosed) {
			b.log.Debug("jetty: unsubscribe after Transport loss", "id", id, "err", err)
		}
	}
}

func (b *Backend) worldGeneration() uint64 {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.status.WorldGeneration
}

func reasonFromError(err error) simulation.Reason {
	var domain *simulation.Error
	if errors.As(err, &domain) {
		return domain.Reason
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return simulation.ReasonRequestTimeout
	}
	return simulation.ReasonNativeFailure
}

func transportFaultKind(err error) simulation.TransportFaultKind {
	var domain *simulation.Error
	if errors.As(err, &domain) {
		switch domain.Reason {
		case simulation.ReasonTypeMismatch:
			return simulation.TransportFaultTypeMismatch
		case simulation.ReasonRequestTimeout:
			return simulation.TransportFaultRequestTimeout
		case simulation.ReasonRemoteRejected:
			return simulation.TransportFaultRemoteRejected
		case simulation.ReasonQueueOverflow:
			return simulation.TransportFaultQueueOverflow
		case simulation.ReasonTransportDisconnected:
			return simulation.TransportFaultDisconnected
		}
	}
	return simulation.TransportFaultNativeFailure
}
