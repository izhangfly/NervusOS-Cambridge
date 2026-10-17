package provider

import (
	"context"
	"math"
	"time"
	"unicode/utf8"

	"github.com/nervus-os/nervus-gazebo-provider/internal/contract"
	"github.com/nervus-os/nervus-gazebo-provider/internal/simulation"
	"github.com/nervus-os/nervus-ipc/protocol/ipcv1"
	"github.com/nervus-os/nervus-ipc/sdk"
	"google.golang.org/protobuf/proto"
)

func decodeMessage(payload []byte, message proto.Message) error {
	if message == nil {
		return plainStatusError(ipcv1.StatusCode_STATUS_CODE_INTERNAL)
	}
	if err := proto.Unmarshal(payload, message); err != nil {
		return plainStatusError(ipcv1.StatusCode_STATUS_CODE_INVALID_ARGUMENT)
	}
	if len(message.ProtoReflect().GetUnknown()) != 0 {
		return plainStatusError(ipcv1.StatusCode_STATUS_CODE_INVALID_ARGUMENT)
	}
	return nil
}

func (s *Service) validateName(value string) bool {
	if value == "" || len(value) > s.config.MaxNameBytes || !utf8.ValidString(value) {
		return false
	}
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '_', r == '-', r == '.', r == ':':
		default:
			return false
		}
	}
	return true
}

func (s *Service) validatePosition(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) &&
		value >= s.config.MinJointRadians && value <= s.config.MaxJointRadians
}

func requirePermissions(cc sdk.CallContext, permissions ...string) error {
	if cc.Caller == nil {
		return plainStatusError(ipcv1.StatusCode_STATUS_CODE_PERMISSION_DENIED)
	}
	granted := make(map[string]struct{}, len(cc.Caller.GetGrantedPermissions()))
	for _, permission := range cc.Caller.GetGrantedPermissions() {
		granted[permission] = struct{}{}
	}
	for _, permission := range permissions {
		if permission == "" {
			continue
		}
		if _, ok := granted[permission]; !ok {
			return plainStatusError(ipcv1.StatusCode_STATUS_CODE_PERMISSION_DENIED)
		}
	}
	return nil
}

func queryAuthorized(cc sdk.CallContext) error {
	return requirePermissions(cc, contract.QueryPermission)
}

func lifecycleAuthorized(cc sdk.CallContext) error {
	return requirePermissions(cc, contract.LifecyclePermission)
}

func controlAuthorized(cc sdk.CallContext) error {
	return requirePermissions(cc, contract.ControlPermission)
}

func controlContext(cc sdk.CallContext, worldGeneration uint64) (simulation.ControlContext, error) {
	return physicalControlContext(cc, worldGeneration, contract.WorldResourceRole, contract.ControlPermission)
}

func physicalControlContext(cc sdk.CallContext, worldGeneration uint64, resourceRole, permission string) (simulation.ControlContext, error) {
	if err := requirePermissions(cc, permission); err != nil {
		return simulation.ControlContext{}, err
	}
	if worldGeneration == 0 {
		return simulation.ControlContext{}, plainStatusError(ipcv1.StatusCode_STATUS_CODE_INVALID_ARGUMENT)
	}
	execution := cc.Execution
	if execution == nil || execution.GetLeaseId() == 0 || execution.GetMotionEpoch() == 0 ||
		execution.GetCommandSequence() == 0 || execution.GetDeadlineNanos() <= 0 ||
		execution.GetResourceHandle() != resourceRole ||
		execution.GetResourceGeneration() == 0 {
		return simulation.ControlContext{}, plainStatusError(ipcv1.StatusCode_STATUS_CODE_FAILED_PRECONDITION)
	}
	var controller simulation.ControllerClass
	switch execution.GetControllerClass() {
	case ipcv1.ControllerClass_CONTROLLER_CLASS_HUMAN:
		controller = simulation.ControllerHuman
	case ipcv1.ControllerClass_CONTROLLER_CLASS_AI:
		controller = simulation.ControllerAI
	default:
		return simulation.ControlContext{}, plainStatusError(ipcv1.StatusCode_STATUS_CODE_FAILED_PRECONDITION)
	}
	return simulation.ControlContext{
		WorldGeneration:    worldGeneration,
		ResourceHandle:     execution.GetResourceHandle(),
		ResourceGeneration: execution.GetResourceGeneration(),
		LeaseID:            execution.GetLeaseId(),
		MotionEpoch:        execution.GetMotionEpoch(),
		DeadlineNanos:      execution.GetDeadlineNanos(),
		CommandSequence:    execution.GetCommandSequence(),
		Controller:         controller,
	}, nil
}

func (s *Service) acquireControl(
	ctx context.Context,
	execution simulation.ControlContext,
) (func(), error) {
	if ctx == nil {
		return nil, context.Canceled
	}
	select {
	case s.controlGate <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	release := func() { <-s.controlGate }
	if err := ctx.Err(); err != nil {
		release()
		return nil, err
	}
	now, err := monotonicNowNanos()
	if err != nil {
		release()
		return nil, statusError(
			simulation.ReasonNativeFailure,
			"control.validate_execution",
			execution.WorldGeneration,
			true,
		)
	}
	if now >= execution.DeadlineNanos {
		release()
		return nil, plainStatusError(ipcv1.StatusCode_STATUS_CODE_DEADLINE_EXCEEDED)
	}
	s.sequenceMu.Lock()
	state := s.sequences[execution.ResourceHandle]
	staleGeneration := state.generation != 0 && execution.ResourceGeneration < state.generation
	staleEpoch := state.epoch != 0 && execution.MotionEpoch < state.epoch
	staleSequence := state.generation == execution.ResourceGeneration && execution.CommandSequence <= state.sequence
	if staleGeneration || staleEpoch || staleSequence {
		s.sequenceMu.Unlock()
		release()
		return nil, plainStatusError(ipcv1.StatusCode_STATUS_CODE_FAILED_PRECONDITION)
	}
	s.sequences[execution.ResourceHandle] = sequenceState{generation: execution.ResourceGeneration, epoch: execution.MotionEpoch, sequence: execution.CommandSequence}
	s.sequenceMu.Unlock()
	return release, nil
}

func executionContext(parent context.Context, execution simulation.ControlContext) (context.Context, context.CancelFunc, error) {
	now, err := monotonicNowNanos()
	if err != nil {
		return nil, nil, statusError(simulation.ReasonNativeFailure, "control.deadline", execution.WorldGeneration, true)
	}
	remaining := execution.DeadlineNanos - now
	if remaining <= 0 {
		return nil, nil, plainStatusError(ipcv1.StatusCode_STATUS_CODE_DEADLINE_EXCEEDED)
	}
	if remaining > int64(time.Duration(1<<63-1)) {
		remaining = int64(time.Duration(1<<63 - 1))
	}
	ctx, cancel := context.WithTimeout(parent, time.Duration(remaining))
	return ctx, cancel, nil
}
