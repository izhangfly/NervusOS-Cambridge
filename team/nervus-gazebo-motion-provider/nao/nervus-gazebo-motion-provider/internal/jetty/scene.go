package jetty

import (
	"context"
	"math"

	"github.com/nervus-os/nervus-gazebo-provider/internal/gzmsgs"
	"github.com/nervus-os/nervus-gazebo-provider/internal/gztransport"
	"github.com/nervus-os/nervus-gazebo-provider/internal/simulation"
	"google.golang.org/protobuf/proto"
)

func (b *Backend) requestScene(ctx context.Context) (sceneSnapshot, error) {
	b.mu.RLock()
	world := b.status.WorldName
	b.mu.RUnlock()
	if world == "" {
		return sceneSnapshot{}, simulation.NewError(
			simulation.ReasonWorldNotReady, "jetty.scene", true,
		)
	}
	return b.requestSceneForWorld(ctx, world)
}

func (b *Backend) requestSceneForWorld(ctx context.Context, world string) (sceneSnapshot, error) {
	payload, err := proto.Marshal(&gzmsgs.Empty{})
	if err != nil {
		return sceneSnapshot{}, err
	}
	wire, err := b.client.Request(ctx, gztransport.ServiceRequest{
		Service:      sceneService(world),
		RequestType:  typeEmpty,
		ResponseType: typeScene,
		Data:         payload,
		Timeout:      b.config.RequestTimeout,
	})
	if err != nil {
		return sceneSnapshot{}, transportError(err, "jetty.scene", simulation.ReasonServiceNotFound)
	}
	var message gzmsgs.Scene
	if err := proto.Unmarshal(wire, &message); err != nil {
		return sceneSnapshot{}, simulation.NewError(
			simulation.ReasonNativeFailure,
			"jetty.decode_scene",
			false,
		)
	}
	snapshot := sceneSnapshot{
		joints:      make(map[string][]simulation.Joint),
		controllers: make(map[string]controller),
	}
	for _, model := range message.GetModel() {
		appendModel(&snapshot, model)
	}
	if len(snapshot.models) == 0 {
		return sceneSnapshot{}, simulation.NewError(
			simulation.ReasonWorldNotReady,
			"jetty.scene_empty",
			true,
		)
	}
	return snapshot, nil
}

func appendModel(snapshot *sceneSnapshot, model *gzmsgs.Model) {
	if snapshot == nil || model == nil || model.GetDeleted() || model.GetName() == "" {
		return
	}
	canonical := ""
	for _, link := range model.GetLink() {
		if link != nil && link.GetCanonical() {
			canonical = link.GetName()
			break
		}
	}
	snapshot.models = append(snapshot.models, simulation.Model{
		Name:          model.GetName(),
		EntityID:      uint64(model.GetId()),
		Static:        model.GetIsStatic(),
		CanonicalLink: canonical,
	})
	joints := make([]simulation.Joint, 0, len(model.GetJoint()))
	for _, joint := range model.GetJoint() {
		if joint == nil || joint.GetName() == "" {
			continue
		}
		item := simulation.Joint{
			Name: joint.GetName(),
			Type: joint.GetType().String(),
		}
		if axis := joint.GetAxis1(); axis != nil {
			item.Axes = append(item.Axes, simulation.JointAxis{
				Axis:              0,
				HasPositionLimits: joint.GetType() != gzmsgs.Joint_CONTINUOUS,
				LowerPosition:     axis.GetLimitLower(),
				UpperPosition:     axis.GetLimitUpper(),
			})
		}
		if axis := joint.GetAxis2(); axis != nil {
			item.Axes = append(item.Axes, simulation.JointAxis{
				Axis:              1,
				HasPositionLimits: joint.GetType() != gzmsgs.Joint_CONTINUOUS,
				LowerPosition:     axis.GetLimitLower(),
				UpperPosition:     axis.GetLimitUpper(),
			})
		}
		joints = append(joints, item)
	}
	snapshot.joints[model.GetName()] = joints
	for _, nested := range model.GetModel() {
		appendModel(snapshot, nested)
	}
}

func (b *Backend) listMotionTopics(ctx context.Context) ([]string, error) {
	topics, err := b.client.ListTopics(ctx)
	if err != nil {
		return nil, transportError(err, "jetty.list_topics", simulation.ReasonTopicNotFound)
	}
	return topics, nil
}

