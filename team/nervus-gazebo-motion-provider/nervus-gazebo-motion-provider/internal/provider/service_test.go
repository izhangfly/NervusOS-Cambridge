package provider

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	simulationv1 "github.com/nervus-os/nervus-gazebo-provider/gen/go/gazebo/simulation/provider/v1"
	"github.com/nervus-os/nervus-gazebo-provider/internal/contract"
	"github.com/nervus-os/nervus-gazebo-provider/internal/simulation"
	"github.com/nervus-os/nervus-ipc/protocol/ipcv1"
	"github.com/nervus-os/nervus-ipc/sdk"
	"google.golang.org/protobuf/proto"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func readyStatus(generation uint64) simulation.Status {
	return simulation.Status{
		Mode:            simulation.ModeAttach,
		Process:         simulation.ProcessExternal,
		Transport:       simulation.TransportReady,
		Run:             simulation.RunRunning,
		WorldName:       "nervus_home",
		WorldGeneration: generation,
	}
}

func degradedStatus(generation uint64) simulation.Status {
	status := readyStatus(generation)
	status.Transport = simulation.TransportDegraded
	return status
}

func waitFor(t *testing.T, condition func() bool, description string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", description)
}

func startTestService(
	t *testing.T,
	status simulation.Status,
	config Config,
) (*Service, *fakeHost, *fakeBackend, context.CancelFunc, <-chan error) {
	t.Helper()
	host := newFakeHost()
	backend := newFakeBackend(status, host)
	service, err := New(host, backend, config, testLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- service.Run(ctx) }()
	return service, host, backend, cancel, done
}

func stopTestService(t *testing.T, cancel context.CancelFunc, done <-chan error) {
	t.Helper()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Service.Run: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Service.Run did not stop")
	}
}

func TestRunRegistersAllHandlersBeforeEndpointsAndShutsDownInOrder(t *testing.T) {
	_, host, backend, cancel, done := startTestService(t, readyStatus(1), DefaultConfig())
	manager := host.endpoint(contract.ManagerInterface)
	control := host.endpoint(contract.ControlInterface)
	waitFor(t, func() bool {
		managerRegistered, _, _, _ := manager.snapshot()
		controlRegistered, _, _, _ := control.snapshot()
		return managerRegistered && controlRegistered
	}, "manager and control registration")

	stopTestService(t, cancel, done)

	_, managerRegisters, managerUnregisters, _ := manager.snapshot()
	_, controlRegisters, controlUnregisters, _ := control.snapshot()
	if managerRegisters != 1 || managerUnregisters != 1 {
		t.Fatalf("manager register/unregister = %d/%d, want 1/1", managerRegisters, managerUnregisters)
	}
	if controlRegisters != 1 || controlUnregisters != 1 {
		t.Fatalf("control register/unregister = %d/%d, want 1/1", controlRegisters, controlUnregisters)
	}
	backend.mu.Lock()
	closed := backend.closed
	backend.mu.Unlock()
	if !closed {
		t.Fatal("backend was not closed")
	}
	wantOrder := []string{
		contract.ManagerInterface + ".register",
		contract.ControlInterface + ".register",
		contract.ControlInterface + ".unregister",
		"backend.close",
		contract.ManagerInterface + ".unregister",
		"host.close",
	}
	if got := host.orderSnapshot(); !reflect.DeepEqual(got, wantOrder) {
		t.Fatalf("lifecycle order = %v, want %v", got, wantOrder)
	}
}

func TestNewUsesContractHashesAndWorldResourceBinding(t *testing.T) {
	host := newFakeHost()
	backend := newFakeBackend(degradedStatus(1), host)
	if _, err := New(host, backend, DefaultConfig(), testLogger()); err != nil {
		t.Fatal(err)
	}
	managerHash, controlHash, err := contract.SchemaHashes()
	if err != nil {
		t.Fatal(err)
	}
	manager := host.endpoint(contract.ManagerInterface)
	control := host.endpoint(contract.ControlInterface)
	if manager.req.Major != contract.InterfaceMajor ||
		!bytes.Equal(manager.req.SchemaHash, managerHash) || manager.req.ResourceHandle != "" {
		t.Fatalf("unexpected manager registration: %+v", manager.req)
	}
	if control.req.Major != contract.InterfaceMajor ||
		!bytes.Equal(control.req.SchemaHash, controlHash) ||
		control.req.ResourceHandle != contract.WorldResourceRole {
		t.Fatalf("unexpected control registration: %+v", control.req)
	}
}

