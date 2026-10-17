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

func TestRealJettyG1MinimumClosedLoop(t *testing.T) {
	transportConfig := gztransport.DefaultConfig()
	client, err := gztransport.Open(transportConfig)
	if err != nil {
		t.Fatalf("open Gazebo Transport: %v", err)
	}
	config := DefaultConfig()
	profiles, err := motionprofile.LoadBundle(
		"../../profiles", "g1-fixed-v1.json", contract.MotionProfileAllowlist(),
	)
	if err != nil {
		t.Fatalf("load fixed G1 Profile: %v", err)
	}
	runtime, err := motionprofile.LoadRuntime(
		"../../profiles", "runtime-g1-fixed-v1.json", profiles,
	)
	if err != nil {
		t.Fatalf("load fixed G1 runtime configuration: %v", err)
	}
	config.Profiles = profiles
	config.Runtime = runtime
	config.World = runtime.GetWorld()
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
	if !containsModel(models.Models, "nervus_g1") {
		t.Fatalf("nervus_g1 missing from %d models", len(models.Models))
	}
	joints, err := backend.ListJoints(ctx, "nervus_g1", status.WorldGeneration)
	if err != nil {
		t.Fatalf("ListJoints: %v", err)
	}
	controllable := 0
	for _, joint := range joints.Joints {
		if joint.Controllable {
			controllable++
		}
	}
	if controllable != 29 {
		t.Fatalf("controllable joints = %d, want 29", controllable)
	}
	armPose := []simulation.JointCommand{
		{Model: "nervus_g1", Joint: "left_shoulder_pitch_joint", Axis: 0, Position: 0.35},
		{Model: "nervus_g1", Joint: "left_shoulder_roll_joint", Axis: 0, Position: 0.20},
		{Model: "nervus_g1", Joint: "left_shoulder_yaw_joint", Axis: 0, Position: -0.15},
		{Model: "nervus_g1", Joint: "left_elbow_joint", Axis: 0, Position: 0.60},
		{Model: "nervus_g1", Joint: "left_wrist_roll_joint", Axis: 0, Position: 0.10},
		{Model: "nervus_g1", Joint: "left_wrist_pitch_joint", Axis: 0, Position: -0.20},
		{Model: "nervus_g1", Joint: "left_wrist_yaw_joint", Axis: 0, Position: 0.15},
		{Model: "nervus_g1", Joint: "right_shoulder_pitch_joint", Axis: 0, Position: 0.35},
		{Model: "nervus_g1", Joint: "right_shoulder_roll_joint", Axis: 0, Position: -0.20},
		{Model: "nervus_g1", Joint: "right_shoulder_yaw_joint", Axis: 0, Position: 0.15},
		{Model: "nervus_g1", Joint: "right_elbow_joint", Axis: 0, Position: 0.60},
		{Model: "nervus_g1", Joint: "right_wrist_roll_joint", Axis: 0, Position: -0.10},
		{Model: "nervus_g1", Joint: "right_wrist_pitch_joint", Axis: 0, Position: -0.20},
		{Model: "nervus_g1", Joint: "right_wrist_yaw_joint", Axis: 0, Position: -0.15},
	}

	defer func() {
		restoreCtx, restoreCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer restoreCancel()
		execution.CommandSequence++
		restore := append([]simulation.JointCommand(nil), armPose...)
		for index := range restore {
			restore[index].Position = 0
		}
		_, _ = backend.SetJointPositions(restoreCtx, execution, restore)
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
	receipt, err := backend.SetJointPositions(ctx, execution, armPose)
	if err != nil {
		t.Fatalf("SetJointPositions dual-arm pose: %v", err)
	}
	if receipt.SubmittedCommands != uint32(len(armPose)) {
		t.Fatalf("submitted commands = %d, want %d", receipt.SubmittedCommands, len(armPose))
	}
	if !receipt.FeedbackAvailable {
		t.Fatal("G1 command receipt did not report required joint-state feedback")
	}
	if _, err := backend.Resume(ctx, execution); err != nil {
		t.Fatalf("Resume for G1 motion: %v", err)
	}
	motionCtx, motionCancel := context.WithTimeout(ctx, 15*time.Second)
	defer motionCancel()
	waitJointPositions(t, motionCtx, backend, status.WorldGeneration, armPose, 0.01)
}

func waitJointPositions(
	t *testing.T,
	ctx context.Context,
	backend *Backend,
	generation uint64,
	commands []simulation.JointCommand,
	tolerance float64,
) {
	t.Helper()
	const settleFor = 250 * time.Millisecond
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	last := make(map[string]simulation.JointState, len(commands))
	var withinToleranceSince time.Time
	for {
		converged := true
		for _, command := range commands {
			state, err := backend.JointState(ctx, simulation.JointQuery{
				WorldGeneration: generation,
				Model:           command.Model, Joint: command.Joint, Axis: command.Axis,
			})
			if err == nil {
				last[command.Joint] = state
			}
			if err != nil || !state.HasPosition || math.Abs(state.Position-command.Position) > tolerance {
				converged = false
			}
		}
		if converged {
			if withinToleranceSince.IsZero() {
				withinToleranceSince = time.Now()
			}
			if time.Since(withinToleranceSince) >= settleFor {
				return
			}
		} else {
			withinToleranceSince = time.Time{}
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			for _, command := range commands {
				state := last[command.Joint]
				t.Logf("joint %s target=%.4f actual=%.4f error=%.4f",
					command.Joint, command.Position, state.Position,
					math.Abs(state.Position-command.Position))
			}
			t.Fatalf("dual-arm pose did not settle for %s within %s and %.4f rad",
				settleFor, 15*time.Second, tolerance)
		}
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
