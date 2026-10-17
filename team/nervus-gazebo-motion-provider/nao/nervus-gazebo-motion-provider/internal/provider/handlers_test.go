package provider

import (
	"math"
	"testing"

	simulationv1 "github.com/nervus-os/nervus-gazebo-provider/gen/go/gazebo/simulation/provider/v1"
	"github.com/nervus-os/nervus-gazebo-provider/internal/contract"
	"github.com/nervus-os/nervus-gazebo-provider/internal/simulation"
	"github.com/nervus-os/nervus-ipc/protocol/ipcv1"
	"google.golang.org/protobuf/proto"
)

func TestManagerHandlersFailClosedOnPermissionAndUnknownFields(t *testing.T) {
	host := newFakeHost()
	backend := newFakeBackend(degradedStatus(1), host)
	_, err := New(host, backend, DefaultConfig(), testLogger())
	if err != nil {
		t.Fatal(err)
	}
	manager := host.endpoint(contract.ManagerInterface)

	_, err = manager.invoke(
		uint32(simulationv1.ManagerMethod_MANAGER_METHOD_GET_STATUS),
		callContext(),
		nil,
	)
	requireStatusError(t, err, ipcv1.StatusCode_STATUS_CODE_PERMISSION_DENIED)
	if got := backend.calls("status"); got != 0 {
		t.Fatalf("backend Status called %d times without permission", got)
	}

	// Field 99 is not in GetStatusRequest. Direct handler invocation must still
	// reject it even though nervud normally performs schema validation first.
	unknownField := []byte{0x98, 0x06, 0x01}
	_, err = manager.invoke(
		uint32(simulationv1.ManagerMethod_MANAGER_METHOD_GET_STATUS),
		queryCallContext(),
		unknownField,
	)
	requireStatusError(t, err, ipcv1.StatusCode_STATUS_CODE_INVALID_ARGUMENT)

	payload, err := manager.invoke(
		uint32(simulationv1.ManagerMethod_MANAGER_METHOD_GET_STATUS),
		queryCallContext(),
		nil,
	)
	if err != nil {
		t.Fatalf("GetStatus: %v", err)
	}
	var status simulationv1.SimulationStatus
	if err := proto.Unmarshal(payload, &status); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if status.GetTransportState() != simulationv1.TransportState_TRANSPORT_STATE_DEGRADED {
		t.Fatalf("transport state = %v", status.GetTransportState())
	}
}

func TestLifecycleMethodsRequireDeclaredLifecyclePermission(t *testing.T) {
	host := newFakeHost()
	backend := newFakeBackend(degradedStatus(1), host)
	backend.startStatus = degradedStatus(2)
	_, err := New(host, backend, DefaultConfig(), testLogger())
	if err != nil {
		t.Fatal(err)
	}
	manager := host.endpoint(contract.ManagerInterface)
	request, _ := proto.Marshal(&simulationv1.StartSimulationRequest{World: "nervus_home_nao"})

	_, err = manager.invoke(
		uint32(simulationv1.ManagerMethod_MANAGER_METHOD_START_SIMULATION),
		callContext(contract.QueryPermission),
		request,
	)
	requireStatusError(t, err, ipcv1.StatusCode_STATUS_CODE_PERMISSION_DENIED)

	payload, err := manager.invoke(
		uint32(simulationv1.ManagerMethod_MANAGER_METHOD_START_SIMULATION),
		callContext(contract.LifecyclePermission),
		request,
	)
	if err != nil {
		t.Fatalf("StartSimulation: %v", err)
	}
	var result simulationv1.LifecycleResult
	if err := proto.Unmarshal(payload, &result); err != nil {
		t.Fatalf("decode LifecycleResult: %v", err)
	}
	if result.GetStatus().GetWorldGeneration() != 2 {
		t.Fatalf("world generation = %d, want 2", result.GetStatus().GetWorldGeneration())
	}
	backend.mu.Lock()
	started := backend.lastStart
	backend.mu.Unlock()
	if started.World != "nervus_home_nao" {
		t.Fatalf("backend Start world = %q", started.World)
	}
}

