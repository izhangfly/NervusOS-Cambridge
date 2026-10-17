package jetty

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"math"
	"testing"
	"time"

	"github.com/nervus-os/nervus-gazebo-provider/internal/gzmsgs"
	"github.com/nervus-os/nervus-gazebo-provider/internal/gztransport"
	"github.com/nervus-os/nervus-gazebo-provider/internal/motionprofile"
	"github.com/nervus-os/nervus-gazebo-provider/internal/simulation"
	"google.golang.org/protobuf/proto"
)

func newTestBackend(t *testing.T, feedback bool) (*Backend, *fakeClient) {
	t.Helper()
	config, client := testConfig(feedback)
	backend, err := New(
		config,
		client,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		if err := backend.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	waitReady(t, backend)
	return backend, client
}

func waitReady(t *testing.T, backend *Backend) simulation.Status {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		status, err := backend.Status(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if status.ReadyForControl() {
			return status
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("backend did not become ready")
	return simulation.Status{}
}

func TestBackendDiscoversSceneAndRunsWorldAndJointClosedLoop(t *testing.T) {
	backend, client := newTestBackend(t, false)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	status, err := backend.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if status.WorldName != "nervus_home" || status.WorldGeneration != 1 ||
		status.Transport != simulation.TransportReady || status.Process != simulation.ProcessExternal {
		t.Fatalf("unexpected ready status: %+v", status)
	}
	capabilities, err := backend.Capabilities(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !capabilities.CanControlWorld || !capabilities.CanQueryScene ||
		!capabilities.CanCommandJoints || capabilities.JointStateFeedback {
		t.Fatalf("unexpected capabilities: %+v", capabilities)
	}

	models, err := backend.ListModels(ctx, 1)
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(models.Models) != 1 || models.Models[0].Name != "nervus_nao" ||
		models.Models[0].CanonicalLink != "Torso" {
		t.Fatalf("unexpected models: %+v", models.Models)
	}
	joints, err := backend.ListJoints(ctx, "nervus_nao", 1)
	if err != nil {
		t.Fatalf("ListJoints: %v", err)
	}
	if len(joints.Joints) != 1 || joints.Joints[0].Name != "HeadYaw" ||
		!joints.Joints[0].Controllable || len(joints.Joints[0].Axes) != 1 {
		t.Fatalf("unexpected joints: %+v", joints.Joints)
	}

	execution := simulation.ControlContext{WorldGeneration: 1, CommandSequence: 7}
	paused, err := backend.Pause(ctx, execution)
	if err != nil {
		t.Fatalf("Pause: %v", err)
	}
	if paused.Run != simulation.RunPaused {
		t.Fatalf("Pause status = %+v", paused)
	}
	stepped, err := backend.Step(ctx, execution, 5)
	if err != nil {
		t.Fatalf("Step: %v", err)
	}
	if stepped.Run != simulation.RunPaused || stepped.Statistics.Iterations != 5 {
		t.Fatalf("Step status = %+v", stepped)
	}
	client.mu.Lock()
	stepRequest := client.requests[len(client.requests)-1]
	client.mu.Unlock()
	if !stepRequest.GetPause() || stepRequest.GetMultiStep() != 5 {
		t.Fatalf("Step request = %+v, want pause=true multi_step=5", stepRequest)
	}
	resumed, err := backend.Resume(ctx, execution)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if resumed.Run != simulation.RunRunning {
		t.Fatalf("Resume status = %+v", resumed)
	}

	receipt, err := backend.SetJointPositions(ctx, execution, []simulation.JointCommand{{
		Model: "nervus_nao", Joint: "HeadYaw", Axis: 0, Position: 0.7,
	}})
	if err != nil {
		t.Fatalf("SetJointPositions: %v", err)
	}
	if receipt.SubmittedCommands != 1 || receipt.FeedbackAvailable {
		t.Fatalf("unexpected receipt: %+v", receipt)
	}
	topic, position, err := client.lastPublishedDouble()
	if err != nil {
		t.Fatal(err)
	}
	if topic != "/model/nervus_nao/joint/HeadYaw/0/cmd_pos" || position != 0.7 {
		t.Fatalf("published %q = %v", topic, position)
	}
	_, err = backend.JointState(ctx, simulation.JointQuery{
		WorldGeneration: 1, Model: "nervus_nao", Joint: "HeadYaw", Axis: 0,
	})
	requireSimulationReason(t, err, simulation.ReasonFeedbackUnavailable)
}

func TestWorldReadinessDoesNotRequireAnyRobotProfile(t *testing.T) {
	config, client := testConfig(false)
	config.Profiles = motionprofile.Empty()
	backend, err := New(config, client, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = backend.Close() })
	status := waitReady(t, backend)
	capabilities, err := backend.Capabilities(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !capabilities.CanQueryScene || !capabilities.CanControlWorld || capabilities.CanCommandJoints {
		t.Fatalf("profile-free capabilities = %+v", capabilities)
	}
	models, err := backend.ListModels(context.Background(), status.WorldGeneration)
	if err != nil || len(models.Models) != 1 {
		t.Fatalf("profile-free ListModels = %+v, %v", models, err)
	}
	bindings, err := backend.MotionBindings(context.Background())
	if err != nil || len(bindings) != 0 {
		t.Fatalf("profile-free bindings = %+v, %v", bindings, err)
	}
	_, err = backend.SetJointPositions(context.Background(), simulation.ControlContext{
		WorldGeneration: status.WorldGeneration,
	}, []simulation.JointCommand{{Model: "nervus_nao", Joint: "HeadYaw", Axis: 0}})
	requireSimulationReason(t, err, simulation.ReasonJointNotControllable)
}

func TestProfileTypeMismatchIsUnsupportedWithoutDisablingWorld(t *testing.T) {
	config, client := testConfig(false)
	client.mu.Lock()
	client.topics[jointCommandTopic(client.model, "HeadYaw", 0)] = gztransport.TopicInfo{
		SubscriberCount: 1,
		MessageType:     "gz.msgs.StringMsg",
	}
	client.mu.Unlock()
	backend, err := New(config, client, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = backend.Close() })
	status := waitReady(t, backend)
	if !status.ReadyForControl() {
		t.Fatalf("world was disabled by a bad motion Profile: %+v", status)
	}
	bindings, err := backend.MotionBindings(context.Background())
	if err != nil || len(bindings) != 1 ||
		bindings[0].State != simulation.MotionBindingUnsupported ||
		bindings[0].Reason != "command_message_type_mismatch" {
		t.Fatalf("type-mismatch bindings = %+v, %v", bindings, err)
	}
	capabilities, _ := backend.Capabilities(context.Background())
	if capabilities.CanCommandJoints || !capabilities.CanQueryScene {
		t.Fatalf("type-mismatch capabilities = %+v", capabilities)
	}
}

func TestGenericSceneQueriesMultipleModels(t *testing.T) {
	config, client := testConfig(false)
	config.Profiles = motionprofile.Empty()
	client.sceneWire, _ = proto.Marshal(&gzmsgs.Scene{
		Name: client.world,
		Model: []*gzmsgs.Model{
			{Name: "robot_a", Id: 1, Joint: []*gzmsgs.Joint{{Name: "JointA", Axis1: &gzmsgs.Axis{}}}},
			{Name: "robot_b", Id: 2, Joint: []*gzmsgs.Joint{{Name: "JointB", Axis1: &gzmsgs.Axis{}}}},
		},
	})
	backend, err := New(config, client, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = backend.Close() })
	status := waitReady(t, backend)
	models, err := backend.ListModels(context.Background(), status.WorldGeneration)
	if err != nil || len(models.Models) != 2 {
		t.Fatalf("ListModels = %+v, %v", models, err)
	}
	joints, err := backend.ListJoints(context.Background(), "robot_b", status.WorldGeneration)
	if err != nil || len(joints.Joints) != 1 || joints.Joints[0].Name != "JointB" ||
		joints.Joints[0].Controllable {
		t.Fatalf("ListJoints robot_b = %+v, %v", joints, err)
	}
}

func TestBackendUsesJointStateFeedbackWhenPublisherExists(t *testing.T) {
	backend, _ := newTestBackend(t, true)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	execution := simulation.ControlContext{WorldGeneration: 1, CommandSequence: 11}
	receipt, err := backend.SetJointPositions(ctx, execution, []simulation.JointCommand{{
		Model: "nervus_nao", Joint: "HeadYaw", Axis: 0, Position: 0.7,
	}})
	if err != nil {
		t.Fatalf("SetJointPositions: %v", err)
	}
	if !receipt.FeedbackAvailable {
		t.Fatal("joint feedback publisher was not reported")
	}
	state, err := backend.JointState(ctx, simulation.JointQuery{
		WorldGeneration: 1, Model: "nervus_nao", Joint: "HeadYaw", Axis: 0,
	})
	if err != nil {
		t.Fatalf("JointState: %v", err)
	}
	if !state.HasPosition || math.Abs(state.Position-0.7) > 1e-9 || !state.HasVelocity {
		t.Fatalf("unexpected feedback: %+v", state)
	}

	select {
	case event := <-backend.Events():
		for event.Kind == simulation.EventStatusChanged || event.Kind == simulation.EventWorldChanged {
			select {
			case event = <-backend.Events():
			case <-ctx.Done():
				t.Fatal("joint feedback event was not emitted")
			}
		}
		if event.Kind != simulation.EventJointStateChanged || event.JointState.Joint != "HeadYaw" {
			t.Fatalf("unexpected event: %+v", event)
		}
	case <-ctx.Done():
		t.Fatal("joint feedback event was not emitted")
	}
}

func TestBackendRejectsRemoteWorldControlFailureAndStaleGeneration(t *testing.T) {
	backend, client := newTestBackend(t, false)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	_, err := backend.Pause(ctx, simulation.ControlContext{WorldGeneration: 2})
	requireSimulationReason(t, err, simulation.ReasonWorldChanged)

	client.mu.Lock()
	client.accepted = false
	client.mu.Unlock()
	_, err = backend.Pause(ctx, simulation.ControlContext{WorldGeneration: 1})
	requireSimulationReason(t, err, simulation.ReasonRemoteRejected)
}

func TestBackendMapsAllResetModesToWorldControl(t *testing.T) {
	backend, client := newTestBackend(t, false)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	execution := simulation.ControlContext{WorldGeneration: 1}
	tests := []struct {
		name string
		mode simulation.ResetMode
		want func(*gzmsgs.WorldReset) bool
	}{
		{name: "all", mode: simulation.ResetAll, want: func(reset *gzmsgs.WorldReset) bool {
			return reset.GetAll() && !reset.GetTimeOnly() && !reset.GetModelOnly()
		}},
		{name: "time", mode: simulation.ResetTimeOnly, want: func(reset *gzmsgs.WorldReset) bool {
			return !reset.GetAll() && reset.GetTimeOnly() && !reset.GetModelOnly()
		}},
		{name: "models", mode: simulation.ResetModelsOnly, want: func(reset *gzmsgs.WorldReset) bool {
			return !reset.GetAll() && !reset.GetTimeOnly() && reset.GetModelOnly()
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := backend.ResetWorld(ctx, execution, test.mode); err != nil {
				t.Fatalf("ResetWorld: %v", err)
			}
			client.mu.Lock()
			request := client.requests[len(client.requests)-1]
			client.mu.Unlock()
			if request.GetReset_() == nil || !test.want(request.GetReset_()) {
				t.Fatalf("unexpected reset request: %+v", request.GetReset_())
			}
		})
	}
}

func TestBackendValidatesNativeJointLimitsBeforePublish(t *testing.T) {
	backend, client := newTestBackend(t, false)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err := backend.SetJointPositions(ctx, simulation.ControlContext{WorldGeneration: 1},
		[]simulation.JointCommand{{
			Model: "nervus_nao", Joint: "HeadYaw", Axis: 0, Position: 3,
		}})
	requireSimulationReason(t, err, simulation.ReasonOutOfRange)
	client.mu.Lock()
	published := len(client.published)
	client.mu.Unlock()
	if published != 0 {
		t.Fatalf("out-of-range command published %d messages", published)
	}
}

func TestBackendDiscoversSubscriberOnlyControllersMissingFromScene(t *testing.T) {
	config, client := testConfig(false)
	client.commandTopicInfoUnavailable = true
	client.sceneWire, _ = proto.Marshal(&gzmsgs.Scene{
		Name: client.world,
		Model: []*gzmsgs.Model{{
			Name: client.model,
			Id:   42,
			Link: []*gzmsgs.Link{{Name: "Torso", Id: 43, Canonical: true}},
		}},
	})
	backend, err := New(config, client, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		if err := backend.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	status := waitReady(t, backend)
	joints, err := backend.ListJoints(context.Background(), client.model, status.WorldGeneration)
	if err != nil {
		t.Fatalf("ListJoints: %v", err)
	}
	if len(joints.Joints) != 1 || joints.Joints[0].Name != "HeadYaw" ||
		joints.Joints[0].Type != "UNKNOWN" || !joints.Joints[0].Controllable {
		t.Fatalf("subscriber-only controller not discovered: %+v", joints.Joints)
	}
	if len(joints.Joints[0].Axes) != 1 || joints.Joints[0].Axes[0].HasPositionLimits {
		t.Fatalf("synthetic joint invented native limits: %+v", joints.Joints[0])
	}
}

func TestBackendRecoversTransportAndAdvancesWorldGeneration(t *testing.T) {
	backend, client := newTestBackend(t, false)
	initial, err := backend.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	initialBindings, err := backend.MotionBindings(context.Background())
	if err != nil || len(initialBindings) != 1 ||
		initialBindings[0].State != simulation.MotionBindingReady ||
		initialBindings[0].WorldGeneration != initial.WorldGeneration {
		t.Fatalf("initial bindings = %+v, %v", initialBindings, err)
	}
	client.disconnect()
	waitStatus(t, backend, func(status simulation.Status) bool {
		return !status.ReadyForControl() &&
			(status.Transport == simulation.TransportDegraded ||
				status.Transport == simulation.TransportDisconnected)
	})
	_, err = backend.Pause(context.Background(), simulation.ControlContext{
		WorldGeneration: initial.WorldGeneration,
	})
	requireSimulationReason(t, err, simulation.ReasonWorldNotReady)

	client.reconnect()
	recovered := waitStatus(t, backend, func(status simulation.Status) bool {
		return status.ReadyForControl() && status.WorldGeneration == initial.WorldGeneration+1
	})
	recoveredBindings, err := backend.MotionBindings(context.Background())
	if err != nil || len(recoveredBindings) != 1 ||
		recoveredBindings[0].WorldGeneration != recovered.WorldGeneration {
		t.Fatalf("recovered bindings = %+v, %v", recoveredBindings, err)
	}
	_, err = backend.Pause(context.Background(), simulation.ControlContext{
		WorldGeneration: initial.WorldGeneration,
	})
	requireSimulationReason(t, err, simulation.ReasonWorldChanged)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := backend.Pause(ctx, simulation.ControlContext{
		WorldGeneration: recovered.WorldGeneration,
	}); err != nil {
		t.Fatalf("Pause after recovery: %v", err)
	}

	wantFault, wantGeneration := false, false
	deadline := time.After(time.Second)
	for !wantFault || !wantGeneration {
		select {
		case event := <-backend.Events():
			if event.Kind == simulation.EventTransportFault {
				wantFault = true
			}
			if event.Kind == simulation.EventWorldChanged &&
				event.WorldChanged.CurrentGeneration == recovered.WorldGeneration {
				wantGeneration = true
			}
		case <-deadline:
			t.Fatalf("events missing: transport_fault=%v generation=%v", wantFault, wantGeneration)
		}
	}
}

func TestManagedBackendStartsReloadsAndStopsOwnedProcess(t *testing.T) {
	config, client := testConfig(false)
	config.Mode = simulation.ModeManaged
	process := newFakeManagedProcess("nervus_home")
	backend, err := NewWithLifecycle(
		config, client, process, nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	if err != nil {
		t.Fatalf("NewWithLifecycle: %v", err)
	}
	t.Cleanup(func() {
		if err := backend.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	initial, err := backend.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if initial.Process != simulation.ProcessStopped || initial.ReadyForControl() {
		t.Fatalf("initial managed status = %+v", initial)
	}
	capabilities, err := backend.Capabilities(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !capabilities.CanManageProcess || !capabilities.CanLoadWorld {
		t.Fatalf("managed capabilities = %+v", capabilities)
	}

	if _, err := backend.Start(context.Background(), simulation.StartOptions{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	first := waitStatus(t, backend, func(status simulation.Status) bool {
		return status.ReadyForControl() && status.WorldGeneration == 1 &&
			status.ProcessOwned && status.ManagedProcessID == 4242
	})
	if _, err := backend.LoadWorld(context.Background(), "nervus_home"); err != nil {
		t.Fatalf("LoadWorld: %v", err)
	}
	second := waitStatus(t, backend, func(status simulation.Status) bool {
		return status.ReadyForControl() && status.WorldGeneration == first.WorldGeneration+1
	})
	if second.WorldName != "nervus_home" {
		t.Fatalf("reloaded status = %+v", second)
	}
	stopped, err := backend.Stop(context.Background())
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if stopped.Process != simulation.ProcessStopped || stopped.ProcessOwned ||
		stopped.Transport != simulation.TransportDisconnected {
		t.Fatalf("stopped status = %+v", stopped)
	}
	process.mu.Lock()
	starts, stops := process.starts, process.stops
	process.mu.Unlock()
	if starts != 2 || stops != 2 {
		t.Fatalf("process starts/stops = %d/%d, want 2/2", starts, stops)
	}
}

func TestManagedBackendRefusesStartWhenWSLgIsNotReady(t *testing.T) {
	config, client := testConfig(false)
	config.Mode = simulation.ModeManaged
	process := newFakeManagedProcess("nervus_home")
	probe := fakeWSLgProbe{faults: []simulation.WSLgFaultEvent{{
		Kind: simulation.WSLgFaultKeepaliveMissing, Retryable: true,
	}}}
	backend, err := NewWithLifecycle(
		config, client, process, probe,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	if err != nil {
		t.Fatalf("NewWithLifecycle: %v", err)
	}
	t.Cleanup(func() { _ = backend.Close() })
	_, err = backend.Start(context.Background(), simulation.StartOptions{})
	requireSimulationReason(t, err, simulation.ReasonWSLgNotReady)
	process.mu.Lock()
	starts := process.starts
	process.mu.Unlock()
	if starts != 0 {
		t.Fatalf("process started %d times despite WSLg fault", starts)
	}
}

func waitStatus(
	t *testing.T,
	backend *Backend,
	predicate func(simulation.Status) bool,
) simulation.Status {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		status, err := backend.Status(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if predicate(status) {
			return status
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("backend status did not reach expected state")
	return simulation.Status{}
}

func requireSimulationReason(t *testing.T, err error, want simulation.Reason) {
	t.Helper()
	if err == nil {
		t.Fatalf("error = nil, want reason %v", want)
	}
	var domain *simulation.Error
	if !errors.As(err, &domain) {
		t.Fatalf("error type = %T, want *simulation.Error: %v", err, err)
	}
	if domain.Reason != want {
		t.Fatalf("reason = %v, want %v", domain.Reason, want)
	}
}
