package jetty

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"time"

	profilev1 "github.com/nervus-os/nervus-gazebo-provider/gen/go/gazebo/simulation/motion/profile/v1"
	"github.com/nervus-os/nervus-gazebo-provider/internal/gztransport"
	"github.com/nervus-os/nervus-gazebo-provider/internal/motion"
	"github.com/nervus-os/nervus-gazebo-provider/internal/simulation"
)

func (b *Backend) discoverProfileBindings(
	ctx context.Context,
	snapshot *sceneSnapshot,
	topics []string,
	world string,
) {
	if snapshot == nil {
		return
	}
	if snapshot.controllers == nil {
		snapshot.controllers = make(map[string]controller)
	}
	if snapshot.baseControllers == nil {
		snapshot.baseControllers = make(map[string]baseController)
	}
	if snapshot.trajectoryGroups == nil {
		snapshot.trajectoryGroups = make(map[string]trajectoryGroup)
	}
	if snapshot.homeStates == nil {
		snapshot.homeStates = make(map[string]map[motion.JointRef]float64)
	}
	models := make([]string, 0, len(snapshot.models))
	for _, model := range snapshot.models {
		models = append(models, model.Name)
	}
	resolution := b.config.Profiles.Resolve(models, b.config.Runtime)
	for _, issue := range resolution.Issues {
		state := simulation.MotionBindingUnsupported
		if issue.Reason == "model_not_discovered" {
			state = simulation.MotionBindingDiscovering
		}
		snapshot.bindings = append(snapshot.bindings, simulation.MotionBinding{
			ProfileID: issue.ProfileID, Model: issue.Model,
			CapabilityRole: issue.CapabilityRole, State: state, Reason: issue.Reason,
		})
	}
	listed := make(map[string]struct{}, len(topics))
	for _, topic := range topics {
		listed[topic] = struct{}{}
	}
	b.mu.RLock()
	for _, cached := range b.scene.controllers {
		listed[cached.topic] = struct{}{}
	}
	b.mu.RUnlock()
	for _, instance := range resolution.Instances {
		commands := commandBindingMap(instance.Profile)
		sources := stateSourceMap(instance.Profile)
		groups := jointGroupMap(instance.Profile)
		for _, capability := range instance.Profile.GetCapabilities() {
			binding := simulation.MotionBinding{
				ProfileID: instance.Profile.GetProfileId(), Model: instance.Model,
				CapabilityRole: capability.GetRole(),
				ResourceRole:   instance.Resources[capability.GetRole()],
				InterfaceID:    capability.GetInterfaceId(),
				State:          simulation.MotionBindingDiscovering,
			}
			spec, ok := motion.Lookup(capability.GetAdapter())
			if !ok {
				binding.State = simulation.MotionBindingUnsupported
				binding.Reason = "adapter_not_registered"
				snapshot.bindings = append(snapshot.bindings, binding)
				continue
			}
			binding.Adapter = spec.Name
			if !spec.RuntimeAvailable {
				binding.State = simulation.MotionBindingUnsupported
				binding.Reason = "adapter_not_implemented"
				snapshot.bindings = append(snapshot.bindings, binding)
				continue
			}
			switch capability.GetAdapter() {
			case profilev1.AdapterKind_ADAPTER_KIND_JOINT_POSITION:
				binding.State, binding.Reason = b.bindJointPosition(
					ctx, snapshot, instance.Model, world, capability,
					groups[capability.GetJointGroup()],
					commands[capability.GetCommandBinding()], sources, listed,
				)
			case profilev1.AdapterKind_ADAPTER_KIND_DIFF_DRIVE:
				binding.State, binding.Reason = b.bindDiffDrive(
					ctx, snapshot, instance.Model, world, binding.ResourceRole,
					capability, commands[capability.GetCommandBinding()], sources, listed,
				)
			default:
				binding.State = simulation.MotionBindingUnsupported
				binding.Reason = "adapter_not_implemented"
			}
			snapshot.bindings = append(snapshot.bindings, binding)
		}
		bindTrajectoryGroups(snapshot, instance.Profile, instance.Model)
	}
	sort.Slice(snapshot.bindings, func(i, j int) bool {
		left := snapshot.bindings[i]
		right := snapshot.bindings[j]
		return fmt.Sprintf("%s/%s/%s", left.ProfileID, left.Model, left.CapabilityRole) <
			fmt.Sprintf("%s/%s/%s", right.ProfileID, right.Model, right.CapabilityRole)
	})
}

