package provider

import (
	"fmt"

	simulationv1 "github.com/nervus-os/nervus-gazebo-provider/gen/go/gazebo/simulation/provider/v1"
	"github.com/nervus-os/nervus-gazebo-provider/internal/simulation"
	"github.com/nervus-os/nervus-ipc/protocol/ipcv1"
	"github.com/nervus-os/nervus-ipc/sdk"
	"google.golang.org/protobuf/proto"
)

func (s *Service) pause(cc sdk.CallContext, payload []byte) ([]byte, error) {
	if err := controlAuthorized(cc); err != nil {
		return nil, err
	}
	var request simulationv1.PauseRequest
	if err := decodeMessage(payload, &request); err != nil {
		return nil, err
	}
	execution, err := controlContext(cc, request.GetWorldGeneration())
	if err != nil {
		return nil, err
	}
	release, err := s.acquireControl(cc.Ctx, execution)
	if err != nil {
		return nil, err
	}
	defer release()
	status, err := s.backend.Pause(cc.Ctx, execution)
	if err != nil {
		return nil, backendError(err, "control.pause", request.GetWorldGeneration())
	}
	return s.worldControlResult(cc, status)
}

func (s *Service) resume(cc sdk.CallContext, payload []byte) ([]byte, error) {
	if err := controlAuthorized(cc); err != nil {
		return nil, err
	}
	var request simulationv1.ResumeRequest
	if err := decodeMessage(payload, &request); err != nil {
		return nil, err
	}
	execution, err := controlContext(cc, request.GetWorldGeneration())
	if err != nil {
		return nil, err
	}
	release, err := s.acquireControl(cc.Ctx, execution)
	if err != nil {
		return nil, err
	}
	defer release()
	status, err := s.backend.Resume(cc.Ctx, execution)
	if err != nil {
		return nil, backendError(err, "control.resume", request.GetWorldGeneration())
	}
	return s.worldControlResult(cc, status)
}

func (s *Service) step(cc sdk.CallContext, payload []byte) ([]byte, error) {
	if err := controlAuthorized(cc); err != nil {
		return nil, err
	}
	var request simulationv1.StepRequest
	if err := decodeMessage(payload, &request); err != nil {
		return nil, err
	}
	if request.GetIterations() == 0 || request.GetIterations() > s.config.MaxStepCount {
		return nil, plainStatusError(ipcv1.StatusCode_STATUS_CODE_INVALID_ARGUMENT)
	}
	execution, err := controlContext(cc, request.GetWorldGeneration())
	if err != nil {
		return nil, err
	}
	release, err := s.acquireControl(cc.Ctx, execution)
	if err != nil {
		return nil, err
	}
	defer release()
	status, err := s.backend.Step(cc.Ctx, execution, request.GetIterations())
	if err != nil {
		return nil, backendError(err, "control.step", request.GetWorldGeneration())
	}
	return s.worldControlResult(cc, status)
}

func (s *Service) resetWorld(cc sdk.CallContext, payload []byte) ([]byte, error) {
	if err := controlAuthorized(cc); err != nil {
		return nil, err
	}
	var request simulationv1.ResetWorldRequest
	if err := decodeMessage(payload, &request); err != nil {
		return nil, err
	}
	var mode simulation.ResetMode
	switch request.GetMode() {
	case simulationv1.WorldResetMode_WORLD_RESET_MODE_ALL:
		mode = simulation.ResetAll
	case simulationv1.WorldResetMode_WORLD_RESET_MODE_TIME_ONLY:
		mode = simulation.ResetTimeOnly
	case simulationv1.WorldResetMode_WORLD_RESET_MODE_MODELS_ONLY:
		mode = simulation.ResetModelsOnly
	default:
		return nil, plainStatusError(ipcv1.StatusCode_STATUS_CODE_INVALID_ARGUMENT)
	}
	execution, err := controlContext(cc, request.GetWorldGeneration())
	if err != nil {
		return nil, err
	}
	release, err := s.acquireControl(cc.Ctx, execution)
	if err != nil {
		return nil, err
	}
	defer release()
	status, err := s.backend.ResetWorld(cc.Ctx, execution, mode)
	if err != nil {
		return nil, backendError(err, "control.reset_world", request.GetWorldGeneration())
	}
	return s.worldControlResult(cc, status)
}

func (s *Service) worldControlResult(cc sdk.CallContext, status simulation.Status) ([]byte, error) {
	if err := s.applyStatus(cc.Ctx, status, 0); err != nil {
		return nil, fmt.Errorf("provider: reconcile control status: %w", err)
	}
	return proto.Marshal(&simulationv1.WorldControlResult{Status: statusToWire(status)})
}

