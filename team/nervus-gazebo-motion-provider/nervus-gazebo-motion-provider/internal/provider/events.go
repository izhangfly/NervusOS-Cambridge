package provider

import (
	"context"

	simulationv1 "github.com/nervus-os/nervus-gazebo-provider/gen/go/gazebo/simulation/provider/v1"
	"github.com/nervus-os/nervus-gazebo-provider/internal/simulation"
	"google.golang.org/protobuf/proto"
)

func (s *Service) handleEvent(ctx context.Context, event simulation.Event) error {
	switch event.Kind {
	case simulation.EventStatusChanged:
		return s.applyStatus(ctx, event.Status, event.MonotonicNanos)

	case simulation.EventProcessExited:
		payload, err := proto.Marshal(&simulationv1.ProcessExited{
			ProcessId:       event.ProcessExit.ProcessID,
			ExitCode:        event.ProcessExit.ExitCode,
			Expected:        event.ProcessExit.Expected,
			WorldGeneration: event.ProcessExit.WorldGeneration,
		})
		if err != nil {
			return err
		}
		s.publishManagerBestEffort(
			uint32(simulationv1.ManagerEvent_MANAGER_EVENT_PROCESS_EXITED),
			payload,
			event.MonotonicNanos,
		)

	case simulation.EventTransportFault:
		payload, err := proto.Marshal(&simulationv1.TransportFault{
			Kind:            transportFaultToWire(event.TransportFault.Kind),
			Reason:          reasonToWire(event.TransportFault.Reason),
			Retryable:       event.TransportFault.Retryable,
			WorldGeneration: event.TransportFault.WorldGeneration,
			DroppedEvents:   event.TransportFault.DroppedEvents,
		})
		if err != nil {
			return err
		}
		s.publishManagerBestEffort(
			uint32(simulationv1.ManagerEvent_MANAGER_EVENT_TRANSPORT_FAULT),
			payload,
			event.MonotonicNanos,
		)

	case simulation.EventWSLgFault:
		payload, err := proto.Marshal(&simulationv1.WSLgFault{
			Kind:            wslgFaultToWire(event.WSLgFault.Kind),
			Retryable:       event.WSLgFault.Retryable,
			WorldGeneration: event.WSLgFault.WorldGeneration,
		})
		if err != nil {
			return err
		}
		s.publishManagerBestEffort(
			uint32(simulationv1.ManagerEvent_MANAGER_EVENT_WSLG_FAULT),
			payload,
			event.MonotonicNanos,
		)

	case simulation.EventWorldChanged:
		payload, err := proto.Marshal(&simulationv1.WorldChanged{
			PreviousWorld:      event.WorldChanged.PreviousWorld,
			PreviousGeneration: event.WorldChanged.PreviousGeneration,
			CurrentWorld:       event.WorldChanged.CurrentWorld,
			CurrentGeneration:  event.WorldChanged.CurrentGeneration,
		})
		if err != nil {
			return err
		}
		s.publishManagerBestEffort(
			uint32(simulationv1.ManagerEvent_MANAGER_EVENT_WORLD_CHANGED),
			payload,
			event.MonotonicNanos,
		)

	case simulation.EventJointStateChanged:
		payload, err := proto.Marshal(jointStateToWire(event.JointState))
		if err != nil {
			return err
		}
		s.publishControlBestEffort(
			uint32(simulationv1.ControlEvent_CONTROL_EVENT_JOINT_STATE_CHANGED),
			payload,
			event.MonotonicNanos,
		)

	case simulation.EventCommandRejected:
		payload, err := proto.Marshal(&simulationv1.CommandRejected{
			WorldGeneration: event.CommandRejected.WorldGeneration,
			CommandSequence: event.CommandRejected.CommandSequence,
			CommandIndex:    event.CommandRejected.CommandIndex,
			Reason:          reasonToWire(event.CommandRejected.Reason),
			Model:           event.CommandRejected.Model,
			Joint:           event.CommandRejected.Joint,
			Axis:            event.CommandRejected.Axis,
		})
		if err != nil {
			return err
		}
		s.publishControlBestEffort(
			uint32(simulationv1.ControlEvent_CONTROL_EVENT_COMMAND_REJECTED),
			payload,
			event.MonotonicNanos,
		)

	default:
		s.log.Warn("gazebo provider: ignored unknown backend event", "kind", event.Kind)
	}
	return nil
}

func (s *Service) publishManagerBestEffort(eventID uint32, payload []byte, timestamp uint64) {
	if err := s.publishManager(eventID, payload, timestamp); err != nil {
		s.log.Debug("gazebo provider: manager event publish failed", "event_id", eventID, "err", err)
	}
}

func (s *Service) publishControlBestEffort(eventID uint32, payload []byte, timestamp uint64) {
	if err := s.publishControl(eventID, payload, timestamp); err != nil {
		s.log.Debug("gazebo provider: control event publish skipped", "event_id", eventID, "err", err)
	}
}

func transportFaultToWire(kind simulation.TransportFaultKind) simulationv1.TransportFaultKind {
	switch kind {
	case simulation.TransportFaultDisconnected:
		return simulationv1.TransportFaultKind_TRANSPORT_FAULT_KIND_DISCONNECTED
	case simulation.TransportFaultDiscoveryFailed:
		return simulationv1.TransportFaultKind_TRANSPORT_FAULT_KIND_DISCOVERY_FAILED
	case simulation.TransportFaultTypeMismatch:
		return simulationv1.TransportFaultKind_TRANSPORT_FAULT_KIND_TYPE_MISMATCH
	case simulation.TransportFaultRequestTimeout:
		return simulationv1.TransportFaultKind_TRANSPORT_FAULT_KIND_REQUEST_TIMEOUT
	case simulation.TransportFaultRemoteRejected:
		return simulationv1.TransportFaultKind_TRANSPORT_FAULT_KIND_REMOTE_REJECTED
	case simulation.TransportFaultQueueOverflow:
		return simulationv1.TransportFaultKind_TRANSPORT_FAULT_KIND_QUEUE_OVERFLOW
	case simulation.TransportFaultNativeFailure:
		return simulationv1.TransportFaultKind_TRANSPORT_FAULT_KIND_NATIVE_FAILURE
	default:
		return simulationv1.TransportFaultKind_TRANSPORT_FAULT_KIND_UNSPECIFIED
	}
}

func wslgFaultToWire(kind simulation.WSLgFaultKind) simulationv1.WSLgFaultKind {
	switch kind {
	case simulation.WSLgFaultKeepaliveMissing:
		return simulationv1.WSLgFaultKind_WSLG_FAULT_KIND_KEEPALIVE_MISSING
	case simulation.WSLgFaultCopyMode:
		return simulationv1.WSLgFaultKind_WSLG_FAULT_KIND_COPY_MODE
	case simulation.WSLgFaultDisplayUnavailable:
		return simulationv1.WSLgFaultKind_WSLG_FAULT_KIND_DISPLAY_UNAVAILABLE
	case simulation.WSLgFaultSharedMemoryUnavailable:
		return simulationv1.WSLgFaultKind_WSLG_FAULT_KIND_SHARED_MEMORY_UNAVAILABLE
	default:
		return simulationv1.WSLgFaultKind_WSLG_FAULT_KIND_UNSPECIFIED
	}
}