func TestServiceFailsClosedOnPrematureStandardMotionBinding(t *testing.T) {
	host := newFakeHost()
	backend := newFakeBackend(readyStatus(1), host)
	backend.bindings = []simulation.MotionBinding{{
		ProfileID: "gazebo.test.arm.v1", Model: "robot",
		CapabilityRole: "arm", ResourceRole: contract.LeftArmResourceRole,
		InterfaceID: contract.ManipulatorInterface, Adapter: "joint_position",
		State: simulation.MotionBindingReady, WorldGeneration: 1,
	}}
	service, err := New(host, backend, DefaultConfig(), testLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	err = service.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "without an endpoint implementation") {
		t.Fatalf("Run error = %v", err)
	}
	if endpoint := host.endpoint(contract.BaseInterface); endpoint == nil {
		t.Fatal("BaseMotion endpoint was not created for handler installation")
	} else if registered, _, _, _ := endpoint.snapshot(); registered {
		t.Fatal("BaseMotion endpoint registered while manipulator binding failed closed")
	}
}

func TestStatusEventsReconcileControlEndpoint(t *testing.T) {
	_, host, backend, cancel, done := startTestService(t, readyStatus(1), DefaultConfig())
	control := host.endpoint(contract.ControlInterface)
	waitFor(t, func() bool {
		registered, _, _, _ := control.snapshot()
		return registered
	}, "initial control registration")

	backend.events <- simulation.Event{
		Kind:           simulation.EventStatusChanged,
		MonotonicNanos: 101,
		Status:         degradedStatus(1),
	}
	waitFor(t, func() bool {
		registered, _, unregisters, _ := control.snapshot()
		return !registered && unregisters == 1
	}, "control removal after degradation")

	backend.events <- simulation.Event{
		Kind:           simulation.EventStatusChanged,
		MonotonicNanos: 102,
		Status:         readyStatus(2),
	}
	waitFor(t, func() bool {
		registered, registers, _, _ := control.snapshot()
		return registered && registers == 2
	}, "control re-registration after recovery")

	stopTestService(t, cancel, done)
	_, registers, unregisters, _ := control.snapshot()
	if registers != 2 || unregisters != 2 {
		t.Fatalf("control register/unregister = %d/%d, want 2/2", registers, unregisters)
	}
}

func TestServiceRegistersBaseMotionOnlyForReadyBinding(t *testing.T) {
	host := newFakeHost()
	backend := newFakeBackend(readyStatus(1), host)
	backend.bindings = []simulation.MotionBinding{{
		ProfileID: "gazebo.test.base.v1", Model: "robot", CapabilityRole: "base",
		ResourceRole: contract.BaseResourceRole, InterfaceID: contract.BaseInterface,
		Adapter: "diff_drive", State: simulation.MotionBindingReady, WorldGeneration: 1,
	}}
	service, err := New(host, backend, DefaultConfig(), testLogger())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- service.Run(ctx) }()
	base := host.endpoint(contract.BaseInterface)
	waitFor(t, func() bool { registered, _, _, _ := base.snapshot(); return registered }, "BaseMotion registration")
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	_, registers, unregisters, _ := base.snapshot()
	if registers != 1 || unregisters != 1 {
		t.Fatalf("BaseMotion register/unregister = %d/%d, want 1/1", registers, unregisters)
	}
}

func TestTypedBackendErrorDoesNotLeakBackendOperation(t *testing.T) {
	host := newFakeHost()
	backend := newFakeBackend(degradedStatus(3), host)
	backend.modelsErr = simulation.NewError(
		simulation.ReasonModelNotFound,
		"native /private/path should not cross the boundary",
		false,
	)
	service, err := New(host, backend, DefaultConfig(), testLogger())
	if err != nil {
		t.Fatal(err)
	}
	manager := host.endpoint(contract.ManagerInterface)
	request, _ := proto.Marshal(&simulationv1.ListModelsRequest{WorldGeneration: 3})
	_, err = manager.invoke(
		uint32(simulationv1.ManagerMethod_MANAGER_METHOD_LIST_MODELS),
		queryCallContext(),
		request,
	)
	status := requireStatusError(t, err, ipcv1.StatusCode_STATUS_CODE_NOT_FOUND)
	var detail simulationv1.SimulationErrorDetail
	if err := proto.Unmarshal(status.Detail, &detail); err != nil {
		t.Fatalf("decode detail: %v", err)
	}
	if detail.GetReason() != simulationv1.SimulationErrorReason_SIMULATION_ERROR_REASON_MODEL_NOT_FOUND {
		t.Fatalf("reason = %v", detail.GetReason())
	}
	if detail.GetOperation() != "manager.list_models" {
		t.Fatalf("operation = %q, backend detail leaked or mapping drifted", detail.GetOperation())
	}
	if detail.GetWorldGeneration() != 3 {
		t.Fatalf("world generation = %d, want 3", detail.GetWorldGeneration())
	}
	_ = service
}

