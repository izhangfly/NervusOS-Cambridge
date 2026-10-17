package provider

import (
	"errors"
	"fmt"
	"math"

	"github.com/nervus-os/nervus-gazebo-provider/internal/contract"
	"github.com/nervus-os/nervus-gazebo-provider/internal/simulation"
	basemotionv1 "github.com/nervus-os/nervus-ipc/protocol/interface/basemotionv1"
	"github.com/nervus-os/nervus-ipc/protocol/ipcv1"
	"github.com/nervus-os/nervus-ipc/sdk"
	"google.golang.org/protobuf/proto"
)

func (s *Service) installBaseHandlers() error {
	handlers := map[basemotionv1.BaseMotionMethod]sdk.Handler{
		basemotionv1.BaseMotionMethod_BASE_MOTION_METHOD_SET_VELOCITY:           s.setVelocity,
		basemotionv1.BaseMotionMethod_BASE_MOTION_METHOD_STOP:                   s.stopBase,
		basemotionv1.BaseMotionMethod_BASE_MOTION_METHOD_GET_MOTION_STATE:       s.getMotionState,
		basemotionv1.BaseMotionMethod_BASE_MOTION_METHOD_SUBSCRIBE_MOTION_STATE: s.subscribeMotionState,
	}
	for method, handler := range handlers {
		if err := s.base.Handle(uint32(method), s.track(handler)); err != nil {
			return fmt.Errorf("provider: install BaseMotion method %s: %w", method, err)
		}
	}
	return nil
}

func (s *Service) baseBackend() (simulation.BaseMotionBackend, error) {
	backend, ok := s.backend.(simulation.BaseMotionBackend)
	if !ok {
		return nil, plainStatusError(ipcv1.StatusCode_STATUS_CODE_UNAVAILABLE)
	}
	return backend, nil
}

func (s *Service) setVelocity(cc sdk.CallContext, payload []byte) ([]byte, error) {
	var request basemotionv1.SetVelocityRequest
	if err := decodeMessage(payload, &request); err != nil {
		return nil, err
	}
	if math.IsNaN(request.GetLinearX()) || math.IsInf(request.GetLinearX(), 0) || math.IsNaN(request.GetLinearY()) || math.IsInf(request.GetLinearY(), 0) || math.IsNaN(request.GetAngularZ()) || math.IsInf(request.GetAngularZ(), 0) {
		return nil, plainStatusError(ipcv1.StatusCode_STATUS_CODE_INVALID_ARGUMENT)
	}
	backend, err := s.baseBackend()
	if err != nil {
		return nil, err
	}
	state, err := backend.BaseMotionState(cc.Ctx, contract.BaseResourceRole)
	if err != nil {
		return nil, baseBackendError(err)
	}
	execution, err := physicalControlContext(cc, state.WorldGeneration, contract.BaseResourceRole, contract.BaseControlPermission)
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
	defer release()
	if err := backend.SetBaseVelocity(opCtx, execution, simulation.BaseVelocity{LinearX: request.GetLinearX(), LinearY: request.GetLinearY(), AngularZ: request.GetAngularZ()}); err != nil {
		return nil, baseBackendError(err)
	}
	return nil, nil
}

func (s *Service) stopBase(cc sdk.CallContext, payload []byte) ([]byte, error) {
	var request basemotionv1.StopRequest
	if err := decodeMessage(payload, &request); err != nil {
		return nil, err
	}
	backend, err := s.baseBackend()
	if err != nil {
		return nil, err
	}
	state, err := backend.BaseMotionState(cc.Ctx, contract.BaseResourceRole)
	if err != nil {
		return nil, baseBackendError(err)
	}
	execution, err := physicalControlContext(cc, state.WorldGeneration, contract.BaseResourceRole, contract.BaseControlPermission)
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
	defer release()
	if err := backend.StopBaseMotion(opCtx, execution); err != nil {
		return nil, baseBackendError(err)
	}
	return nil, nil
}

func (s *Service) getMotionState(cc sdk.CallContext, payload []byte) ([]byte, error) {
	var request basemotionv1.GetMotionStateRequest
	if err := decodeMessage(payload, &request); err != nil {
		return nil, err
	}
	backend, err := s.baseBackend()
	if err != nil {
		return nil, err
	}
	state, err := backend.BaseMotionState(cc.Ctx, contract.BaseResourceRole)
	if err != nil {
		return nil, baseBackendError(err)
	}
	return proto.Marshal(baseMotionStateToWire(state))
}

func (s *Service) subscribeMotionState(cc sdk.CallContext, payload []byte) ([]byte, error) {
	var request basemotionv1.SubscribeMotionStateRequest
	if err := decodeMessage(payload, &request); err != nil {
		return nil, err
	}
	return s.getMotionState(cc, payload)
}

func baseMotionStateToWire(state simulation.BaseMotionState) *basemotionv1.MotionState {
	readiness := basemotionv1.Readiness_READINESS_NOT_READY
	if state.Fault {
		readiness = basemotionv1.Readiness_READINESS_FAULT
	} else if state.Ready {
		readiness = basemotionv1.Readiness_READINESS_READY
	}
	return &basemotionv1.MotionState{Readiness: readiness, SupportsLinearX: state.SupportsLinearX, SupportsLinearY: state.SupportsLinearY, SupportsAngularZ: state.SupportsAngularZ, VelocityLimits: &basemotionv1.VelocityLimits{MaxLinear: state.MaxLinear, MaxAngular: state.MaxAngular}, ActiveMotionEpoch: state.ActiveMotionEpoch}
}

func baseBackendError(err error) error {
	if err == nil {
		return nil
	}
	var domain *simulation.Error
	if !errors.As(err, &domain) {
		return err
	}
	reason := basemotionv1.BaseMotionReason_BASE_MOTION_REASON_RESOURCE_FAULT
	switch domain.Reason {
	case simulation.ReasonFeedbackUnavailable, simulation.ReasonWorldNotReady:
		reason = basemotionv1.BaseMotionReason_BASE_MOTION_REASON_LOCOMOTION_NOT_READY
	case simulation.ReasonWorldChanged:
		reason = basemotionv1.BaseMotionReason_BASE_MOTION_REASON_STALE_EPOCH
	}
	detail, _ := proto.Marshal(&basemotionv1.BaseMotionErrorDetail{Reason: reason})
	code := ipcv1.StatusCode_STATUS_CODE_UNAVAILABLE
	if domain.Reason == simulation.ReasonOutOfRange {
		code = ipcv1.StatusCode_STATUS_CODE_INVALID_ARGUMENT
	}
	return &sdk.StatusError{Code: code, Detail: detail}
}
