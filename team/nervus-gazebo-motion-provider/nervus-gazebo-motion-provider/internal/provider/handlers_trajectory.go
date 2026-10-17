package provider

import (
	"context"
	"math"
	"time"

	simulationv1 "github.com/nervus-os/nervus-gazebo-provider/gen/go/gazebo/simulation/provider/v1"
	"github.com/nervus-os/nervus-gazebo-provider/internal/simulation"
	"github.com/nervus-os/nervus-ipc/protocol/ipcv1"
	"github.com/nervus-os/nervus-ipc/sdk"
	"google.golang.org/protobuf/proto"
)

func (s *Service) jointMotionBackend() (simulation.JointMotionBackend, error) {
	backend, ok := s.backend.(simulation.JointMotionBackend)
	if !ok {
		return nil, plainStatusError(ipcv1.StatusCode_STATUS_CODE_UNAVAILABLE)
	}
	return backend, nil
}

func (s *Service) setJointTrajectory(cc sdk.CallContext, payload []byte) ([]byte, error) {
	var request simulationv1.SetJointTrajectoryRequest
	if err := decodeMessage(payload, &request); err != nil {
		return nil, err
	}
	if !s.validateName(request.GetModel()) || !s.validateName(request.GetJointGroup()) ||
		len(request.GetPoints()) == 0 || len(request.GetPoints()) > s.config.MaxTrajectoryPoints {
		return nil, plainStatusError(ipcv1.StatusCode_STATUS_CODE_INVALID_ARGUMENT)
	}
	points := make([]simulation.JointTrajectoryPoint, len(request.GetPoints()))
	jointCount := -1
	var previous int64
	for index, point := range request.GetPoints() {
		if point == nil || point.GetTimeFromStartNanos() <= 0 ||
			(index > 0 && point.GetTimeFromStartNanos() <= previous) ||
			time.Duration(point.GetTimeFromStartNanos()) > s.config.MaxTrajectoryDuration ||
			len(point.GetPositions()) == 0 || len(point.GetPositions()) > s.config.MaxJointBatch {
			return nil, plainStatusError(ipcv1.StatusCode_STATUS_CODE_INVALID_ARGUMENT)
		}
		if jointCount < 0 {
			jointCount = len(point.GetPositions())
		} else if len(point.GetPositions()) != jointCount {
			return nil, plainStatusError(ipcv1.StatusCode_STATUS_CODE_INVALID_ARGUMENT)
		}
		for _, position := range point.GetPositions() {
			if math.IsNaN(position) || math.IsInf(position, 0) || !s.validatePosition(position) {
				return nil, plainStatusError(ipcv1.StatusCode_STATUS_CODE_INVALID_ARGUMENT)
			}
		}
		previous = point.GetTimeFromStartNanos()
		points[index] = simulation.JointTrajectoryPoint{
			Positions:     append([]float64(nil), point.GetPositions()...),
			TimeFromStart: point.GetTimeFromStartNanos(),
		}
	}
	backend, err := s.jointMotionBackend()
	if err != nil {
		return nil, err
	}
	return s.runJointMotion(cc, request.GetWorldGeneration(), request.GetModel(), request.GetJointGroup(), func(
		ctx context.Context,
		execution simulation.ControlContext,
	) (simulation.JointMotionReceipt, error) {
		return backend.StartJointTrajectory(ctx, execution, request.GetModel(), request.GetJointGroup(), points)
	})
}

func (s *Service) goHome(cc sdk.CallContext, payload []byte) ([]byte, error) {
	var request simulationv1.GoHomeRequest
	if err := decodeMessage(payload, &request); err != nil {
		return nil, err
	}
	home := request.GetHomeState()
	if home == "" {
		home = "home"
	}
	if !s.validateName(request.GetModel()) || !s.validateName(request.GetJointGroup()) || !s.validateName(home) {
		return nil, plainStatusError(ipcv1.StatusCode_STATUS_CODE_INVALID_ARGUMENT)
	}
	backend, err := s.jointMotionBackend()
	if err != nil {
		return nil, err
	}
	return s.runJointMotion(cc, request.GetWorldGeneration(), request.GetModel(), request.GetJointGroup(), func(
		ctx context.Context,
		execution simulation.ControlContext,
	) (simulation.JointMotionReceipt, error) {
		return backend.StartJointHome(ctx, execution, request.GetModel(), request.GetJointGroup(), home)
	})
}