func TestBackendEventsUseTypedPayloadsAndSourceTimestamp(t *testing.T) {
	_, host, backend, cancel, done := startTestService(t, readyStatus(8), DefaultConfig())
	manager := host.endpoint(contract.ManagerInterface)
	control := host.endpoint(contract.ControlInterface)
	waitFor(t, func() bool {
		registered, _, _, _ := control.snapshot()
		return registered
	}, "control registration")

	backend.events <- simulation.Event{
		Kind:           simulation.EventTransportFault,
		MonotonicNanos: 555,
		TransportFault: simulation.TransportFaultEvent{
			Kind:            simulation.TransportFaultTypeMismatch,
			Reason:          simulation.ReasonTypeMismatch,
			WorldGeneration: 8,
		},
	}
	backend.events <- simulation.Event{
		Kind:           simulation.EventCommandRejected,
		MonotonicNanos: 556,
		CommandRejected: simulation.CommandRejectedEvent{
			WorldGeneration: 8,
			CommandSequence: 17,
			Reason:          simulation.ReasonRemoteRejected,
			Model:           "nervus_nao",
			Joint:           "HeadYaw",
		},
	}
	backend.events <- simulation.Event{
		Kind: simulation.EventProcessExited,
		ProcessExit: simulation.ProcessExitEvent{
			ProcessID: 901, ExitCode: 17, Expected: false, WorldGeneration: 8,
		},
	}
	backend.events <- simulation.Event{
		Kind: simulation.EventWSLgFault,
		WSLgFault: simulation.WSLgFaultEvent{
			Kind: simulation.WSLgFaultKeepaliveMissing, Retryable: true, WorldGeneration: 8,
		},
	}
	backend.events <- simulation.Event{
		Kind: simulation.EventWorldChanged,
		WorldChanged: simulation.WorldChangedEvent{
			PreviousWorld: "nervus_home", PreviousGeneration: 8,
			CurrentWorld: "generic_lab", CurrentGeneration: 9,
		},
	}

	waitFor(t, func() bool {
		_, _, _, managerEvents := manager.snapshot()
		_, _, _, controlEvents := control.snapshot()
		return hasEvent(managerEvents,
			uint32(simulationv1.ManagerEvent_MANAGER_EVENT_TRANSPORT_FAULT)) &&
			hasEvent(managerEvents,
				uint32(simulationv1.ManagerEvent_MANAGER_EVENT_PROCESS_EXITED)) &&
			hasEvent(managerEvents,
				uint32(simulationv1.ManagerEvent_MANAGER_EVENT_WSLG_FAULT)) &&
			hasEvent(managerEvents,
				uint32(simulationv1.ManagerEvent_MANAGER_EVENT_WORLD_CHANGED)) &&
			hasEvent(controlEvents,
				uint32(simulationv1.ControlEvent_CONTROL_EVENT_COMMAND_REJECTED))
	}, "typed manager and control events")

	_, _, _, managerEvents := manager.snapshot()
	faultEvent := findEvent(managerEvents,
		uint32(simulationv1.ManagerEvent_MANAGER_EVENT_TRANSPORT_FAULT))
	if faultEvent.timestamp != 555 {
		t.Fatalf("transport fault timestamp = %d, want 555", faultEvent.timestamp)
	}
	var fault simulationv1.TransportFault
	if err := proto.Unmarshal(faultEvent.payload, &fault); err != nil {
		t.Fatalf("decode TransportFault: %v", err)
	}
	if fault.GetKind() != simulationv1.TransportFaultKind_TRANSPORT_FAULT_KIND_TYPE_MISMATCH {
		t.Fatalf("transport fault kind = %v", fault.GetKind())
	}
	processEvent := findEvent(managerEvents,
		uint32(simulationv1.ManagerEvent_MANAGER_EVENT_PROCESS_EXITED))
	var process simulationv1.ProcessExited
	if err := proto.Unmarshal(processEvent.payload, &process); err != nil {
		t.Fatalf("decode ProcessExited: %v", err)
	}
	if process.GetProcessId() != 901 || process.GetExitCode() != 17 || process.GetExpected() {
		t.Fatalf("unexpected ProcessExited: %+v", &process)
	}
	wslgEvent := findEvent(managerEvents,
		uint32(simulationv1.ManagerEvent_MANAGER_EVENT_WSLG_FAULT))
	var wslgFault simulationv1.WSLgFault
	if err := proto.Unmarshal(wslgEvent.payload, &wslgFault); err != nil {
		t.Fatalf("decode WSLgFault: %v", err)
	}
	if wslgFault.GetKind() != simulationv1.WSLgFaultKind_WSLG_FAULT_KIND_KEEPALIVE_MISSING ||
		!wslgFault.GetRetryable() {
		t.Fatalf("unexpected WSLgFault: %+v", &wslgFault)
	}
	worldEvent := findEvent(managerEvents,
		uint32(simulationv1.ManagerEvent_MANAGER_EVENT_WORLD_CHANGED))
	var world simulationv1.WorldChanged
	if err := proto.Unmarshal(worldEvent.payload, &world); err != nil {
		t.Fatalf("decode WorldChanged: %v", err)
	}
	if world.GetPreviousGeneration() != 8 || world.GetCurrentGeneration() != 9 ||
		world.GetCurrentWorld() != "generic_lab" {
		t.Fatalf("unexpected WorldChanged: %+v", &world)
	}

	_, _, _, controlEvents := control.snapshot()
	rejectedEvent := findEvent(controlEvents,
		uint32(simulationv1.ControlEvent_CONTROL_EVENT_COMMAND_REJECTED))
	var rejected simulationv1.CommandRejected
	if err := proto.Unmarshal(rejectedEvent.payload, &rejected); err != nil {
		t.Fatalf("decode CommandRejected: %v", err)
	}
	if rejected.GetCommandSequence() != 17 || rejected.GetJoint() != "HeadYaw" {
		t.Fatalf("unexpected CommandRejected: %+v", &rejected)
	}

	stopTestService(t, cancel, done)
}