func (b *Backend) ListModels(ctx context.Context, generation uint64) (simulation.ModelList, error) {
	if err := b.requireWorld(generation); err != nil {
		return simulation.ModelList{}, err
	}
	snapshot, err := b.requestScene(ctx)
	if err != nil {
		return simulation.ModelList{}, err
	}
	topics, _ := b.listMotionTopics(ctx)
	b.discoverProfileBindings(ctx, &snapshot, topics, b.readyWorldName())
	b.replaceScene(snapshot)
	b.mu.RLock()
	models := append([]simulation.Model(nil), b.scene.models...)
	worldGeneration := b.status.WorldGeneration
	b.mu.RUnlock()
	return simulation.ModelList{
		WorldName:       b.readyWorldName(),
		WorldGeneration: worldGeneration,
		Models:          models,
	}, nil
}

func (b *Backend) ListJoints(
	ctx context.Context,
	model string,
	generation uint64,
) (simulation.JointList, error) {
	if err := b.requireWorld(generation); err != nil {
		return simulation.JointList{}, err
	}
	if !validName(model) {
		return simulation.JointList{}, simulation.NewError(
			simulation.ReasonModelNotFound,
			"jetty.list_joints",
			false,
		)
	}
	snapshot, err := b.requestScene(ctx)
	if err != nil {
		return simulation.JointList{}, err
	}
	topics, _ := b.listMotionTopics(ctx)
	b.discoverProfileBindings(ctx, &snapshot, topics, b.readyWorldName())
	joints, ok := snapshot.joints[model]
	if !ok {
		return simulation.JointList{}, simulation.NewError(
			simulation.ReasonModelNotFound,
			"jetty.list_joints",
			false,
		)
	}
	b.replaceScene(snapshot)
	b.mu.RLock()
	worldGeneration := b.status.WorldGeneration
	b.mu.RUnlock()
	return simulation.JointList{
		WorldName:       b.readyWorldName(),
		WorldGeneration: worldGeneration,
		Model:           model,
		Joints:          cloneJoints(joints),
	}, nil
}

func (b *Backend) readyWorldName() string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.status.WorldName
}

func (b *Backend) replaceScene(snapshot sceneSnapshot) {
	b.mu.Lock()
	for index := range snapshot.bindings {
		snapshot.bindings[index].WorldGeneration = b.status.WorldGeneration
	}
	b.scene = snapshot
	b.notifyLocked()
	b.mu.Unlock()
}

func (b *Backend) MotionBindings(context.Context) ([]simulation.MotionBinding, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return append([]simulation.MotionBinding(nil), b.scene.bindings...), nil
}

func (b *Backend) requireWorld(generation uint64) error {
	b.mu.RLock()
	status := b.status
	b.mu.RUnlock()
	if !status.ReadyForControl() {
		return simulation.NewError(simulation.ReasonWorldNotReady, "jetty.world", true)
	}
	if generation != 0 && generation != status.WorldGeneration {
		return simulation.NewError(simulation.ReasonWorldChanged, "jetty.world", false)
	}
	return nil
}

func cloneJoints(source []simulation.Joint) []simulation.Joint {
	out := make([]simulation.Joint, len(source))
	for index, joint := range source {
		out[index] = joint
		out[index].Axes = append([]simulation.JointAxis(nil), joint.Axes...)
	}
	return out
}

func validateJointLimit(joint simulation.Joint, axis uint32, position float64) bool {
	if math.IsNaN(position) || math.IsInf(position, 0) {
		return false
	}
	for _, description := range joint.Axes {
		if description.Axis != axis {
			continue
		}
		return !description.HasPositionLimits ||
			(position >= description.LowerPosition && position <= description.UpperPosition)
	}
	return false
}

func hasAxis(joint simulation.Joint, axis uint32) bool {
	for _, description := range joint.Axes {
		if description.Axis == axis {
			return true
		}
	}
	return false
}

func findJoint(joints []simulation.Joint, name string) (simulation.Joint, bool) {
	for _, joint := range joints {
		if joint.Name == name {
			return joint, true
		}
	}
	return simulation.Joint{}, false
}
