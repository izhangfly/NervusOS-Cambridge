package provider

import (
	simulationv1 "github.com/nervus-os/nervus-gazebo-provider/gen/go/gazebo/simulation/provider/v1"
	"github.com/nervus-os/nervus-gazebo-provider/internal/simulation"
	"google.golang.org/protobuf/proto"
)

func marshalStatus(status simulation.Status) ([]byte, error) {
	return proto.Marshal(statusToWire(status))
}

func statusToWire(status simulation.Status) *simulationv1.SimulationStatus {
	return &simulationv1.SimulationStatus{
		Mode:             modeToWire(status.Mode),
		ProcessState:     processToWire(status.Process),
		TransportState:   transportToWire(status.Transport),
		SimulationState:  runToWire(status.Run),
		WorldName:        status.WorldName,
		WorldGeneration:  status.WorldGeneration,
		ProcessOwned:     status.ProcessOwned,
		ManagedProcessId: status.ManagedProcessID,
		HasLastExitCode:  status.HasLastExitCode,
		LastExitCode:     status.LastExitCode,
		Statistics: &simulationv1.SimulationStatistics{
			Valid:            status.Statistics.Valid,
			SimulationTimeNs: status.Statistics.SimulationTimeNS,
			RealTimeNs:       status.Statistics.RealTimeNS,
			Iterations:       status.Statistics.Iterations,
			RealTimeFactor:   status.Statistics.RealTimeFactor,
			SampleAgeMs:      status.Statistics.SampleAgeMS,
		},
		LastFault: reasonToWire(status.LastFault),
		Versions: &simulationv1.RuntimeVersions{
			GazeboSim:       status.Versions.GazeboSim,
			GazeboTransport: status.Versions.GazeboTransport,
			GazeboMsgs:      status.Versions.GazeboMessages,
			Adapter:         status.Versions.Adapter,
		},
	}
}

func modeToWire(mode simulation.Mode) simulationv1.ProviderMode {
	switch mode {
	case simulation.ModeAttach:
		return simulationv1.ProviderMode_PROVIDER_MODE_ATTACH
	case simulation.ModeManaged:
		return simulationv1.ProviderMode_PROVIDER_MODE_MANAGED
	default:
		return simulationv1.ProviderMode_PROVIDER_MODE_UNSPECIFIED
	}
}

func processToWire(state simulation.ProcessState) simulationv1.ProcessState {
	switch state {
	case simulation.ProcessExternal:
		return simulationv1.ProcessState_PROCESS_STATE_EXTERNAL
	case simulation.ProcessStopped:
		return simulationv1.ProcessState_PROCESS_STATE_STOPPED
	case simulation.ProcessStarting:
		return simulationv1.ProcessState_PROCESS_STATE_STARTING
	case simulation.ProcessRunning:
		return simulationv1.ProcessState_PROCESS_STATE_RUNNING
	case simulation.ProcessStopping:
		return simulationv1.ProcessState_PROCESS_STATE_STOPPING
	case simulation.ProcessExited:
		return simulationv1.ProcessState_PROCESS_STATE_EXITED
	default:
		return simulationv1.ProcessState_PROCESS_STATE_UNSPECIFIED
	}
}

func transportToWire(state simulation.TransportState) simulationv1.TransportState {
	switch state {
	case simulation.TransportDisconnected:
		return simulationv1.TransportState_TRANSPORT_STATE_DISCONNECTED
	case simulation.TransportDiscovering:
		return simulationv1.TransportState_TRANSPORT_STATE_DISCOVERING
	case simulation.TransportReady:
		return simulationv1.TransportState_TRANSPORT_STATE_READY
	case simulation.TransportDegraded:
		return simulationv1.TransportState_TRANSPORT_STATE_DEGRADED
	default:
		return simulationv1.TransportState_TRANSPORT_STATE_UNSPECIFIED
	}
}

func runToWire(state simulation.RunState) simulationv1.SimulationState {
	switch state {
	case simulation.RunRunning:
		return simulationv1.SimulationState_SIMULATION_STATE_RUNNING
	case simulation.RunPaused:
		return simulationv1.SimulationState_SIMULATION_STATE_PAUSED
	default:
		return simulationv1.SimulationState_SIMULATION_STATE_UNSPECIFIED
	}
}

func capabilitiesToWire(capabilities simulation.Capabilities) *simulationv1.SimulationCapabilities {
	return &simulationv1.SimulationCapabilities{
		Mode:                       modeToWire(capabilities.Mode),
		CanManageProcess:           capabilities.CanManageProcess,
		CanLoadWorld:               capabilities.CanLoadWorld,
		CanResetWorld:              capabilities.CanResetWorld,
		CanControlWorld:            capabilities.CanControlWorld,
		CanQueryScene:              capabilities.CanQueryScene,
		CanCommandJoints:           capabilities.CanCommandJoints,
		JointStateFeedback:         capabilities.JointStateFeedback,
		SensorTopics:               capabilities.SensorTopics,
		AllowedWorlds:              append([]string(nil), capabilities.AllowedWorlds...),
		MaxStepIterations:          capabilities.MaxStepIterations,
		MaxJointCommandsPerRequest: capabilities.MaxJointCommandsPerRequest,
	}
}

func modelListToWire(list simulation.ModelList) *simulationv1.ModelList {
	out := &simulationv1.ModelList{
		WorldName:       list.WorldName,
		WorldGeneration: list.WorldGeneration,
		Models:          make([]*simulationv1.ModelSummary, 0, len(list.Models)),
	}
	for _, model := range list.Models {
		out.Models = append(out.Models, &simulationv1.ModelSummary{
			Name:          model.Name,
			EntityId:      model.EntityID,
			IsStatic:      model.Static,
			CanonicalLink: model.CanonicalLink,
		})
	}
	return out
}

func jointListToWire(list simulation.JointList) *simulationv1.JointList {
	out := &simulationv1.JointList{
		WorldName:       list.WorldName,
		WorldGeneration: list.WorldGeneration,
		Model:           list.Model,
		Joints:          make([]*simulationv1.JointSummary, 0, len(list.Joints)),
	}
	for _, joint := range list.Joints {
		item := &simulationv1.JointSummary{
			Name:         joint.Name,
			Type:         joint.Type,
			Controllable: joint.Controllable,
			Axes:         make([]*simulationv1.JointAxisDescription, 0, len(joint.Axes)),
		}
		for _, axis := range joint.Axes {
			item.Axes = append(item.Axes, &simulationv1.JointAxisDescription{
				Axis:              axis.Axis,
				HasPositionLimits: axis.HasPositionLimits,
				LowerPosition:     axis.LowerPosition,
				UpperPosition:     axis.UpperPosition,
			})
		}
		out.Joints = append(out.Joints, item)
	}
	return out
}

func jointStateToWire(state simulation.JointState) *simulationv1.JointState {
	return &simulationv1.JointState{
		WorldName:              state.WorldName,
		WorldGeneration:        state.WorldGeneration,
		Model:                  state.Model,
		Joint:                  state.Joint,
		Axis:                   state.Axis,
		HasPosition:            state.HasPosition,
		Position:               state.Position,
		HasVelocity:            state.HasVelocity,
		Velocity:               state.Velocity,
		SampleSimulationTimeNs: state.SampleSimulationTimeNS,
		Sequence:               state.Sequence,
	}
}