func bindTrajectoryGroups(
	snapshot *sceneSnapshot,
	profile *profilev1.MotionProfile,
	model string,
) {
	if snapshot == nil || profile == nil || model == "" {
		return
	}
	maxVelocity, tolerance, settle := trajectoryDefaults(profile)
	if maxVelocity <= 0 || tolerance <= 0 || settle <= 0 {
		return
	}
	groups := jointGroupMap(profile)
	for _, group := range profile.GetJointGroups() {
		configured := trajectoryGroup{model: model, role: group.GetRole(), settleTime: settle}
		valid := group.GetRole() != "" && len(group.GetJoints()) > 0
		for _, spec := range group.GetJoints() {
			ref := motion.JointRef{Model: model, Joint: spec.GetName(), Axis: spec.GetAxis()}
			controller, ok := snapshot.controllers[jointKey(model, ref.Joint, ref.Axis)]
			axisIndex, hasAxis := jointAxisIndex(controller.joint, ref.Axis)
			if !ok || !hasAxis {
				valid = false
				break
			}
			axis := controller.joint.Axes[axisIndex]
			jointTolerance := spec.GetPositionToleranceRad()
			if jointTolerance <= 0 {
				jointTolerance = tolerance
			}
			jointVelocity := spec.GetMaxVelocityRadS()
			if jointVelocity <= 0 {
				jointVelocity = maxVelocity
			}
			configured.joints = append(configured.joints, ref)
			configured.limits = append(configured.limits, motion.JointLimit{
				HasPosition: axis.HasPositionLimits,
				Lower:       axis.LowerPosition,
				Upper:       axis.UpperPosition,
				Tolerance:   jointTolerance,
				MaxVelocity: jointVelocity,
			})
		}
		if valid {
			snapshot.trajectoryGroups[trajectoryGroupKey(model, group.GetRole())] = configured
		}
	}
	for _, home := range profile.GetHomeStates() {
		group := groups[home.GetJointGroup()]
		if home.GetRole() == "" || group == nil || len(home.GetPositionRad()) != len(group.GetJoints()) {
			continue
		}
		targets := make(map[motion.JointRef]float64, len(group.GetJoints()))
		for index, spec := range group.GetJoints() {
			targets[motion.JointRef{Model: model, Joint: spec.GetName(), Axis: spec.GetAxis()}] =
				home.GetPositionRad()[index]
		}
		snapshot.homeStates[trajectoryHomeKey(model, home.GetRole())] = targets
	}
}

func trajectoryDefaults(profile *profilev1.MotionProfile) (float64, float64, time.Duration) {
	for _, capability := range profile.GetCapabilities() {
		limits := capability.GetLimits()
		if capability.GetJointGroup() == "" || limits == nil ||
			limits.GetMaxJointVelocityRadS() <= 0 || limits.GetPositionToleranceRad() <= 0 ||
			limits.GetSettleTimeMs() == 0 {
			continue
		}
		return limits.GetMaxJointVelocityRadS(), limits.GetPositionToleranceRad(),
			time.Duration(limits.GetSettleTimeMs()) * time.Millisecond
	}
	return 0, 0, 0
}

func trajectoryGroupKey(model, role string) string { return model + "\x00" + role }

func trajectoryHomeKey(model, role string) string { return model + "\x00" + role }

const commandSubscriberProbeTimeout = 2 * time.Second

func (b *Backend) requireCommandSubscribers(
	ctx context.Context,
	topic string,
	messageType string,
	wasListed bool,
) (simulation.MotionBindingState, string, bool) {
	info, infoErr := b.client.TopicInfo(ctx, topic)
	discovered := wasListed || infoErr == nil
	if infoErr == nil {
		if info.TypeConflict || info.MessageType != messageType {
			return simulation.MotionBindingUnsupported, "command_message_type_mismatch", false
		}
		if info.SubscriberCount > 0 {
			return simulation.MotionBindingReady, "", true
		}
	} else if !errors.Is(infoErr, gztransport.ErrNotFound) {
		return simulation.MotionBindingDegraded, "command_topic_missing", false
	}

	probeCtx, cancel := context.WithTimeout(ctx, commandSubscriberProbeTimeout)
	defer cancel()
	if err := b.client.WaitForSubscribers(probeCtx, topic, messageType); err != nil {
		if errors.Is(err, gztransport.ErrTypeMismatch) {
			return simulation.MotionBindingUnsupported, "command_message_type_mismatch", false
		}
		if discovered {
			return simulation.MotionBindingDegraded, "command_subscriber_missing", false
		}
		return simulation.MotionBindingDegraded, "command_topic_missing", false
	}
	return simulation.MotionBindingReady, "", true
}