func (s *Service) setJointPosition(cc sdk.CallContext, payload []byte) ([]byte, error) {
	if err := controlAuthorized(cc); err != nil {
		return nil, err
	}
	var request simulationv1.SetJointPositionRequest
	if err := decodeMessage(payload, &request); err != nil {
		return nil, err
	}
	if request.GetCommand() == nil {
		return nil, plainStatusError(ipcv1.StatusCode_STATUS_CODE_INVALID_ARGUMENT)
	}
	command, err := s.validateJointCommand(request.GetCommand(), request.GetWorldGeneration())
	if err != nil {
		return nil, err
	}
	return s.submitJointCommands(
		cc,
		request.GetWorldGeneration(),
		[]simulation.JointCommand{command},
		"control.set_joint_position",
	)
}

func (s *Service) setJointPositions(cc sdk.CallContext, payload []byte) ([]byte, error) {
	if err := controlAuthorized(cc); err != nil {
		return nil, err
	}
	var request simulationv1.SetJointPositionsRequest
	if err := decodeMessage(payload, &request); err != nil {
		return nil, err
	}
	if len(request.GetCommands()) == 0 {
		return nil, plainStatusError(ipcv1.StatusCode_STATUS_CODE_INVALID_ARGUMENT)
	}
	if len(request.GetCommands()) > s.config.MaxJointBatch {
		return nil, statusError(
			simulation.ReasonBatchTooLarge,
			"control.set_joint_positions",
			request.GetWorldGeneration(),
			false,
		)
	}
	commands := make([]simulation.JointCommand, 0, len(request.GetCommands()))
	seen := make(map[string]struct{}, len(request.GetCommands()))
	var model string
	for _, wireCommand := range request.GetCommands() {
		command, err := s.validateJointCommand(wireCommand, request.GetWorldGeneration())
		if err != nil {
			return nil, err
		}
		if model == "" {
			model = command.Model
		} else if model != command.Model {
			return nil, plainStatusError(ipcv1.StatusCode_STATUS_CODE_INVALID_ARGUMENT)
		}
		key := command.Model + "\x00" + command.Joint + "\x00" + fmt.Sprint(command.Axis)
		if _, duplicate := seen[key]; duplicate {
			return nil, plainStatusError(ipcv1.StatusCode_STATUS_CODE_INVALID_ARGUMENT)
		}
		seen[key] = struct{}{}
		commands = append(commands, command)
	}
	return s.submitJointCommands(
		cc,
		request.GetWorldGeneration(),
		commands,
		"control.set_joint_positions",
	)
}

func (s *Service) validateJointCommand(
	command *simulationv1.JointPositionCommand,
	worldGeneration uint64,
) (simulation.JointCommand, error) {
	if command == nil || !s.validateName(command.GetModel()) || !s.validateName(command.GetJoint()) {
		return simulation.JointCommand{}, plainStatusError(ipcv1.StatusCode_STATUS_CODE_INVALID_ARGUMENT)
	}
	if command.GetAxis() != 0 {
		return simulation.JointCommand{}, statusError(
			simulation.ReasonAxisNotSupported,
			"control.validate_joint_command",
			worldGeneration,
			false,
		)
	}
	if !s.validatePosition(command.GetPosition()) {
		return simulation.JointCommand{}, statusError(
			simulation.ReasonOutOfRange,
			"control.validate_joint_command",
			worldGeneration,
			false,
		)
	}
	return simulation.JointCommand{
		Model:    command.GetModel(),
		Joint:    command.GetJoint(),
		Axis:     command.GetAxis(),
		Position: command.GetPosition(),
	}, nil
}

func (s *Service) submitJointCommands(
	cc sdk.CallContext,
	worldGeneration uint64,
	commands []simulation.JointCommand,
	operation string,
) ([]byte, error) {
	execution, err := controlContext(cc, worldGeneration)
	if err != nil {
		return nil, err
	}
	release, err := s.acquireControl(cc.Ctx, execution)
	if err != nil {
		return nil, err
	}
	defer release()
	receipt, err := s.backend.SetJointPositions(cc.Ctx, execution, commands)
	if err != nil {
		return nil, backendError(err, operation, worldGeneration)
	}
	if receipt.SubmittedCommands != uint32(len(commands)) {
		return nil, fmt.Errorf("provider: backend returned an inconsistent joint command receipt")
	}
	return proto.Marshal(&simulationv1.JointCommandReceipt{
		WorldGeneration:   worldGeneration,
		CommandSequence:   execution.CommandSequence,
		SubmittedCommands: receipt.SubmittedCommands,
		FeedbackAvailable: receipt.FeedbackAvailable,
	})
}
