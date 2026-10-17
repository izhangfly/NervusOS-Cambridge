package provider

import (
	"context"
	"errors"

	simulationv1 "github.com/nervus-os/nervus-gazebo-provider/gen/go/gazebo/simulation/provider/v1"
	"github.com/nervus-os/nervus-gazebo-provider/internal/simulation"
	"github.com/nervus-os/nervus-ipc/protocol/ipcv1"
	"github.com/nervus-os/nervus-ipc/sdk"
	"google.golang.org/protobuf/proto"
)

func backendError(err error, operation string, worldGeneration uint64) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	var domain *simulation.Error
	if !errors.As(err, &domain) {
		return err
	}
	return statusError(domain.Reason, operation, worldGeneration, domain.Retryable)
}

func statusError(
	reason simulation.Reason,
	operation string,
	worldGeneration uint64,
	retryable bool,
) error {
	detail, err := proto.Marshal(&simulationv1.SimulationErrorDetail{
		Reason:          reasonToWire(reason),
		Operation:       operation,
		WorldGeneration: worldGeneration,
		Retryable:       retryable,
	})
	if err != nil {
		detail = nil
	}
	return &sdk.StatusError{
		Code:   statusCodeForReason(reason),
		Detail: detail,
	}
}

func plainStatusError(code ipcv1.StatusCode) error {
	return &sdk.StatusError{Code: code}
}

func statusCodeForReason(reason simulation.Reason) ipcv1.StatusCode {
	switch reason {
	case simulation.ReasonOutOfRange,
		simulation.ReasonBatchTooLarge:
		return ipcv1.StatusCode_STATUS_CODE_INVALID_ARGUMENT
	case simulation.ReasonWorldNotFound,
		simulation.ReasonModelNotFound,
		simulation.ReasonJointNotFound:
		return ipcv1.StatusCode_STATUS_CODE_NOT_FOUND
	case simulation.ReasonProviderNotReady,
		simulation.ReasonWorldNotReady,
		simulation.ReasonTransportDisconnected,
		simulation.ReasonServiceNotFound,
		simulation.ReasonTopicNotFound,
		simulation.ReasonFeedbackUnavailable,
		simulation.ReasonWSLgNotReady,
		simulation.ReasonProcessExited:
		return ipcv1.StatusCode_STATUS_CODE_UNAVAILABLE
	case simulation.ReasonRequestTimeout:
		return ipcv1.StatusCode_STATUS_CODE_DEADLINE_EXCEEDED
	case simulation.ReasonQueueOverflow:
		return ipcv1.StatusCode_STATUS_CODE_RESOURCE_EXHAUSTED
	case simulation.ReasonProcessExternallyManaged,
		simulation.ReasonProcessAlreadyRunning,
		simulation.ReasonProcessNotRunning,
		simulation.ReasonWorldChanged,
		simulation.ReasonTypeMismatch,
		simulation.ReasonRemoteRejected,
		simulation.ReasonJointNotControllable,
		simulation.ReasonAxisNotSupported,
		simulation.ReasonInvalidState:
		return ipcv1.StatusCode_STATUS_CODE_FAILED_PRECONDITION
	case simulation.ReasonNativeFailure,
		simulation.ReasonInternal,
		simulation.ReasonUnspecified:
		return ipcv1.StatusCode_STATUS_CODE_INTERNAL
	default:
		return ipcv1.StatusCode_STATUS_CODE_INTERNAL
	}
}