func (s *Service) runJointMotion(
	cc sdk.CallContext,
	worldGeneration uint64,
	model, group string,
	start func(context.Context, simulation.ControlContext) (simulation.JointMotionReceipt, error),
) ([]byte, error) {
	backend, err := s.jointMotionBackend()
	if err != nil {
		return nil, err
	}
	execution, err := controlContext(cc, worldGeneration)
	if err != nil {
		return nil, err
	}
	opCtx, cancel, err := executionContext(cc.Ctx, execution)
	if err != nil {
		return nil, err
	}
	defer cancel()
	release, err := s.acquireControl(opCtx, execution)
	if err != nil {
		return nil, err
	}
	receipt, err := start(opCtx, execution)
	release()
	if err != nil {
		return nil, backendError(err, "control.start_joint_motion", worldGeneration)
	}
	completed, waitErr := backend.WaitJointMotion(opCtx, receipt.MotionID)
	if waitErr != nil {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 2*time.Second)
		_, _ = backend.CancelJointMotion(stopCtx, execution, model, group)
		stopCancel()
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		_, _ = backend.WaitJointMotion(cleanupCtx, receipt.MotionID)
		cleanupCancel()
		return nil, backendError(waitErr, "control.wait_joint_motion", worldGeneration)
	}
	return marshalJointMotionReceipt(worldGeneration, execution.CommandSequence, completed)
}

func (s *Service) stopJointMotion(cc sdk.CallContext, payload []byte) ([]byte, error) {
	var request simulationv1.StopMotionRequest
	if err := decodeMessage(payload, &request); err != nil {
		return nil, err
	}
	if !s.validateName(request.GetModel()) || !s.validateName(request.GetJointGroup()) {
		return nil, plainStatusError(ipcv1.StatusCode_STATUS_CODE_INVALID_ARGUMENT)
	}
	backend, err := s.jointMotionBackend()
	if err != nil {
		return nil, err
	}
	execution, opCtx, release, cancel, err := s.beginJointControl(cc, request.GetWorldGeneration())
	if err != nil {
		return nil, err
	}
	defer cancel()
	defer release()
	receipt, err := backend.CancelJointMotion(opCtx, execution, request.GetModel(), request.GetJointGroup())
	if err != nil {
		return nil, backendError(err, "control.stop_joint_motion", request.GetWorldGeneration())
	}
	return marshalJointMotionReceipt(request.GetWorldGeneration(), execution.CommandSequence, receipt)
}

func (s *Service) stopAllMotion(cc sdk.CallContext, payload []byte) ([]byte, error) {
	var request simulationv1.StopAllMotionRequest
	if err := decodeMessage(payload, &request); err != nil {
		return nil, err
	}
	backend, err := s.jointMotionBackend()
	if err != nil {
		return nil, err
	}
	execution, opCtx, release, cancel, err := s.beginJointControl(cc, request.GetWorldGeneration())
	if err != nil {
		return nil, err
	}
	defer cancel()
	defer release()
	receipt, err := backend.StopAllMotion(opCtx, execution)
	if err != nil {
		return nil, backendError(err, "control.stop_all_motion", request.GetWorldGeneration())
	}
	return marshalJointMotionReceipt(request.GetWorldGeneration(), execution.CommandSequence, receipt)
}

func (s *Service) beginJointControl(
	cc sdk.CallContext,
	worldGeneration uint64,
) (simulation.ControlContext, context.Context, func(), context.CancelFunc, error) {
	execution, err := controlContext(cc, worldGeneration)
	if err != nil {
		return simulation.ControlContext{}, nil, nil, nil, err
	}
	opCtx, cancel, err := executionContext(cc.Ctx, execution)
	if err != nil {
		return simulation.ControlContext{}, nil, nil, nil, err
	}
	release, err := s.acquireControl(opCtx, execution)
	if err != nil {
		cancel()
		return simulation.ControlContext{}, nil, nil, nil, err
	}
	return execution, opCtx, release, cancel, nil
}

func marshalJointMotionReceipt(
	worldGeneration, commandSequence uint64,
	receipt simulation.JointMotionReceipt,
) ([]byte, error) {
	return proto.Marshal(&simulationv1.JointMotionReceipt{
		WorldGeneration:   worldGeneration,
		CommandSequence:   commandSequence,
		MotionId:          receipt.MotionID,
		ControlledJoints:  receipt.ControlledJoints,
		Accepted:          receipt.Accepted,
		FeedbackAvailable: receipt.FeedbackAvailable,
		Converged:         receipt.Converged,
	})
}
