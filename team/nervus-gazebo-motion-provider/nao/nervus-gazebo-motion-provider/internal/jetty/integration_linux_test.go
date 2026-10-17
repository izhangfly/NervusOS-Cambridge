//go:build linux && cgo && jettyintegration

package jetty

import (
	"context"
	"io"
	"log/slog"
	"math"
	"testing"
	"time"

	"github.com/nervus-os/nervus-gazebo-provider/internal/contract"
	"github.com/nervus-os/nervus-gazebo-provider/internal/gztransport"
	"github.com/nervus-os/nervus-gazebo-provider/internal/motionprofile"
	"github.com/nervus-os/nervus-gazebo-provider/internal/simulation"
)

func TestRealJettyNAOMinimumClosedLoop(t *testing.T) {
	transportConfig := gztransport.DefaultConfig()
	client, err := gztransport.Open(transportConfig)
	if err != nil {
		t.Fatalf("open Gazebo Transport: %v", err)
	}
	config := DefaultConfig()
	profiles, err := motionprofile.LoadBundle(
		"../../profiles", "nao-fixed-v1.json", contract.MotionProfileAllowlist(),
	)
	if err != nil {
		t.Fatalf("load fixed NAO Profile: %v", err)
	}
	runtimeConfig, err := motionprofile.LoadRuntime(
		"../../profiles", "runtime-nao-fixed-v1.json", profiles,
	)
	if err != nil {
		t.Fatalf("load fixed NAO runtime selection: %v", err)
	}
	config.Profiles = profiles
	config.Runtime = runtimeConfig
	config.DiscoveryTimeout = 30 * time.Second
	backend, err := New(
		config,
		client,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	if err != nil {
		_ = client.Close()
		t.Fatalf("create backend: %v", err)
	}
	defer func() {
		if err := backend.Close(); err != nil {
			t.Errorf("close backend: %v", err)
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	status := waitRealReady(t, ctx, backend)
	initialRun := status.Run
	execution := simulation.ControlContext{WorldGeneration: status.WorldGeneration}

	models, err := backend.ListModels(ctx, status.WorldGeneration)
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if !containsModel(models.Models, "nervus_nao") {
		t.Fatalf("nervus_nao missing from %d models", len(models.Models))
	}
	joints, err := backend.ListJoints(ctx, "nervus_nao", status.WorldGeneration)
	if err != nil {
		t.Fatalf("ListJoints: %v", err)
	}
	controllable := 0
	for _, joint := range joints.Joints {
		if joint.Controllable {
			controllable++
		}
	}
	if controllable != 24 {
		t.Fatalf("controllable joints = %d, want 24", controllable)
	}

	defer func() {
		restoreCtx, restoreCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer restoreCancel()
		_, _ = backend.SetJointPositions(restoreCtx, execution, []simulation.JointCommand{{
			Model: "nervus_nao", Joint: "HeadYaw", Axis: 0, Position: 0,
		}})
		if initialRun == simulation.RunRunning {
			_, _ = backend.Resume(restoreCtx, execution)
		}
	}()

	paused, err := backend.Pause(ctx, execution)
	if err != nil {
		t.Fatalf("Pause: %v", err)
	}
	before := paused.Statistics.Iterations
	stepped, err := backend.Step(ctx, execution, 3)
	if err != nil {
		t.Fatalf("Step: %v", err)
	}
	if stepped.Run != simulation.RunPaused || stepped.Statistics.Iterations < before+3 {
		t.Fatalf("step did not advance three iterations: before=%d after=%d run=%v",
			before, stepped.Statistics.Iterations, stepped.Run)
	}

	execution.CommandSequence = 1
	receipt, err := backend.SetJointPositions(ctx, execution, []simulation.JointCommand{{
		Model: "nervus_nao", Joint: "HeadYaw", Axis: 0, Position: 0.7,
	}})
	if err != nil {
		t.Fatalf("SetJointPositions HeadYaw: %v", err)
	}
	if receipt.SubmittedCommands != 1 {
		t.Fatalf("submitted commands = %d, want 1", receipt.SubmittedCommands)
	}
	if receipt.FeedbackAvailable {
		state, err := backend.JointState(ctx, simulation.JointQuery{
			WorldGeneration: status.WorldGeneration,
			Model:           "nervus_nao",
			Joint:           "HeadYaw",
			Axis:            0,
		})
		if err != nil {
			t.Fatalf("HeadYaw feedback: %v", err)
		}
		if !state.HasPosition || math.Abs(state.Position-0.7) > 0.02 {
			t.Fatalf("HeadYaw feedback = %+v, want 0.7 +/- 0.02", state)
		}
	} else {
		t.Log("JointStatePublisher is absent; publish receipt does not claim position feedback")
	}
}

func waitRealReady(t *testing.T, ctx context.Context, backend *Backend) simulation.Status {
	t.Helper()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		status, err := backend.Status(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if status.ReadyForControl() {
			return status
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			t.Fatalf("Jetty backend not ready: %+v", status)
		}
	}
}

func containsModel(models []simulation.Model, name string) bool {
	for _, model := range models {
		if model.Name == name {
			return true
		}
	}
	return false
}