func TestControlRequiresLeaseAndRejectsReplayedSequence(t *testing.T) {
	host := newFakeHost()
	backend := newFakeBackend(degradedStatus(4), host)
	backend.controlStatus = degradedStatus(4)
	_, err := New(host, backend, DefaultConfig(), testLogger())
	if err != nil {
		t.Fatal(err)
	}
	control := host.endpoint(contract.ControlInterface)
	request, _ := proto.Marshal(&simulationv1.PauseRequest{WorldGeneration: 4})

	_, err = control.invoke(
		uint32(simulationv1.ControlMethod_CONTROL_METHOD_PAUSE),
		queryCallContext(),
		request,
	)
	requireStatusError(t, err, ipcv1.StatusCode_STATUS_CODE_PERMISSION_DENIED)

	_, err = control.invoke(
		uint32(simulationv1.ControlMethod_CONTROL_METHOD_PAUSE),
		callContext(contract.ControlPermission),
		request,
	)
	requireStatusError(t, err, ipcv1.StatusCode_STATUS_CODE_FAILED_PRECONDITION)

	cc := controlCallContext(7, 30)
	payload, err := control.invoke(
		uint32(simulationv1.ControlMethod_CONTROL_METHOD_PAUSE),
		cc,
		request,
	)
	if err != nil {
		t.Fatalf("Pause: %v", err)
	}
	var result simulationv1.WorldControlResult
	if err := proto.Unmarshal(payload, &result); err != nil {
		t.Fatalf("decode WorldControlResult: %v", err)
	}
	backend.mu.Lock()
	forwarded := backend.lastControl
	backend.mu.Unlock()
	if forwarded.CommandSequence != 7 || forwarded.ResourceGeneration != 30 ||
		forwarded.WorldGeneration != 4 || forwarded.MotionEpoch != 20 ||
		forwarded.DeadlineNanos != math.MaxInt64 ||
		forwarded.Controller != simulation.ControllerHuman {
		t.Fatalf("unexpected forwarded execution context: %+v", forwarded)
	}

	_, err = control.invoke(
		uint32(simulationv1.ControlMethod_CONTROL_METHOD_PAUSE),
		cc,
		request,
	)
	requireStatusError(t, err, ipcv1.StatusCode_STATUS_CODE_FAILED_PRECONDITION)
	if got := backend.calls("pause"); got != 1 {
		t.Fatalf("backend Pause calls = %d, replay reached backend", got)
	}

	// A new Catalog resource generation starts its own monotonic sequence.
	_, err = control.invoke(
		uint32(simulationv1.ControlMethod_CONTROL_METHOD_PAUSE),
		controlCallContext(1, 31),
		request,
	)
	if err != nil {
		t.Fatalf("Pause after resource generation change: %v", err)
	}
	_, err = control.invoke(
		uint32(simulationv1.ControlMethod_CONTROL_METHOD_PAUSE),
		controlCallContext(99, 30),
		request,
	)
	requireStatusError(t, err, ipcv1.StatusCode_STATUS_CODE_FAILED_PRECONDITION)

	staleEpoch := controlCallContext(2, 32)
	staleEpoch.Execution.MotionEpoch = 19
	_, err = control.invoke(
		uint32(simulationv1.ControlMethod_CONTROL_METHOD_PAUSE),
		staleEpoch,
		request,
	)
	requireStatusError(t, err, ipcv1.StatusCode_STATUS_CODE_FAILED_PRECONDITION)
}

func TestControlRejectsExpiredAbsoluteDeadline(t *testing.T) {
	now, err := monotonicNowNanos()
	if err != nil {
		t.Skipf("CLOCK_MONOTONIC unavailable on this platform: %v", err)
	}
	host := newFakeHost()
	backend := newFakeBackend(degradedStatus(4), host)
	_, err = New(host, backend, DefaultConfig(), testLogger())
	if err != nil {
		t.Fatal(err)
	}
	request, _ := proto.Marshal(&simulationv1.PauseRequest{WorldGeneration: 4})
	cc := controlCallContext(1, 30)
	cc.Execution.DeadlineNanos = now - 1
	_, err = host.endpoint(contract.ControlInterface).invoke(
		uint32(simulationv1.ControlMethod_CONTROL_METHOD_PAUSE),
		cc,
		request,
	)
	requireStatusError(t, err, ipcv1.StatusCode_STATUS_CODE_DEADLINE_EXCEEDED)
	if got := backend.calls("pause"); got != 0 {
		t.Fatalf("backend Pause calls = %d, expired command reached backend", got)
	}
}

