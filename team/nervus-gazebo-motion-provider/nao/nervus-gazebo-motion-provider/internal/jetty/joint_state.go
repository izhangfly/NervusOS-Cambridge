package jetty

import (
	"context"

	"github.com/nervus-os/nervus-gazebo-provider/internal/gzmsgs"
	"github.com/nervus-os/nervus-gazebo-provider/internal/gztransport"
	"github.com/nervus-os/nervus-gazebo-provider/internal/simulation"
	"google.golang.org/protobuf/proto"
)

func (b *Backend) applyJointState(event gztransport.Event, expectedModel string) error {
	var message gzmsgs.Model
	if err := proto.Unmarshal(event.Data, &message); err != nil {
		return simulation.NewError(
			simulation.ReasonNativeFailure,
			"jetty.decode_joint_state",
			false,
		)
	}
	if expectedModel == "" || message.GetName() != expectedModel {
		return nil
	}

	var publish []simulation.JointState
	b.mu.Lock()
	generation := b.status.WorldGeneration
	world := b.status.WorldName
	simTime := b.status.Statistics.SimulationTimeNS
	for _, joint := range message.GetJoint() {
		if joint == nil || joint.GetName() == "" {
			continue
		}
		axes := []*gzmsgs.Axis{joint.GetAxis1(), joint.GetAxis2()}
		for axis, value := range axes {
			if value == nil {
				continue
			}
			state := simulation.JointState{
				WorldName:              world,
				WorldGeneration:        generation,
				Model:                  message.GetName(),
				Joint:                  joint.GetName(),
				Axis:                   uint32(axis),
				HasPosition:            true,
				Position:               value.GetPosition(),
				HasVelocity:            true,
				Velocity:               value.GetVelocity(),
				SampleSimulationTimeNS: simTime,
				Sequence:               event.Sequence,
			}
			key := jointKey(state.Model, state.Joint, state.Axis)
			b.jointStates[key] = state
			if _, pending := b.pendingFeedback[key]; pending {
				delete(b.pendingFeedback, key)
				publish = append(publish, state)
			}
		}
	}
	b.notifyLocked()
	b.mu.Unlock()

	for _, state := range publish {
		select {
		case b.events <- simulation.Event{
			Kind:       simulation.EventJointStateChanged,
			JointState: state,
		}:
		case <-b.ctx.Done():
			return nil
		}
	}
	return nil
}

func (b *Backend) JointState(
	ctx context.Context,
	query simulation.JointQuery,
) (simulation.JointState, error) {
	if err := b.requireWorld(query.WorldGeneration); err != nil {
		return simulation.JointState{}, err
	}
	if !validName(query.Model) || !validName(query.Joint) {
		return simulation.JointState{}, simulation.NewError(
			simulation.ReasonJointNotFound,
			"jetty.joint_state",
			false,
		)
	}
	if query.Axis > 1 {
		return simulation.JointState{}, simulation.NewError(
			simulation.ReasonAxisNotSupported,
			"jetty.joint_state",
			false,
		)
	}
	key := jointKey(query.Model, query.Joint, query.Axis)
	for {
		b.mu.RLock()
		feedback := b.feedbackModels[query.Model]
		state, ok := b.jointStates[key]
		signal := b.signal
		joints := b.scene.joints[query.Model]
		b.mu.RUnlock()
		if !feedback {
			return simulation.JointState{}, simulation.NewError(
				simulation.ReasonFeedbackUnavailable,
				"jetty.joint_state",
				true,
			)
		}
		joint, exists := findJoint(joints, query.Joint)
		if !exists {
			return simulation.JointState{}, simulation.NewError(
				simulation.ReasonJointNotFound,
				"jetty.joint_state",
				false,
			)
		}
		if !hasAxis(joint, query.Axis) {
			return simulation.JointState{}, simulation.NewError(
				simulation.ReasonAxisNotSupported,
				"jetty.joint_state",
				false,
			)
		}
		if ok {
			return state, nil
		}
		select {
		case <-signal:
		case <-ctx.Done():
			return simulation.JointState{}, ctx.Err()
		}
	}
}