func (b *Backend) bindDiffDrive(
	ctx context.Context,
	snapshot *sceneSnapshot,
	model, world, resourceRole string,
	capability *profilev1.MotionCapability,
	command *profilev1.CommandBinding,
	sources map[string]*profilev1.StateSource,
	listed map[string]struct{},
) (simulation.MotionBindingState, string) {
	if command == nil || resourceRole == "" || capability.GetLimits() == nil {
		return simulation.MotionBindingUnsupported, "profile_binding_incomplete"
	}
	limits := capability.GetLimits()
	if limits.GetMaxLinearMS() <= 0 || limits.GetMaxAngularRadS() <= 0 ||
		limits.GetSettleTimeMs() == 0 {
		return simulation.MotionBindingUnsupported, "motion_limits_missing"
	}
	commandTopic, err := motion.ExpandTemplate(command.GetTopicTemplate(), map[string]string{
		"model": model,
	})
	if err != nil {
		return simulation.MotionBindingUnsupported, "command_topic_invalid"
	}
	_, wasListed := listed[commandTopic]
	if state, reason, ready := b.requireCommandSubscribers(
		ctx, commandTopic, command.GetMessageType(), wasListed,
	); !ready {
		return state, reason
	}

	odometryTopic := ""
	for _, role := range capability.GetStateSources() {
		source := sources[role]
		if source == nil {
			return simulation.MotionBindingUnsupported, "state_source_missing"
		}
		topic, expandErr := motion.ExpandTemplate(source.GetTopicTemplate(), map[string]string{
			"world": world, "model": model,
		})
		if expandErr != nil {
			return simulation.MotionBindingUnsupported, "state_topic_invalid"
		}
		stateInfo, stateErr := b.client.TopicInfo(ctx, topic)
		if stateErr != nil {
			if source.GetOptional() && errors.Is(stateErr, gztransport.ErrNotFound) {
				continue
			}
			return simulation.MotionBindingDegraded, "state_topic_missing"
		}
		if stateInfo.TypeConflict || stateInfo.MessageType != source.GetMessageType() ||
			stateInfo.PublisherCount < 1 {
			if source.GetOptional() {
				continue
			}
			return simulation.MotionBindingUnsupported, "state_message_type_mismatch"
		}
		if source.GetKind() == profilev1.StateSourceKind_STATE_SOURCE_KIND_ODOMETRY {
			if source.GetOptional() || odometryTopic != "" {
				return simulation.MotionBindingUnsupported, "odometry_source_invalid"
			}
			odometryTopic = topic
			snapshot.stateSources = append(snapshot.stateSources, stateSource{
				topic: topic, messageType: source.GetMessageType(), model: model,
				kind: source.GetKind(), resourceRole: resourceRole,
			})
		}
	}
	if odometryTopic == "" {
		return simulation.MotionBindingUnsupported, "odometry_source_missing"
	}
	linearTolerance := limits.GetMaxLinearMS() * 0.02
	if linearTolerance < 0.005 {
		linearTolerance = 0.005
	}
	angularTolerance := limits.GetMaxAngularRadS() * 0.02
	if angularTolerance < 0.01 {
		angularTolerance = 0.01
	}
	snapshot.baseControllers[resourceRole] = baseController{
		model: model, commandTopic: commandTopic, odometryTopic: odometryTopic,
		maxLinear: limits.GetMaxLinearMS(), maxAngular: limits.GetMaxAngularRadS(),
		settleTime:          time.Duration(limits.GetSettleTimeMs()) * time.Millisecond,
		linearStopTolerance: linearTolerance, angularStopTolerance: angularTolerance,
	}
	return simulation.MotionBindingDiscovering, "feedback_pending"
}

