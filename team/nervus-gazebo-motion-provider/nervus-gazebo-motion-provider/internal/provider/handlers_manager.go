package provider

import (
	"fmt"

	simulationv1 "github.com/nervus-os/nervus-gazebo-provider/gen/go/gazebo/simulation/provider/v1"
	"github.com/nervus-os/nervus-gazebo-provider/internal/simulation"
	"github.com/nervus-os/nervus-ipc/protocol/ipcv1"
	"github.com/nervus-os/nervus-ipc/sdk"
	"google.golang.org/protobuf/proto"
)

func (s *Service) getStatus(cc sdk.CallContext, payload []byte) ([]byte, error) {
	if err := queryAuthorized(cc); err != nil {
		return nil, err
	}
	if err := decodeMessage(payload, &simulationv1.GetStatusRequest{}); err != nil {
		return nil, err
	}
	status, err := s.backend.Status(cc.Ctx)
	if err != nil {
		return nil, backendError(err, "manager.get_status", 0)
	}
	return proto.Marshal(statusToWire(status))
}

func (s *Service) listModels(cc sdk.CallContext, payload []byte) ([]byte, error) {
	if err := queryAuthorized(cc); err != nil {
		return nil, err
	}
	var request simulationv1.ListModelsRequest
	if err := decodeMessage(payload, &request); err != nil {
		return nil, err
	}
	list, err := s.backend.ListModels(cc.Ctx, request.GetWorldGeneration())
	if err != nil {
		return nil, backendError(err, "manager.list_models", request.GetWorldGeneration())
	}
	return proto.Marshal(modelListToWire(list))
}

func (s *Service) listJoints(cc sdk.CallContext, payload []byte) ([]byte, error) {
	if err := queryAuthorized(cc); err != nil {
		return nil, err
	}
	var request simulationv1.ListJointsRequest
	if err := decodeMessage(payload, &request); err != nil {
		return nil, err
	}
	if !s.validateName(request.GetModel()) {
		return nil, plainStatusError(ipcv1.StatusCode_STATUS_CODE_INVALID_ARGUMENT)
	}
	list, err := s.backend.ListJoints(
		cc.Ctx,
		request.GetModel(),
		request.GetWorldGeneration(),
	)
	if err != nil {
		return nil, backendError(err, "manager.list_joints", request.GetWorldGeneration())
	}
	return proto.Marshal(jointListToWire(list))
}

func (s *Service) getJointState(cc sdk.CallContext, payload []byte) ([]byte, error) {
	if err := queryAuthorized(cc); err != nil {
		return nil, err
	}
	var request simulationv1.GetJointStateRequest
	if err := decodeMessage(payload, &request); err != nil {
		return nil, err
	}
	if !s.validateName(request.GetModel()) || !s.validateName(request.GetJoint()) {
		return nil, plainStatusError(ipcv1.StatusCode_STATUS_CODE_INVALID_ARGUMENT)
	}
	state, err := s.backend.JointState(cc.Ctx, simulation.JointQuery{
		WorldGeneration: request.GetWorldGeneration(),
		Model:           request.GetModel(),
		Joint:           request.GetJoint(),
		Axis:            request.GetAxis(),
	})
	if err != nil {
		return nil, backendError(err, "manager.get_joint_state", request.GetWorldGeneration())
	}
	return proto.Marshal(jointStateToWire(state))
}

func (s *Service) startSimulation(cc sdk.CallContext, payload []byte) ([]byte, error) {
	if err := lifecycleAuthorized(cc); err != nil {
		return nil, err
	}
	var request simulationv1.StartSimulationRequest
	if err := decodeMessage(payload, &request); err != nil {
		return nil, err
	}
	if request.GetWorld() != "" && !s.validateName(request.GetWorld()) {
		return nil, plainStatusError(ipcv1.StatusCode_STATUS_CODE_INVALID_ARGUMENT)
	}
	status, err := s.backend.Start(cc.Ctx, simulation.StartOptions{World: request.GetWorld()})
	if err != nil {
		return nil, backendError(err, "manager.start", 0)
	}
	return s.lifecycleResult(cc, status)
}

func (s *Service) stopSimulation(cc sdk.CallContext, payload []byte) ([]byte, error) {
	if err := lifecycleAuthorized(cc); err != nil {
		return nil, err
	}
	if err := decodeMessage(payload, &simulationv1.StopSimulationRequest{}); err != nil {
		return nil, err
	}
	status, err := s.backend.Stop(cc.Ctx)
	if err != nil {
		return nil, backendError(err, "manager.stop", 0)
	}
	return s.lifecycleResult(cc, status)
}

func (s *Service) loadWorld(cc sdk.CallContext, payload []byte) ([]byte, error) {
	if err := lifecycleAuthorized(cc); err != nil {
		return nil, err
	}
	var request simulationv1.LoadWorldRequest
	if err := decodeMessage(payload, &request); err != nil {
		return nil, err
	}
	if !s.validateName(request.GetWorld()) {
		return nil, plainStatusError(ipcv1.StatusCode_STATUS_CODE_INVALID_ARGUMENT)
	}
	status, err := s.backend.LoadWorld(cc.Ctx, request.GetWorld())
	if err != nil {
		return nil, backendError(err, "manager.load_world", 0)
	}
	return s.lifecycleResult(cc, status)
}

func (s *Service) lifecycleResult(cc sdk.CallContext, status simulation.Status) ([]byte, error) {
	if err := s.applyStatus(cc.Ctx, status, 0); err != nil {
		return nil, fmt.Errorf("provider: reconcile lifecycle status: %w", err)
	}
	return proto.Marshal(&simulationv1.LifecycleResult{Status: statusToWire(status)})
}

func (s *Service) getCapabilities(cc sdk.CallContext, payload []byte) ([]byte, error) {
	if err := queryAuthorized(cc); err != nil {
		return nil, err
	}
	if err := decodeMessage(payload, &simulationv1.GetCapabilitiesRequest{}); err != nil {
		return nil, err
	}
	capabilities, err := s.backend.Capabilities(cc.Ctx)
	if err != nil {
		return nil, backendError(err, "manager.get_capabilities", 0)
	}
	if capabilities.CanControlWorld &&
		(capabilities.MaxStepIterations == 0 || capabilities.MaxStepIterations > s.config.MaxStepCount) {
		capabilities.MaxStepIterations = s.config.MaxStepCount
	}
	if capabilities.CanCommandJoints &&
		(capabilities.MaxJointCommandsPerRequest == 0 ||
			capabilities.MaxJointCommandsPerRequest > uint32(s.config.MaxJointBatch)) {
		capabilities.MaxJointCommandsPerRequest = uint32(s.config.MaxJointBatch)
	}
	return proto.Marshal(capabilitiesToWire(capabilities))
}
