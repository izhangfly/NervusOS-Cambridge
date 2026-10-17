package jetty

import (
	"context"

	"github.com/nervus-os/nervus-gazebo-provider/internal/gzmsgs"
	"github.com/nervus-os/nervus-gazebo-provider/internal/gztransport"
	"github.com/nervus-os/nervus-gazebo-provider/internal/simulation"
	"google.golang.org/protobuf/proto"
)

func (b *Backend) Pause(
	ctx context.Context,
	execution simulation.ControlContext,
) (simulation.Status, error) {
	if err := b.requireControl(execution); err != nil {
		return simulation.Status{}, err
	}
	_, baseline := b.statusSequence()
	if err := b.requestControl(ctx, &gzmsgs.WorldControl{Pause: true}, "jetty.pause"); err != nil {
		return simulation.Status{}, err
	}
	return b.waitForStatus(ctx, baseline, func(status simulation.Status) bool {
		return status.Run == simulation.RunPaused
	})
}

func (b *Backend) Resume(
	ctx context.Context,
	execution simulation.ControlContext,
) (simulation.Status, error) {
	if err := b.requireControl(execution); err != nil {
		return simulation.Status{}, err
	}
	_, baseline := b.statusSequence()
	if err := b.requestControl(ctx, &gzmsgs.WorldControl{Pause: false}, "jetty.resume"); err != nil {
		return simulation.Status{}, err
	}
	return b.waitForStatus(ctx, baseline, func(status simulation.Status) bool {
		return status.Run == simulation.RunRunning
	})
}

func (b *Backend) Step(
	ctx context.Context,
	execution simulation.ControlContext,
	iterations uint32,
) (simulation.Status, error) {
	if err := b.requireControl(execution); err != nil {
		return simulation.Status{}, err
	}
	if iterations == 0 || iterations > b.config.MaxStepIterations {
		return simulation.Status{}, simulation.NewError(
			simulation.ReasonOutOfRange,
			"jetty.step",
			false,
		)
	}
	status, baseline := b.statusSequence()
	if status.Run != simulation.RunPaused {
		return simulation.Status{}, simulation.NewError(
			simulation.ReasonInvalidState,
			"jetty.step",
			false,
		)
	}
	if status.Statistics.Iterations > ^uint64(0)-uint64(iterations) {
		return simulation.Status{}, simulation.NewError(
			simulation.ReasonOutOfRange,
			"jetty.step",
			false,
		)
	}
	target := status.Statistics.Iterations + uint64(iterations)
	if err := b.requestControl(
		ctx,
		&gzmsgs.WorldControl{Pause: true, MultiStep: iterations},
		"jetty.step",
	); err != nil {
		return simulation.Status{}, err
	}
	return b.waitForStatus(ctx, baseline, func(current simulation.Status) bool {
		return current.Run == simulation.RunPaused && current.Statistics.Iterations >= target
	})
}

func (b *Backend) ResetWorld(
	ctx context.Context,
	execution simulation.ControlContext,
	mode simulation.ResetMode,
) (simulation.Status, error) {
	if err := b.requireControl(execution); err != nil {
		return simulation.Status{}, err
	}
	reset := &gzmsgs.WorldReset{}
	switch mode {
	case simulation.ResetAll:
		reset.All = true
	case simulation.ResetTimeOnly:
		reset.TimeOnly = true
	case simulation.ResetModelsOnly:
		reset.ModelOnly = true
	default:
		return simulation.Status{}, simulation.NewError(
			simulation.ReasonOutOfRange,
			"jetty.reset_world",
			false,
		)
	}
	_, baseline := b.statusSequence()
	if err := b.requestControl(
		ctx,
		&gzmsgs.WorldControl{Reset_: reset},
		"jetty.reset_world",
	); err != nil {
		return simulation.Status{}, err
	}
	return b.waitForStatus(ctx, baseline, func(simulation.Status) bool { return true })
}

