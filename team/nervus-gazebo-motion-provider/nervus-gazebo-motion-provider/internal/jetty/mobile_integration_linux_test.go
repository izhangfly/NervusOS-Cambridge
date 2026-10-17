//go:build linux && cgo && jettyintegration

package jetty

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nervus-os/nervus-gazebo-provider/internal/contract"
	"github.com/nervus-os/nervus-gazebo-provider/internal/gztransport"
	"github.com/nervus-os/nervus-gazebo-provider/internal/motionprofile"
	"github.com/nervus-os/nervus-gazebo-provider/internal/simulation"
)

func TestRealJettyMobileG1TrajectoryCancelAndHome(t *testing.T) {
	variantRoot, err := filepath.Abs("../../../mobile-g1-dex3")
	if err != nil {
		t.Fatal(err)
	}
	profiles, err := motionprofile.LoadBundle(
		filepath.Join(variantRoot, "profile"),
		"mobile-g1-dex3-v1.json",
		contract.MotionProfileAllowlist(),
	)
	if err != nil {
		t.Fatalf("load Mobile G1 Profile: %v", err)
	}
	runtime, err := motionprofile.LoadRuntime(
		filepath.Join(variantRoot, "profile"),
		"runtime-mobile-g1-dex3-v1.json",
		profiles,
	)
	if err != nil {
		t.Fatalf("load Mobile G1 runtime: %v", err)
	}
	transportConfig := gztransport.DefaultConfig()
	transportConfig.Partition = os.Getenv("GZ_PARTITION")
	client, err := gztransport.Open(transportConfig)
	if err != nil {
		t.Fatalf("open Gazebo Transport: %v", err)
	}
	config := DefaultConfig()
	config.World = "nervus_mobile_g1_dex3"
	config.Profiles = profiles
	config.Runtime = runtime
	config.DiscoveryTimeout = 30 * time.Second
	backend, err := New(config, client, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		_ = client.Close()
		t.Fatalf("create backend: %v", err)
	}
	defer func() {
		if err := backend.Close(); err != nil {
			t.Errorf("close backend: %v", err)
		}
	}()

	readyCtx, readyCancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer readyCancel()
	status := waitRealReady(t, readyCtx, backend)
	waitRealMotionBinding(t, readyCtx, backend, modelBinding{
		model: "nervus_mobile_g1_dex3", capabilityRole: "compatibility.joints",
	})
	waitRealMotionBinding(t, readyCtx, backend, modelBinding{
		model: "nervus_mobile_g1_dex3", capabilityRole: "base.motion",
	})
	waitRealJointFeedback(t, readyCtx, backend, status.WorldGeneration)
	readyCancel()

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	execution := simulation.ControlContext{
		WorldGeneration:    status.WorldGeneration,
		ResourceHandle:     contract.WorldResourceRole,
		ResourceGeneration: 1,
		LeaseID:            1,
		MotionEpoch:        1,
		DeadlineNanos:      math.MaxInt64,
		CommandSequence:    1,
		Controller:         simulation.ControllerHuman,
	}
	model := "nervus_mobile_g1_dex3"
	probe := []float64{0.15, 0.10, 0, 0.20, 0, 0.08, 0}
	started, err := backend.StartJointTrajectory(ctx, execution, model, "left_arm", []simulation.JointTrajectoryPoint{{
		Positions: probe, TimeFromStart: int64(time.Second),
	}})
	if err != nil {
		t.Fatalf("start left-arm trajectory: %v", err)
	}
	completed, err := backend.WaitJointMotion(ctx, started.MotionID)
	if err != nil {
		t.Fatalf("wait left-arm trajectory: %v", err)
	}
	if !completed.Converged || completed.ControlledJoints != 7 {
		t.Fatalf("left-arm completion = %+v", completed)
	}

	started, err = backend.StartJointTrajectory(ctx, execution, model, "left_arm", []simulation.JointTrajectoryPoint{{
		Positions: []float64{-0.15, 0, 0, 0, 0, 0, 0}, TimeFromStart: int64(2 * time.Second),
	}})
	if err != nil {
		t.Fatalf("start cancellable trajectory: %v", err)
	}
	time.Sleep(200 * time.Millisecond)
	if _, err := backend.CancelJointMotion(ctx, execution, model, "left_arm"); err != nil {
		t.Fatalf("cancel left-arm trajectory: %v", err)
	}
	if cancelled, err := backend.WaitJointMotion(ctx, started.MotionID); !errors.Is(err, context.Canceled) || cancelled.Converged {
		t.Fatalf("cancelled trajectory = %+v, %v", cancelled, err)
	}

	started, err = backend.StartJointHome(ctx, execution, model, "left_arm", "home")
	if err != nil {
		t.Fatalf("start left-arm Home: %v", err)
	}
	completed, err = backend.WaitJointMotion(ctx, started.MotionID)
	if err != nil || !completed.Converged {
		t.Fatalf("left-arm Home = %+v, %v", completed, err)
	}
	if _, err := backend.StopAllMotion(ctx, execution); err != nil {
		t.Fatalf("StopAllMotion: %v", err)
	}
}

func waitRealJointFeedback(t *testing.T, ctx context.Context, backend *Backend, generation uint64) {
	t.Helper()
	_, err := backend.JointState(ctx, simulation.JointQuery{
		WorldGeneration: generation,
		Model:           "nervus_mobile_g1_dex3",
		Joint:           "left_shoulder_pitch_joint",
		Axis:            0,
	})
	if err != nil {
		t.Fatalf("initial Mobile G1 joint feedback unavailable: %v", err)
	}
}

type modelBinding struct {
	model          string
	capabilityRole string
}

func waitRealMotionBinding(t *testing.T, ctx context.Context, backend *Backend, want modelBinding) {
	t.Helper()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	var last []simulation.MotionBinding
	for {
		bindings, err := backend.MotionBindings(ctx)
		if err != nil {
			t.Fatal(err)
		}
		last = bindings
		for _, binding := range bindings {
			if binding.Model == want.model && binding.CapabilityRole == want.capabilityRole &&
				binding.State == simulation.MotionBindingReady {
				return
			}
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			diagnosticCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			topics, listErr := backend.client.ListTopics(diagnosticCtx)
			motionTopics := make([]string, 0)
			for _, topic := range topics {
				if strings.Contains(topic, "/cmd_vel") || strings.Contains(topic, "/cmd_pos") {
					motionTopics = append(motionTopics, topic)
				}
			}
			jointInfo, jointErr := backend.client.TopicInfo(
				diagnosticCtx,
				"/model/nervus_mobile_g1_dex3/joint/left_shoulder_pitch_joint/0/cmd_pos",
			)
			baseInfo, baseErr := backend.client.TopicInfo(
				diagnosticCtx,
				"/model/nervus_mobile_g1_dex3/cmd_vel",
			)
			t.Fatalf(
				"motion binding not ready: want=%+v bindings=%+v motion_topics=%v list_err=%v joint_info=%+v joint_err=%v base_info=%+v base_err=%v",
				want, last, motionTopics, listErr, jointInfo, jointErr, baseInfo, baseErr,
			)
		}
	}
}
