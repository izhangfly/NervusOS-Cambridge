package jetty

import (
	"context"
	"math"
	"time"

	"github.com/nervus-os/nervus-gazebo-provider/internal/gzmsgs"
	"github.com/nervus-os/nervus-gazebo-provider/internal/gztransport"
	"github.com/nervus-os/nervus-gazebo-provider/internal/simulation"
	"google.golang.org/protobuf/proto"
)

var _ simulation.BaseMotionBackend = (*Backend)(nil)

func (b *Backend) BaseMotionState(_ context.Context, role string) (simulation.BaseMotionState, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	controller, ok := b.scene.baseControllers[role]
	if !ok {
		return simulation.BaseMotionState{}, simulation.NewError(simulation.ReasonFeedbackUnavailable, "jetty.base_state", true)
	}
	return simulation.BaseMotionState{
		WorldGeneration: b.status.WorldGeneration,
		Ready:           controller.hasFeedback && b.status.ReadyForControl(),
		Fault:           !b.status.ReadyForControl(),
		SupportsLinearX: true, SupportsLinearY: false, SupportsAngularZ: true,
		MaxLinear: controller.maxLinear, MaxAngular: controller.maxAngular,
		ActiveMotionEpoch: controller.activeMotionEpoch,
	}, nil
}

func (b *Backend) SetBaseVelocity(ctx context.Context, execution simulation.ControlContext, velocity simulation.BaseVelocity) error {
	if err := b.requireBaseControl(execution, false); err != nil {
		return err
	}
	if !finite(velocity.LinearX) || !finite(velocity.LinearY) || !finite(velocity.AngularZ) {
		return simulation.NewError(simulation.ReasonOutOfRange, "jetty.base_set_velocity", false)
	}
	b.mu.RLock()
	controller, ok := b.scene.baseControllers[execution.ResourceHandle]
	b.mu.RUnlock()
	if !ok || !controller.hasFeedback {
		return simulation.NewError(simulation.ReasonFeedbackUnavailable, "jetty.base_set_velocity", true)
	}
	if math.Abs(velocity.LinearY) > 1e-9 || math.Abs(velocity.LinearX) > controller.maxLinear || math.Abs(velocity.AngularZ) > controller.maxAngular {
		return simulation.NewError(simulation.ReasonOutOfRange, "jetty.base_set_velocity", false)
	}
	return b.publishBaseVelocity(ctx, execution.ResourceHandle, execution.MotionEpoch, velocity)
}

func (b *Backend) StopBaseMotion(ctx context.Context, execution simulation.ControlContext) error {
	if err := b.requireBaseControl(execution, true); err != nil {
		return err
	}
	if err := b.publishBaseVelocity(ctx, execution.ResourceHandle, execution.MotionEpoch, simulation.BaseVelocity{}); err != nil {
		return err
	}
	for {
		b.mu.RLock()
		controller, ok := b.scene.baseControllers[execution.ResourceHandle]
		signal := b.signal
		b.mu.RUnlock()
		if !ok {
			return simulation.NewError(simulation.ReasonTransportDisconnected, "jetty.base_stop", true)
		}
		if controller.hasFeedback && !controller.lastZeroAt.IsZero() && time.Since(controller.lastZeroAt) >= controller.settleTime {
			return nil
		}
		wait := time.NewTimer(25 * time.Millisecond)
		select {
		case <-signal:
			wait.Stop()
		case <-wait.C:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (b *Backend) publishBaseVelocity(ctx context.Context, role string, epoch uint64, velocity simulation.BaseVelocity) error {
	b.mu.RLock()
	controller, ok := b.scene.baseControllers[role]
	b.mu.RUnlock()
	if !ok {
		return simulation.NewError(simulation.ReasonFeedbackUnavailable, "jetty.base_publish", true)
	}
	payload, err := proto.Marshal(&gzmsgs.Twist{
		Linear:  &gzmsgs.Vector3D{X: velocity.LinearX, Y: velocity.LinearY},
		Angular: &gzmsgs.Vector3D{Z: velocity.AngularZ},
	})
	if err != nil {
		return simulation.NewError(simulation.ReasonNativeFailure, "jetty.base_encode", false)
	}
	if err := b.client.Publish(ctx, gztransport.Message{Topic: controller.commandTopic, Type: typeTwist, Data: payload}, gztransport.PublishOptions{RequireConnection: true, ConnectionTimeout: b.config.RequestTimeout}); err != nil {
		return transportError(err, "jetty.base_publish", simulation.ReasonTopicNotFound)
	}
	b.mu.Lock()
	if current, exists := b.scene.baseControllers[role]; exists {
		current.activeMotionEpoch = epoch
		current.linearX, current.linearY, current.angularZ = velocity.LinearX, velocity.LinearY, velocity.AngularZ
		if velocity.LinearX != 0 || velocity.LinearY != 0 || velocity.AngularZ != 0 {
			current.lastZeroAt = time.Time{}
		}
		b.scene.baseControllers[role] = current
		b.notifyLocked()
	}
	b.mu.Unlock()
	return nil
}

func (b *Backend) applyBaseOdometry(event gztransport.Event, role string) error {
	var message gzmsgs.Odometry
	if err := proto.Unmarshal(event.Data, &message); err != nil {
		return simulation.NewError(simulation.ReasonNativeFailure, "jetty.decode_odometry", false)
	}
	linear, angular := message.GetTwist().GetLinear(), message.GetTwist().GetAngular()
	if linear == nil || angular == nil || !finite(linear.GetX()) || !finite(linear.GetY()) || !finite(angular.GetZ()) {
		return simulation.NewError(simulation.ReasonNativeFailure, "jetty.decode_odometry", false)
	}
	b.mu.Lock()
	controller, ok := b.scene.baseControllers[role]
	if !ok {
		b.mu.Unlock()
		return nil
	}
	controller.linearX, controller.linearY, controller.angularZ = linear.GetX(), linear.GetY(), angular.GetZ()
	controller.hasFeedback = true
	if math.Abs(controller.linearX) <= controller.linearStopTolerance && math.Abs(controller.angularZ) <= controller.angularStopTolerance {
		if controller.lastZeroAt.IsZero() {
			controller.lastZeroAt = time.Now()
		}
	} else {
		controller.lastZeroAt = time.Time{}
	}
	b.scene.baseControllers[role] = controller
	becameReady := false
	for index := range b.scene.bindings {
		binding := &b.scene.bindings[index]
		if binding.ResourceRole == role && binding.Adapter == "diff_drive" && binding.State == simulation.MotionBindingDiscovering {
			binding.State = simulation.MotionBindingReady
			binding.Reason = ""
			becameReady = true
		}
	}
	b.notifyLocked()
	status := b.status
	b.mu.Unlock()
	if becameReady {
		b.emitStatus(status)
	}
	return nil
}

func (b *Backend) requireBaseControl(execution simulation.ControlContext, _ bool) error {
	if execution.ResourceHandle == "" || execution.WorldGeneration == 0 || execution.MotionEpoch == 0 || execution.CommandSequence == 0 {
		return simulation.NewError(simulation.ReasonWorldChanged, "jetty.base_control", false)
	}
	if err := b.requireWorld(execution.WorldGeneration); err != nil {
		return err
	}
	b.mu.RLock()
	controller, ok := b.scene.baseControllers[execution.ResourceHandle]
	b.mu.RUnlock()
	if !ok || !controller.hasFeedback {
		return simulation.NewError(simulation.ReasonFeedbackUnavailable, "jetty.base_control", true)
	}
	return nil
}

func finite(value float64) bool { return !math.IsNaN(value) && !math.IsInf(value, 0) }