func reasonToWire(reason simulation.Reason) simulationv1.SimulationErrorReason {
	switch reason {
	case simulation.ReasonProviderNotReady:
		return simulationv1.SimulationErrorReason_SIMULATION_ERROR_REASON_PROVIDER_NOT_READY
	case simulation.ReasonProcessExternallyManaged:
		return simulationv1.SimulationErrorReason_SIMULATION_ERROR_REASON_PROCESS_EXTERNALLY_MANAGED
	case simulation.ReasonProcessAlreadyRunning:
		return simulationv1.SimulationErrorReason_SIMULATION_ERROR_REASON_PROCESS_ALREADY_RUNNING
	case simulation.ReasonProcessNotRunning:
		return simulationv1.SimulationErrorReason_SIMULATION_ERROR_REASON_PROCESS_NOT_RUNNING
	case simulation.ReasonWorldNotFound:
		return simulationv1.SimulationErrorReason_SIMULATION_ERROR_REASON_WORLD_NOT_FOUND
	case simulation.ReasonWorldNotReady:
		return simulationv1.SimulationErrorReason_SIMULATION_ERROR_REASON_WORLD_NOT_READY
	case simulation.ReasonWorldChanged:
		return simulationv1.SimulationErrorReason_SIMULATION_ERROR_REASON_WORLD_CHANGED
	case simulation.ReasonTransportDisconnected:
		return simulationv1.SimulationErrorReason_SIMULATION_ERROR_REASON_TRANSPORT_DISCONNECTED
	case simulation.ReasonServiceNotFound:
		return simulationv1.SimulationErrorReason_SIMULATION_ERROR_REASON_SERVICE_NOT_FOUND
	case simulation.ReasonTopicNotFound:
		return simulationv1.SimulationErrorReason_SIMULATION_ERROR_REASON_TOPIC_NOT_FOUND
	case simulation.ReasonTypeMismatch:
		return simulationv1.SimulationErrorReason_SIMULATION_ERROR_REASON_TYPE_MISMATCH
	case simulation.ReasonRequestTimeout:
		return simulationv1.SimulationErrorReason_SIMULATION_ERROR_REASON_REQUEST_TIMEOUT
	case simulation.ReasonRemoteRejected:
		return simulationv1.SimulationErrorReason_SIMULATION_ERROR_REASON_REMOTE_REJECTED
	case simulation.ReasonModelNotFound:
		return simulationv1.SimulationErrorReason_SIMULATION_ERROR_REASON_MODEL_NOT_FOUND
	case simulation.ReasonJointNotFound:
		return simulationv1.SimulationErrorReason_SIMULATION_ERROR_REASON_JOINT_NOT_FOUND
	case simulation.ReasonJointNotControllable:
		return simulationv1.SimulationErrorReason_SIMULATION_ERROR_REASON_JOINT_NOT_CONTROLLABLE
	case simulation.ReasonAxisNotSupported:
		return simulationv1.SimulationErrorReason_SIMULATION_ERROR_REASON_AXIS_NOT_SUPPORTED
	case simulation.ReasonOutOfRange:
		return simulationv1.SimulationErrorReason_SIMULATION_ERROR_REASON_OUT_OF_RANGE
	case simulation.ReasonFeedbackUnavailable:
		return simulationv1.SimulationErrorReason_SIMULATION_ERROR_REASON_FEEDBACK_UNAVAILABLE
	case simulation.ReasonQueueOverflow:
		return simulationv1.SimulationErrorReason_SIMULATION_ERROR_REASON_QUEUE_OVERFLOW
	case simulation.ReasonWSLgNotReady:
		return simulationv1.SimulationErrorReason_SIMULATION_ERROR_REASON_WSLG_NOT_READY
	case simulation.ReasonNativeFailure:
		return simulationv1.SimulationErrorReason_SIMULATION_ERROR_REASON_NATIVE_FAILURE
	case simulation.ReasonInvalidState:
		return simulationv1.SimulationErrorReason_SIMULATION_ERROR_REASON_INVALID_STATE
	case simulation.ReasonBatchTooLarge:
		return simulationv1.SimulationErrorReason_SIMULATION_ERROR_REASON_BATCH_TOO_LARGE
	case simulation.ReasonProcessExited:
		return simulationv1.SimulationErrorReason_SIMULATION_ERROR_REASON_PROCESS_EXITED
	case simulation.ReasonInternal:
		return simulationv1.SimulationErrorReason_SIMULATION_ERROR_REASON_INTERNAL
	default:
		return simulationv1.SimulationErrorReason_SIMULATION_ERROR_REASON_UNSPECIFIED
	}
}
