package jetty

import (
	"context"
	"errors"
	"math"
	"sort"
	"time"

	"github.com/nervus-os/nervus-gazebo-provider/internal/gzmsgs"
	"github.com/nervus-os/nervus-gazebo-provider/internal/gztransport"
	"github.com/nervus-os/nervus-gazebo-provider/internal/motion"
	"github.com/nervus-os/nervus-gazebo-provider/internal/simulation"
	"google.golang.org/protobuf/proto"
)

const trajectoryUpdateInterval = 20 * time.Millisecond

var _ simulation.JointMotionBackend = (*Backend)(nil)

func (b *Backend) StartJointTrajectory(
	ctx context.Context,
	execution simulation.ControlContext,
	model, groupRole string,
	points []simulation.JointTrajectoryPoint,
) (simulation.JointMotionReceipt, error) {
	if err := b.requireControl(execution); err != nil {
		return simulation.JointMotionReceipt{}, err
	}
	group, err := b.trajectoryGroup(model, groupRole)
	if err != nil {
		return simulation.JointMotionReceipt{}, err
	}
	trajectory := make([]motion.TrajectoryPoint, len(points))
	for index, point := range points {
		trajectory[index] = motion.TrajectoryPoint{
			Positions:     append([]float64(nil), point.Positions...),
			TimeFromStart: time.Duration(point.TimeFromStart),
		}
	}
	return b.startJointMotion(ctx, execution, group, trajectory)
}

func (b *Backend) StartJointHome(
	ctx context.Context,
	execution simulation.ControlContext,
	model, groupRole, homeRole string,
) (simulation.JointMotionReceipt, error) {
	if err := b.requireControl(execution); err != nil {
		return simulation.JointMotionReceipt{}, err
	}
	group, err := b.trajectoryGroup(model, groupRole)
	if err != nil {
		return simulation.JointMotionReceipt{}, err
	}
	b.mu.RLock()
	home := b.scene.homeStates[trajectoryHomeKey(model, homeRole)]
	b.mu.RUnlock()
	if len(home) == 0 {
		return simulation.JointMotionReceipt{}, simulation.NewError(
			simulation.ReasonInvalidState, "jetty.joint_home_not_configured", false,
		)
	}
	initial, _, err := b.readJointSamples(ctx, execution.WorldGeneration, group.joints)
	if err != nil {
		return simulation.JointMotionReceipt{}, err
	}
	target := make([]float64, len(group.joints))
	duration := 500 * time.Millisecond
	for index, joint := range group.joints {
		value, ok := home[joint]
		if !ok {
			return simulation.JointMotionReceipt{}, simulation.NewError(
				simulation.ReasonInvalidState, "jetty.joint_home_group_mismatch", false,
			)
		}
		target[index] = value
		needed := time.Duration(math.Abs(value-initial[index]) / group.limits[index].MaxVelocity * float64(time.Second))
		if needed > duration {
			duration = needed
		}
	}
	return b.startJointMotionWithInitial(
		execution,
		group,
		initial,
		[]motion.TrajectoryPoint{{Positions: target, TimeFromStart: duration}},
	)
}

func (b *Backend) WaitJointMotion(
	ctx context.Context,
	motionID uint64,
) (simulation.JointMotionReceipt, error) {
	result, err := b.jointMotion.Wait(ctx, motionID)
	if err != nil {
		return simulation.JointMotionReceipt{}, err
	}
	receipt := simulation.JointMotionReceipt{
		MotionID: motionID, ControlledJoints: uint32(result.ControlledJoints),
		Accepted: true, FeedbackAvailable: true, Converged: result.Converged,
	}
	if result.Err != nil {
		return receipt, result.Err
	}
	return receipt, nil
}

func (b *Backend) CancelJointMotion(
	ctx context.Context,
	execution simulation.ControlContext,
	model, groupRole string,
) (simulation.JointMotionReceipt, error) {
	if err := b.requireControl(execution); err != nil {
		return simulation.JointMotionReceipt{}, err
	}
	group, err := b.trajectoryGroup(model, groupRole)
	if err != nil {
		return simulation.JointMotionReceipt{}, err
	}
	if _, err := b.jointMotion.CancelJointsAndWait(ctx, group.joints); err != nil {
		return simulation.JointMotionReceipt{}, err
	}
	positions, _, err := b.readJointSamples(ctx, execution.WorldGeneration, group.joints)
	if err != nil {
		return simulation.JointMotionReceipt{}, err
	}
	if err := b.publishTrajectoryTargets(ctx, execution.WorldGeneration, group.joints, positions); err != nil {
		return simulation.JointMotionReceipt{}, err
	}
	if err := b.waitJointsStopped(ctx, execution.WorldGeneration, group.joints, group.settleTime); err != nil {
		return simulation.JointMotionReceipt{}, err
	}
	return simulation.JointMotionReceipt{
		ControlledJoints: uint32(len(group.joints)), Accepted: true,
		FeedbackAvailable: true, Converged: true,
	}, nil
}