func (b *Backend) SetJointPositions(
	ctx context.Context,
	execution simulation.ControlContext,
	commands []simulation.JointCommand,
) (simulation.CommandReceipt, error) {
	if err := b.requireControl(execution); err != nil {
		return simulation.CommandReceipt{}, err
	}
	if len(commands) == 0 || uint32(len(commands)) > b.config.MaxJointCommands {
		return simulation.CommandReceipt{}, simulation.NewError(
			simulation.ReasonBatchTooLarge,
			"jetty.set_joint_positions",
			false,
		)
	}

	type submission struct {
		command simulation.JointCommand
		topic   string
	}
	submissions := make([]submission, 0, len(commands))
	b.mu.RLock()
	feedback := true
	for _, command := range commands {
		if !b.feedbackModels[command.Model] {
			feedback = false
		}
		key := jointKey(command.Model, command.Joint, command.Axis)
		item, ok := b.scene.controllers[key]
		if !ok {
			b.mu.RUnlock()
			return simulation.CommandReceipt{}, b.jointCommandError(command)
		}
		if !validateJointLimit(item.joint, command.Axis, command.Position) {
			b.mu.RUnlock()
			return simulation.CommandReceipt{}, simulation.NewError(
				simulation.ReasonOutOfRange,
				"jetty.set_joint_positions",
				false,
			)
		}
		submissions = append(submissions, submission{command: command, topic: item.topic})
	}
	b.mu.RUnlock()

	if feedback {
		b.mu.Lock()
		for _, item := range submissions {
			b.pendingFeedback[jointKey(
				item.command.Model,
				item.command.Joint,
				item.command.Axis,
			)] = execution.CommandSequence
		}
		b.mu.Unlock()
	}
	submitted := false
	defer func() {
		if !feedback || submitted {
			return
		}
		b.mu.Lock()
		for _, item := range submissions {
			delete(b.pendingFeedback, jointKey(
				item.command.Model,
				item.command.Joint,
				item.command.Axis,
			))
		}
		b.mu.Unlock()
	}()
	for _, item := range submissions {
		payload, err := proto.Marshal(&gzmsgs.Double{Data: item.command.Position})
		if err != nil {
			return simulation.CommandReceipt{}, err
		}
		if err := b.client.Publish(ctx, gztransport.Message{
			Topic: item.topic,
			Type:  typeDouble,
			Data:  payload,
		}, gztransport.PublishOptions{
			RequireConnection: true,
			ConnectionTimeout: b.config.RequestTimeout,
		}); err != nil {
			return simulation.CommandReceipt{}, transportError(
				err,
				"jetty.set_joint_positions",
				simulation.ReasonTopicNotFound,
			)
		}
	}
	submitted = true
	return simulation.CommandReceipt{
		SubmittedCommands: uint32(len(submissions)),
		FeedbackAvailable: feedback,
	}, nil
}

func (b *Backend) requestControl(
	ctx context.Context,
	request *gzmsgs.WorldControl,
	operation string,
) error {
	b.mu.RLock()
	world := b.status.WorldName
	b.mu.RUnlock()
	if world == "" {
		return simulation.NewError(simulation.ReasonWorldNotReady, operation, true)
	}
	payload, err := proto.Marshal(request)
	if err != nil {
		return err
	}
	wire, err := b.client.Request(ctx, gztransport.ServiceRequest{
		Service:      controlService(world),
		RequestType:  typeWorldControl,
		ResponseType: typeBoolean,
		Data:         payload,
		Timeout:      b.config.RequestTimeout,
	})
	if err != nil {
		return transportError(err, operation, simulation.ReasonServiceNotFound)
	}
	var response gzmsgs.Boolean
	if err := proto.Unmarshal(wire, &response); err != nil {
		return simulation.NewError(simulation.ReasonNativeFailure, operation, false)
	}
	if !response.GetData() {
		return simulation.NewError(simulation.ReasonRemoteRejected, operation, false)
	}
	return nil
}

func (b *Backend) requireControl(execution simulation.ControlContext) error {
	if execution.WorldGeneration == 0 {
		return simulation.NewError(simulation.ReasonWorldChanged, "jetty.control", false)
	}
	return b.requireWorld(execution.WorldGeneration)
}

func (b *Backend) statusSequence() (simulation.Status, uint64) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.status, b.statsSequence
}

func (b *Backend) waitForStatus(
	ctx context.Context,
	baseline uint64,
	predicate func(simulation.Status) bool,
) (simulation.Status, error) {
	for {
		b.mu.RLock()
		status := b.status
		sequence := b.statsSequence
		signal := b.signal
		b.mu.RUnlock()
		if !status.ReadyForControl() {
			return simulation.Status{}, simulation.NewError(
				simulation.ReasonTransportDisconnected,
				"jetty.wait_status",
				true,
			)
		}
		if sequence > baseline && predicate(status) {
			return status, nil
		}
		select {
		case <-signal:
		case <-ctx.Done():
			return simulation.Status{}, ctx.Err()
		}
	}
}

func (b *Backend) jointCommandError(command simulation.JointCommand) error {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if command.Axis != 0 {
		return simulation.NewError(
			simulation.ReasonAxisNotSupported,
			"jetty.set_joint_positions",
			false,
		)
	}
	if _, exists := b.scene.joints[command.Model]; !exists {
		return simulation.NewError(
			simulation.ReasonModelNotFound,
			"jetty.set_joint_positions",
			false,
		)
	}
	if _, exists := findJoint(b.scene.joints[command.Model], command.Joint); !exists {
		return simulation.NewError(
			simulation.ReasonJointNotFound,
			"jetty.set_joint_positions",
			false,
		)
	}
	return simulation.NewError(
		simulation.ReasonJointNotControllable,
		"jetty.set_joint_positions",
		false,
	)
}