func hasEvent(events []fakePublishedEvent, id uint32) bool {
	for _, event := range events {
		if event.id == id {
			return true
		}
	}
	return false
}

func findEvent(events []fakePublishedEvent, id uint32) fakePublishedEvent {
	for _, event := range events {
		if event.id == id {
			return event
		}
	}
	return fakePublishedEvent{}
}

func queryCallContext() sdk.CallContext {
	return callContext(contract.QueryPermission)
}

func callContext(permissions ...string) sdk.CallContext {
	return sdk.CallContext{
		Ctx: context.Background(),
		Caller: &ipcv1.CallerContext{
			PackageId:          "test.caller",
			ComponentId:        "main",
			GrantedPermissions: append([]string(nil), permissions...),
		},
	}
}

func controlCallContext(sequence, resourceGeneration uint64) sdk.CallContext {
	cc := callContext(contract.ControlPermission)
	cc.Execution = &ipcv1.ExecutionContext{
		LeaseId:            10,
		ControllerClass:    ipcv1.ControllerClass_CONTROLLER_CLASS_HUMAN,
		MotionEpoch:        20,
		DeadlineNanos:      math.MaxInt64,
		CommandSequence:    sequence,
		ResourceHandle:     contract.WorldResourceRole,
		ResourceGeneration: resourceGeneration,
	}
	return cc
}

func requireStatusError(
	t *testing.T,
	err error,
	want ipcv1.StatusCode,
) *sdk.StatusError {
	t.Helper()
	if err == nil {
		t.Fatalf("error = nil, want %v", want)
	}
	var status *sdk.StatusError
	if !errors.As(err, &status) {
		t.Fatalf("error type = %T, want *sdk.StatusError: %v", err, err)
	}
	if status.Code != want {
		t.Fatalf("status code = %v, want %v", status.Code, want)
	}
	return status
}