func (b *Backend) StopAllMotion(
	ctx context.Context,
	execution simulation.ControlContext,
) (simulation.JointMotionReceipt, error) {
	if err := b.requireControl(execution); err != nil {
		return simulation.JointMotionReceipt{}, err
	}
	if _, err := b.jointMotion.CancelAllAndWait(ctx); err != nil {
		return simulation.JointMotionReceipt{}, err
	}

	b.mu.RLock()
	unique := make(map[motion.JointRef]struct{})
	settle := 250 * time.Millisecond
	for _, group := range b.scene.trajectoryGroups {
		if group.settleTime > settle {
			settle = group.settleTime
		}
		for _, joint := range group.joints {
			unique[joint] = struct{}{}
		}
	}
	baseRoles := make([]string, 0, len(b.scene.baseControllers))
	for role := range b.scene.baseControllers {
		baseRoles = append(baseRoles, role)
	}
	b.mu.RUnlock()

	joints := make([]motion.JointRef, 0, len(unique))
	for joint := range unique {
		joints = append(joints, joint)
	}
	sort.Slice(joints, func(i, j int) bool {
		if joints[i].Model != joints[j].Model {
			return joints[i].Model < joints[j].Model
		}
		if joints[i].Joint != joints[j].Joint {
			return joints[i].Joint < joints[j].Joint
		}
		return joints[i].Axis < joints[j].Axis
	})
	if len(joints) > 0 {
		positions, _, err := b.readJointSamples(ctx, execution.WorldGeneration, joints)
		if err != nil {
			return simulation.JointMotionReceipt{}, err
		}
		if err := b.publishTrajectoryTargets(ctx, execution.WorldGeneration, joints, positions); err != nil {
			return simulation.JointMotionReceipt{}, err
		}
	}
	for _, role := range baseRoles {
		if err := b.publishBaseVelocity(ctx, role, execution.MotionEpoch, simulation.BaseVelocity{}); err != nil {
			return simulation.JointMotionReceipt{}, err
		}
	}
	if len(joints) > 0 {
		if err := b.waitJointsStopped(ctx, execution.WorldGeneration, joints, settle); err != nil {
			return simulation.JointMotionReceipt{}, err
		}
	}
	if err := b.waitBasesStopped(ctx, baseRoles); err != nil {
		return simulation.JointMotionReceipt{}, err
	}
	return simulation.JointMotionReceipt{
		ControlledJoints: uint32(len(joints)), Accepted: true,
		FeedbackAvailable: true, Converged: true,
	}, nil
}

func (b *Backend) startJointMotion(
	ctx context.Context,
	execution simulation.ControlContext,
	group trajectoryGroup,
	points []motion.TrajectoryPoint,
) (simulation.JointMotionReceipt, error) {
	initial, _, err := b.readJointSamples(ctx, execution.WorldGeneration, group.joints)
	if err != nil {
		return simulation.JointMotionReceipt{}, err
	}
	return b.startJointMotionWithInitial(execution, group, initial, points)
}

func (b *Backend) startJointMotionWithInitial(
	execution simulation.ControlContext,
	group trajectoryGroup,
	initial []float64,
	points []motion.TrajectoryPoint,
) (simulation.JointMotionReceipt, error) {
	refs := append([]motion.JointRef(nil), group.joints...)
	plan := motion.Plan{
		Generation: execution.WorldGeneration,
		Joints:     refs,
		Initial:    append([]float64(nil), initial...),
		Points:     points,
		Limits:     append([]motion.JointLimit(nil), group.limits...),
		Interval:   trajectoryUpdateInterval,
		Settle:     group.settleTime,
		Publish: func(ctx context.Context, positions []float64) error {
			return b.publishTrajectoryTargets(ctx, execution.WorldGeneration, refs, positions)
		},
		Read: func(ctx context.Context) ([]float64, error) {
			positions, _, err := b.readJointSamples(ctx, execution.WorldGeneration, refs)
			return positions, err
		},
	}
	b.jointCommandMu.Lock()
	motionID, err := b.jointMotion.Start(plan)
	b.jointCommandMu.Unlock()
	if err != nil {
		if errors.Is(err, motion.ErrJointBusy) {
			return simulation.JointMotionReceipt{}, simulation.NewError(
				simulation.ReasonInvalidState, "jetty.joint_command_source_busy", false,
			)
		}
		return simulation.JointMotionReceipt{}, simulation.NewError(
			simulation.ReasonOutOfRange, "jetty.joint_trajectory_invalid", false,
		)
	}
	return simulation.JointMotionReceipt{
		MotionID: motionID, ControlledJoints: uint32(len(refs)), Accepted: true,
		FeedbackAvailable: true,
	}, nil
}