func (b *Backend) bindJointPosition(
	ctx context.Context,
	snapshot *sceneSnapshot,
	model, world string,
	capability *profilev1.MotionCapability,
	group *profilev1.JointGroup,
	command *profilev1.CommandBinding,
	sources map[string]*profilev1.StateSource,
	listed map[string]struct{},
) (simulation.MotionBindingState, string) {
	if group == nil || command == nil {
		return simulation.MotionBindingUnsupported, "profile_binding_incomplete"
	}
	joints := cloneJoints(snapshot.joints[model])
	indices := make(map[string]int, len(joints))
	for index, joint := range joints {
		indices[joint.Name] = index
	}
	localControllers := make(map[string]controller, len(group.GetJoints()))
	for _, expected := range group.GetJoints() {
		index, exists := indices[expected.GetName()]
		if !exists {
			joints = append(joints, simulation.Joint{
				Name: expected.GetName(), Type: "UNKNOWN",
				Axes: []simulation.JointAxis{{Axis: expected.GetAxis()}},
			})
			index = len(joints) - 1
			indices[expected.GetName()] = index
		}
		joint := joints[index]
		axisIndex, ok := jointAxisIndex(joint, expected.GetAxis())
		if !ok {
			return simulation.MotionBindingUnsupported, "joint_axis_missing"
		}
		if expected.GetHasPositionLimits() {
			native := joint.Axes[axisIndex]
			if native.HasPositionLimits && (expected.GetLowerPositionRad() < native.LowerPosition ||
				expected.GetUpperPositionRad() > native.UpperPosition) {
				return simulation.MotionBindingUnsupported, "profile_limits_exceed_model"
			}
			joint.Axes[axisIndex].HasPositionLimits = true
			joint.Axes[axisIndex].LowerPosition = expected.GetLowerPositionRad()
			joint.Axes[axisIndex].UpperPosition = expected.GetUpperPositionRad()
		}
		topic, err := motion.ExpandTemplate(command.GetTopicTemplate(), map[string]string{
			"model": model, "joint": expected.GetName(),
			"axis": strconv.FormatUint(uint64(expected.GetAxis()), 10),
		})
		if err != nil {
			return simulation.MotionBindingUnsupported, "command_topic_invalid"
		}
		_, wasListed := listed[topic]
		if state, reason, ready := b.requireCommandSubscribers(
			ctx, topic, command.GetMessageType(), wasListed,
		); !ready {
			return state, reason
		}
		joint.Controllable = true
		joints[index] = joint
		localControllers[jointKey(model, expected.GetName(), expected.GetAxis())] = controller{
			topic: topic, joint: joint,
		}
	}
	for _, role := range capability.GetStateSources() {
		source := sources[role]
		if source == nil {
			return simulation.MotionBindingUnsupported, "state_source_missing"
		}
		topic, err := motion.ExpandTemplate(source.GetTopicTemplate(), map[string]string{
			"world": world, "model": model,
		})
		if err != nil {
			return simulation.MotionBindingUnsupported, "state_topic_invalid"
		}
		info, err := b.client.TopicInfo(ctx, topic)
		if err != nil {
			if source.GetOptional() && errors.Is(err, gztransport.ErrNotFound) {
				continue
			}
			return simulation.MotionBindingDegraded, "state_topic_missing"
		}
		if info.TypeConflict || info.MessageType != source.GetMessageType() || info.PublisherCount < 1 {
			if source.GetOptional() {
				continue
			}
			return simulation.MotionBindingUnsupported, "state_message_type_mismatch"
		}
		snapshot.stateSources = append(snapshot.stateSources, stateSource{
			topic: topic, messageType: source.GetMessageType(), model: model,
			optional: source.GetOptional(),
		})
	}
	for key, item := range localControllers {
		snapshot.controllers[key] = item
	}
	sort.Slice(joints, func(i, j int) bool { return joints[i].Name < joints[j].Name })
	snapshot.joints[model] = joints
	return simulation.MotionBindingReady, ""
}

func commandBindingMap(profile *profilev1.MotionProfile) map[string]*profilev1.CommandBinding {
	out := make(map[string]*profilev1.CommandBinding, len(profile.GetCommandBindings()))
	for _, binding := range profile.GetCommandBindings() {
		out[binding.GetRole()] = binding
	}
	return out
}

func stateSourceMap(profile *profilev1.MotionProfile) map[string]*profilev1.StateSource {
	out := make(map[string]*profilev1.StateSource, len(profile.GetStateSources()))
	for _, source := range profile.GetStateSources() {
		out[source.GetRole()] = source
	}
	return out
}

func jointGroupMap(profile *profilev1.MotionProfile) map[string]*profilev1.JointGroup {
	out := make(map[string]*profilev1.JointGroup, len(profile.GetJointGroups()))
	for _, group := range profile.GetJointGroups() {
		out[group.GetRole()] = group
	}
	return out
}

func jointAxisIndex(joint simulation.Joint, axis uint32) (int, bool) {
	for index, item := range joint.Axes {
		if item.Axis == axis {
			return index, true
		}
	}
	return 0, false
}

func (b *Backend) degradeModelBindings(snapshot *sceneSnapshot, model, reason string) {
	for index := range snapshot.bindings {
		if snapshot.bindings[index].Model == model &&
			snapshot.bindings[index].State == simulation.MotionBindingReady {
			snapshot.bindings[index].State = simulation.MotionBindingDegraded
			snapshot.bindings[index].Reason = reason
		}
	}
	for key, item := range snapshot.controllers {
		if item.joint.Name != "" {
			// The NUL-delimited controller key always begins with model.
			if len(key) > len(model) && key[:len(model)] == model && key[len(model)] == 0 {
				delete(snapshot.controllers, key)
			}
		}
	}
	for role, item := range snapshot.baseControllers {
		if item.model == model {
			delete(snapshot.baseControllers, role)
		}
	}
}