func TestJointBatchValidationAndReceipt(t *testing.T) {
	host := newFakeHost()
	backend := newFakeBackend(degradedStatus(9), host)
	config := DefaultConfig()
	config.MaxJointBatch = 2
	_, err := New(host, backend, config, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	control := host.endpoint(contract.ControlInterface)
	method := uint32(simulationv1.ControlMethod_CONTROL_METHOD_SET_JOINT_POSITIONS)

	cases := []struct {
		name     string
		commands []*simulationv1.JointPositionCommand
		code     ipcv1.StatusCode
	}{
		{
			name: "axis one",
			commands: []*simulationv1.JointPositionCommand{{
				Model: "nervus_nao", Joint: "HeadYaw", Axis: 1, Position: 0.2,
			}},
			code: ipcv1.StatusCode_STATUS_CODE_FAILED_PRECONDITION,
		},
		{
			name: "non finite",
			commands: []*simulationv1.JointPositionCommand{{
				Model: "nervus_nao", Joint: "HeadYaw", Position: math.NaN(),
			}},
			code: ipcv1.StatusCode_STATUS_CODE_INVALID_ARGUMENT,
		},
		{
			name: "mixed models",
			commands: []*simulationv1.JointPositionCommand{
				{Model: "nervus_nao", Joint: "HeadYaw", Position: 0.2},
				{Model: "other", Joint: "HeadPitch", Position: 0.1},
			},
			code: ipcv1.StatusCode_STATUS_CODE_INVALID_ARGUMENT,
		},
		{
			name: "duplicate joint axis",
			commands: []*simulationv1.JointPositionCommand{
				{Model: "nervus_nao", Joint: "HeadYaw", Position: 0.2},
				{Model: "nervus_nao", Joint: "HeadYaw", Position: 0.1},
			},
			code: ipcv1.StatusCode_STATUS_CODE_INVALID_ARGUMENT,
		},
		{
			name: "batch too large",
			commands: []*simulationv1.JointPositionCommand{
				{Model: "nervus_nao", Joint: "HeadYaw", Position: 0.2},
				{Model: "nervus_nao", Joint: "HeadPitch", Position: 0.1},
				{Model: "nervus_nao", Joint: "LShoulderPitch", Position: 0.3},
			},
			code: ipcv1.StatusCode_STATUS_CODE_INVALID_ARGUMENT,
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			request, _ := proto.Marshal(&simulationv1.SetJointPositionsRequest{
				WorldGeneration: 9,
				Commands:        test.commands,
			})
			_, err := control.invoke(method, controlCallContext(1, 40), request)
			requireStatusError(t, err, test.code)
		})
	}
	if got := backend.calls("set_joints"); got != 0 {
		t.Fatalf("invalid batches reached backend %d times", got)
	}

	request, _ := proto.Marshal(&simulationv1.SetJointPositionsRequest{
		WorldGeneration: 9,
		Commands: []*simulationv1.JointPositionCommand{
			{Model: "nervus_nao", Joint: "HeadYaw", Position: 0.7},
			{Model: "nervus_nao", Joint: "HeadPitch", Position: -0.2},
		},
	})
	payload, err := control.invoke(method, controlCallContext(11, 40), request)
	if err != nil {
		t.Fatalf("SetJointPositions: %v", err)
	}
	var receipt simulationv1.JointCommandReceipt
	if err := proto.Unmarshal(payload, &receipt); err != nil {
		t.Fatalf("decode receipt: %v", err)
	}
	if receipt.GetSubmittedCommands() != 2 || receipt.GetCommandSequence() != 11 ||
		!receipt.GetFeedbackAvailable() {
		t.Fatalf("unexpected receipt: %+v", &receipt)
	}
	backend.mu.Lock()
	commands := append([]simulation.JointCommand(nil), backend.lastCommands...)
	backend.mu.Unlock()
	if len(commands) != 2 || commands[0].Joint != "HeadYaw" || commands[0].Position != 0.7 {
		t.Fatalf("unexpected backend commands: %+v", commands)
	}
}