func (b *Backend) trajectoryGroup(model, role string) (trajectoryGroup, error) {
	if !validName(model) || !validName(role) {
		return trajectoryGroup{}, simulation.NewError(
			simulation.ReasonInvalidState, "jetty.joint_group_invalid", false,
		)
	}
	b.mu.RLock()
	group, ok := b.scene.trajectoryGroups[trajectoryGroupKey(model, role)]
	b.mu.RUnlock()
	if !ok {
		return trajectoryGroup{}, simulation.NewError(
			simulation.ReasonJointNotControllable, "jetty.joint_group_not_configured", false,
		)
	}
	group.joints = append([]motion.JointRef(nil), group.joints...)
	group.limits = append([]motion.JointLimit(nil), group.limits...)
	return group, nil
}

func (b *Backend) publishTrajectoryTargets(
	ctx context.Context,
	generation uint64,
	joints []motion.JointRef,
	positions []float64,
) error {
	b.jointCommandMu.Lock()
	defer b.jointCommandMu.Unlock()
	if len(joints) == 0 || len(joints) != len(positions) {
		return simulation.NewError(simulation.ReasonOutOfRange, "jetty.joint_trajectory_publish", false)
	}
	if err := b.requireWorld(generation); err != nil {
		return err
	}
	topics := make([]string, len(joints))
	b.mu.RLock()
	for index, joint := range joints {
		controller, ok := b.scene.controllers[jointKey(joint.Model, joint.Joint, joint.Axis)]
		if !ok || !validateJointLimit(controller.joint, joint.Axis, positions[index]) {
			b.mu.RUnlock()
			return simulation.NewError(simulation.ReasonJointNotControllable, "jetty.joint_trajectory_publish", false)
		}
		topics[index] = controller.topic
	}
	b.mu.RUnlock()
	for index, topic := range topics {
		payload, err := proto.Marshal(&gzmsgs.Double{Data: positions[index]})
		if err != nil {
			return err
		}
		if err := b.client.Publish(ctx, gztransport.Message{Topic: topic, Type: typeDouble, Data: payload}, gztransport.PublishOptions{
			RequireConnection: true, ConnectionTimeout: b.config.RequestTimeout,
		}); err != nil {
			return transportError(err, "jetty.joint_trajectory_publish", simulation.ReasonTopicNotFound)
		}
	}
	return nil
}

func (b *Backend) readJointSamples(
	ctx context.Context,
	generation uint64,
	joints []motion.JointRef,
) ([]float64, []float64, error) {
	positions := make([]float64, len(joints))
	velocities := make([]float64, len(joints))
	for index, joint := range joints {
		state, err := b.JointState(ctx, simulation.JointQuery{
			WorldGeneration: generation, Model: joint.Model, Joint: joint.Joint, Axis: joint.Axis,
		})
		if err != nil {
			return nil, nil, err
		}
		if !state.HasPosition || !state.HasVelocity || !finite(state.Position) || !finite(state.Velocity) {
			return nil, nil, simulation.NewError(
				simulation.ReasonFeedbackUnavailable, "jetty.joint_trajectory_feedback", true,
			)
		}
		positions[index], velocities[index] = state.Position, state.Velocity
	}
	return positions, velocities, nil
}

func (b *Backend) waitJointsStopped(
	ctx context.Context,
	generation uint64,
	joints []motion.JointRef,
	settle time.Duration,
) error {
	var stoppedAt time.Time
	ticker := time.NewTicker(trajectoryUpdateInterval)
	defer ticker.Stop()
	for {
		_, velocities, err := b.readJointSamples(ctx, generation, joints)
		if err != nil {
			return err
		}
		stopped := true
		for _, velocity := range velocities {
			if math.Abs(velocity) > 0.02 {
				stopped = false
				break
			}
		}
		now := time.Now()
		if stopped {
			if stoppedAt.IsZero() {
				stoppedAt = now
			}
			if now.Sub(stoppedAt) >= settle {
				return nil
			}
		} else {
			stoppedAt = time.Time{}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (b *Backend) waitBasesStopped(ctx context.Context, roles []string) error {
	for {
		allStopped := true
		b.mu.RLock()
		for _, role := range roles {
			controller, ok := b.scene.baseControllers[role]
			if !ok || !controller.hasFeedback || controller.lastZeroAt.IsZero() ||
				time.Since(controller.lastZeroAt) < controller.settleTime {
				allStopped = false
				break
			}
		}
		signal := b.signal
		b.mu.RUnlock()
		if allStopped {
			return nil
		}
		select {
		case <-signal:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}
